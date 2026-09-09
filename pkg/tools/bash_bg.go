package tools

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/LocalKinAI/kincode/pkg/provider"
)

// Long-running commands, and reading them while they run.
//
// bash was one call, one process, capped at 300 seconds. That is the
// right shape for `go test ./...` and the wrong shape for everything a
// developer actually leaves running: a dev server, a watcher, a build
// of something large, a test suite that takes ten minutes. The agent
// either could not start them or started them and got a timeout error
// for its trouble, with the process killed underneath.
//
// Background tasks outlive the call that starts them. Output is
// captured as it arrives and read back in pieces, so the agent can
// start a server, do something else, and come back to check whether it
// came up — which is the loop it needs for anything with a startup
// time.

// bgTask is one running (or finished) background command.
type bgTask struct {
	id      string
	command string
	cmd     *exec.Cmd
	started time.Time

	mu       sync.Mutex
	buf      bytes.Buffer
	readTo   int // how much of buf the agent has already seen
	done     bool
	exitCode int
	err      error
}

// Write captures output as the process produces it. Bounded: a watcher
// left running overnight must not become a memory leak, and the tail
// is the part anyone wants.
func (t *bgTask) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > maxOutputSize*2 {
		keep := t.buf.Bytes()[t.buf.Len()-maxOutputSize:]
		fresh := bytes.NewBuffer(append([]byte("… (earlier output dropped)\n"), keep...))
		t.readTo = 0
		t.buf = *fresh
	}
	return len(p), nil
}

// bgTasks holds the running tasks for this process.
type bgTasks struct {
	mu    sync.Mutex
	tasks map[string]*bgTask
	seq   int
}

var background = &bgTasks{tasks: map[string]*bgTask{}}

// start launches a command detached from the call that asked for it.
func (b *bgTasks) start(command, dir string) (*bgTask, error) {
	cmd := exec.Command("bash", "-c", command)
	cmd.Dir = dir
	// Its own process group, so killing the task kills what it spawned
	// — a dev server that forks a child would otherwise survive and
	// keep the port.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	b.mu.Lock()
	b.seq++
	t := &bgTask{id: fmt.Sprintf("task-%d", b.seq), command: command, cmd: cmd, started: time.Now()}
	b.tasks[t.id] = t
	b.mu.Unlock()

	cmd.Stdout = t
	cmd.Stderr = t
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		err := cmd.Wait()
		t.mu.Lock()
		t.done, t.err = true, err
		if cmd.ProcessState != nil {
			t.exitCode = cmd.ProcessState.ExitCode()
		}
		t.mu.Unlock()
	}()
	return t, nil
}

func (b *bgTasks) get(id string) *bgTask {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tasks[id]
}

func (b *bgTasks) list() []*bgTask {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*bgTask, 0, len(b.tasks))
	for _, t := range b.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// KillAll stops every background task. Called when the session ends or
// the user switches repos: a dev server for the previous project has
// no business still holding a port.
func KillAll() {
	background.mu.Lock()
	tasks := make([]*bgTask, 0, len(background.tasks))
	for _, t := range background.tasks {
		tasks = append(tasks, t)
	}
	background.tasks = map[string]*bgTask{}
	background.mu.Unlock()
	for _, t := range tasks {
		t.kill()
	}
}

func (t *bgTask) kill() {
	t.mu.Lock()
	done := t.done
	t.mu.Unlock()
	if done || t.cmd.Process == nil {
		return
	}
	// The whole group, not just the shell: `bash -c "npm run dev"` is a
	// shell whose child holds the port.
	_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGTERM)
	go func() {
		time.Sleep(3 * time.Second)
		t.mu.Lock()
		stillRunning := !t.done
		t.mu.Unlock()
		if stillRunning && t.cmd.Process != nil {
			_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
}

// read returns what has arrived since the last read.
func (t *bgTask) read() (chunk string, done bool, exit int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	all := t.buf.String()
	if t.readTo > len(all) {
		t.readTo = 0
	}
	chunk = all[t.readTo:]
	t.readTo = len(all)
	return chunk, t.done, t.exitCode
}

func (t *bgTask) status() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.done {
		return fmt.Sprintf("running for %s", time.Since(t.started).Round(time.Second))
	}
	if t.exitCode == 0 {
		return "finished ok"
	}
	return fmt.Sprintf("exited %d", t.exitCode)
}

// BashOutputTool reads (and stops) background tasks.
type BashOutputTool struct{}

func (b *BashOutputTool) Name() string { return "bash_output" }

func (b *BashOutputTool) Description() string {
	return "Read new output from a background bash task (one started with background=true). " +
		"Returns whatever has arrived since you last read it, so calling it repeatedly follows the log. " +
		"Pass kill=true to stop the task. Call with no task to list what is running."
}

func (b *BashOutputTool) Def() provider.ToolDef {
	return provider.NewToolDef("bash_output", b.Description(), map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "Task id, e.g. 'task-1'. Omit to list all tasks.",
			},
			"kill": map[string]any{
				"type":        "boolean",
				"description": "Stop the task (and anything it spawned).",
			},
		},
	})
}

func (b *BashOutputTool) Execute(args map[string]any) (string, error) {
	id, _ := args["task"].(string)
	kill, _ := args["kill"].(bool)

	if id == "" {
		tasks := background.list()
		if len(tasks) == 0 {
			return "no background tasks", nil
		}
		var sb strings.Builder
		for _, t := range tasks {
			fmt.Fprintf(&sb, "%s  %s  %s\n", t.id, t.status(), firstLine(t.command))
		}
		return sb.String(), nil
	}

	t := background.get(id)
	if t == nil {
		return "", fmt.Errorf("no task %q — call bash_output with no task to see what is running", id)
	}
	if kill {
		t.kill()
		return fmt.Sprintf("%s stopped", id), nil
	}

	chunk, done, exit := t.read()
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%s)\n", id, t.status())
	if chunk == "" {
		if done {
			fmt.Fprintf(&sb, "no new output; the task finished with exit code %d\n", exit)
		} else {
			sb.WriteString("no new output since the last read; it is still running\n")
		}
		return sb.String(), nil
	}
	if len(chunk) > maxOutputSize {
		chunk = chunk[len(chunk)-maxOutputSize:]
		sb.WriteString("… (showing the tail)\n")
	}
	sb.WriteString(chunk)
	return sb.String(), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 70 {
		s = s[:70] + "…"
	}
	return s
}
