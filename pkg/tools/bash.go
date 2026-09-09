package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/LocalKinAI/kincode/pkg/provider"
)

const (
	bashTimeout   = 30 * time.Second
	maxOutputSize = 128 * 1024 // 128KB
)

// BashTool executes shell commands.
//
// Two things it does that a bare exec does not.
//
// `cd` sticks. Every call used to be its own process from the repo
// root, so `cd cmd/kincode` did nothing to the call after it and the
// agent had to repeat the path or re-cd every single time — a
// permanent small tax, and a source of "file not found" on commands
// that looked right. The directory the command ends in is carried into
// the next one, the way it would be in a terminal.
//
// (Environment does not persist — an `export` is gone with the shell
// that ran it. Keeping it would mean holding a real shell process open
// and parsing where one command's output stops, which is a much larger
// and more fragile machine than the problem justifies.)
//
// And a command can be left running. See bash_bg.go.
type BashTool struct {
	mu  sync.Mutex
	cwd string // "" until the first cd; the process's own directory
}

func (b *BashTool) Name() string { return "bash" }

func (b *BashTool) Description() string {
	return "Execute a bash command and return its output. Timeout 30s (max 300), output capped at 128KB. " +
		"`cd` carries over to your next bash call, like a real terminal. " +
		"For something long-running — a dev server, a watcher, a slow build — pass background=true: " +
		"it returns a task id straight away and keeps running, and bash_output reads its log as it goes."
}

// Dir is the directory the next command will run in.
func (b *BashTool) Dir() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cwd
}

// ResetDir forgets the accumulated cd — called when the user switches
// repos, since a path inside the old project is worse than no path.
func (b *BashTool) ResetDir() {
	b.mu.Lock()
	b.cwd = ""
	b.mu.Unlock()
}

func (b *BashTool) Def() provider.ToolDef {
	return provider.NewToolDef("bash", b.Description(), map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The bash command to execute",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": "Timeout in seconds (default 30, max 300). Ignored when background=true.",
			},
			"background": map[string]any{
				"type": "boolean",
				"description": "Run it without waiting. Returns a task id immediately; read its output " +
					"with bash_output. Use this for anything that does not finish on its own — a server, " +
					"a watcher — or takes longer than a couple of minutes.",
			},
		},
		"required": []string{"command"},
	})
}

// blockedCommands are patterns that should never be executed.
var blockedCommands = []string{
	"rm -rf /",
	"rm -rf /*",
	"sudo rm -rf",
	"mkfs.",
	":(){:|:&};:",
	"> /dev/sda",
	"dd if=/dev/zero of=/dev/sda",
	"chmod -R 777 /",
}

func (b *BashTool) Execute(args map[string]any) (string, error) {
	command, ok := args["command"].(string)
	if !ok || command == "" {
		return "", fmt.Errorf("command is required")
	}

	// Check blocklist.
	cmdLower := strings.ToLower(strings.TrimSpace(command))
	for _, blocked := range blockedCommands {
		if strings.Contains(cmdLower, strings.ToLower(blocked)) {
			return "", fmt.Errorf("blocked: command matches dangerous pattern %q", blocked)
		}
	}

	b.mu.Lock()
	dir := b.cwd
	b.mu.Unlock()

	if bg, _ := args["background"].(bool); bg {
		t, err := background.start(command, dir)
		if err != nil {
			return "", fmt.Errorf("could not start: %w", err)
		}
		return fmt.Sprintf("started %s in the background: %s\n"+
			"It keeps running after this call. Read its output with "+
			"bash_output(task=%q), and stop it with bash_output(task=%q, kill=true).",
			t.id, firstLine(command), t.id, t.id), nil
	}

	timeout := bashTimeout
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
		if timeout > 300*time.Second {
			timeout = 300 * time.Second
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Where the command leaves the shell is written to a temp file
	// rather than printed, so carrying `cd` forward costs the caller no
	// output it has to read past.
	pwdFile, ferr := os.CreateTemp("", "kincode-cwd-*")
	if ferr != nil {
		return "", fmt.Errorf("could not prepare the shell: %w", ferr)
	}
	pwdPath := pwdFile.Name()
	_ = pwdFile.Close()
	defer os.Remove(pwdPath)

	script := command + "\n__kincode_rc=$?\npwd > " + shellQuote(pwdPath) + " 2>/dev/null\nexit $__kincode_rc\n"

	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	// Remember where it ended up, whether or not the command
	// succeeded: `cd somewhere && a-failing-thing` still moved you.
	if data, rerr := os.ReadFile(pwdPath); rerr == nil {
		if ended := strings.TrimSpace(string(data)); ended != "" && ended != dir {
			if info, serr := os.Stat(ended); serr == nil && info.IsDir() {
				b.mu.Lock()
				b.cwd = ended
				b.mu.Unlock()
			}
		}
	}

	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += "STDERR:\n" + stderr.String()
	}

	// Cap output size.
	if len(output) > maxOutputSize {
		output = output[:maxOutputSize] + "\n... (output truncated at 128KB)"
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return output, fmt.Errorf("command timed out after %v", timeout)
		}
		return output, fmt.Errorf("exit code %d", cmd.ProcessState.ExitCode())
	}

	return output, nil
}

// shellQuote makes a path safe to paste into a script — temp dirs on
// macOS live under /var/folders/… with characters that are fine, but a
// user's TMPDIR is theirs to set.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
