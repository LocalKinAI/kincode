package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runBash(t *testing.T, b *BashTool, args map[string]any) string {
	t.Helper()
	out, err := b.Execute(args)
	if err != nil {
		t.Fatalf("bash %v: %v\n%s", args, err, out)
	}
	return out
}

func TestCdCarriesIntoTheNextCall(t *testing.T) {
	// The tax this removes: every call used to start at the repo root,
	// so the agent had to repeat the path forever or re-cd each time.
	dir := t.TempDir()
	sub := filepath.Join(dir, "cmd", "thing")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	b := &BashTool{}
	runBash(t, b, map[string]any{"command": "cd " + dir})
	runBash(t, b, map[string]any{"command": "cd cmd/thing"})

	got := strings.TrimSpace(runBash(t, b, map[string]any{"command": "pwd"}))
	// macOS temp dirs are symlinked through /private.
	if !strings.HasSuffix(got, filepath.Join("cmd", "thing")) {
		t.Errorf("pwd = %q, want to still be in cmd/thing", got)
	}
}

func TestCdSticksEvenWhenTheCommandAfterItFails(t *testing.T) {
	// `cd somewhere && something-that-fails` still moved you, and the
	// next call should see that.
	dir := t.TempDir()
	b := &BashTool{}
	runBash(t, b, map[string]any{"command": "cd " + dir})
	if _, err := b.Execute(map[string]any{"command": "false"}); err == nil {
		t.Fatal("`false` should report a non-zero exit")
	}
	got := strings.TrimSpace(runBash(t, b, map[string]any{"command": "pwd"}))
	if !strings.HasSuffix(got, filepath.Base(dir)) {
		t.Errorf("pwd = %q, want %q", got, dir)
	}
}

func TestResetDirForgetsIt(t *testing.T) {
	dir := t.TempDir()
	b := &BashTool{}
	runBash(t, b, map[string]any{"command": "cd " + dir})
	if b.Dir() == "" {
		t.Fatal("cd should have been recorded")
	}
	b.ResetDir()
	if b.Dir() != "" {
		t.Error("a repo switch should forget the old project's directory")
	}
}

func TestPwdPlumbingLeavesNoTraceInTheOutput(t *testing.T) {
	// The directory is carried through a temp file precisely so the
	// caller never has to read past it.
	b := &BashTool{}
	got := runBash(t, b, map[string]any{"command": "echo hello"})
	if strings.TrimSpace(got) != "hello" {
		t.Errorf("output = %q, want just the command's own output", got)
	}
}

func TestBackgroundReturnsImmediatelyAndKeepsRunning(t *testing.T) {
	defer KillAll()
	b := &BashTool{}
	start := time.Now()
	out := runBash(t, b, map[string]any{
		"command":    "echo first; sleep 30; echo never",
		"background": true,
	})
	if time.Since(start) > 3*time.Second {
		t.Fatalf("background start blocked for %v", time.Since(start))
	}
	id := taskID(t, out)

	// Give it a moment to produce its first line.
	var chunk string
	for i := 0; i < 40; i++ {
		chunk = runBash2(t, &BashOutputTool{}, map[string]any{"task": id})
		if strings.Contains(chunk, "first") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(chunk, "first") {
		t.Fatalf("never saw the task's output:\n%s", chunk)
	}
	if !strings.Contains(chunk, "running") {
		t.Errorf("status should say it is still running:\n%s", chunk)
	}

	// Reading again returns only what is new — following a log, not
	// re-reading it.
	again := runBash2(t, &BashOutputTool{}, map[string]any{"task": id})
	if strings.Contains(again, "first") {
		t.Errorf("second read repeated old output:\n%s", again)
	}

	if out := runBash2(t, &BashOutputTool{}, map[string]any{"task": id, "kill": true}); !strings.Contains(out, "stopped") {
		t.Errorf("kill said %q", out)
	}
}

func TestListingTasksAndUnknownIds(t *testing.T) {
	defer KillAll()
	b := &BashTool{}
	out := runBash(t, b, map[string]any{"command": "sleep 20", "background": true})
	id := taskID(t, out)

	listed := runBash2(t, &BashOutputTool{}, map[string]any{})
	if !strings.Contains(listed, id) || !strings.Contains(listed, "sleep 20") {
		t.Errorf("listing should name the task and its command:\n%s", listed)
	}

	_, err := (&BashOutputTool{}).Execute(map[string]any{"task": "task-999"})
	if err == nil || !strings.Contains(err.Error(), "no task") {
		t.Errorf("unknown id: %v", err)
	}
}

func TestFinishedTaskReportsItsExitCode(t *testing.T) {
	defer KillAll()
	b := &BashTool{}
	out := runBash(t, b, map[string]any{"command": "exit 3", "background": true})
	id := taskID(t, out)
	var got string
	for i := 0; i < 40; i++ {
		got = runBash2(t, &BashOutputTool{}, map[string]any{"task": id})
		if strings.Contains(got, "exited") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(got, "exited 3") {
		t.Errorf("a finished task should report how it ended:\n%s", got)
	}
}

func taskID(t *testing.T, out string) string {
	t.Helper()
	i := strings.Index(out, "task-")
	if i < 0 {
		t.Fatalf("no task id in %q", out)
	}
	rest := out[i:]
	end := strings.IndexAny(rest, " \n:")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

func runBash2(t *testing.T, b *BashOutputTool, args map[string]any) string {
	t.Helper()
	out, err := b.Execute(args)
	if err != nil {
		t.Fatalf("bash_output %v: %v", args, err)
	}
	return out
}
