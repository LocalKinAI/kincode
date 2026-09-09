package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/LocalKinAI/kincode/pkg/provider"
)

// GitTool is read-only git: what changed, what's dirty, what happened
// before.
//
// A coding agent without git is working blind. It cannot see what it
// just changed, which branch it is on, whether the file it is about to
// rewrite has uncommitted work in it, or how this codebase names
// things — all of which live in `git diff` and `git log` and nowhere
// else. kincode could already reach git through bash, but two things
// were missing. Plan mode denies bash outright, so while planning —
// exactly when you want to read the diff — it could not. And a raw
// `git diff` on a real change is thousands of lines that arrive in one
// piece and cost the model its context; here it arrives as a summary
// first, with a path to zoom in on.
//
// Read-only on purpose. Committing, pushing and resetting are the
// user's acts; the agent can still ask for them through bash, where
// the approval gate can put the question to a human.
type GitTool struct{}

func (g *GitTool) Name() string { return "git" }

func (g *GitTool) Description() string {
	return "Read the repository: what has changed, what is uncommitted, and what happened before. " +
		"action=status (branch + changed files), diff (your changes — summary first, pass path to see one file's patch), " +
		"log (recent commits), show (one commit), blame (who last touched each line of a file). " +
		"Read-only: to commit or push, use bash — those go past the user."
}

func (g *GitTool) Def() provider.ToolDef {
	return provider.NewToolDef("git", g.Description(), map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"status", "diff", "log", "show", "blame"},
				"description": "What to read.",
			},
			"path": map[string]any{
				"type": "string",
				"description": "For diff: one file, to get its full patch instead of the summary. " +
					"For blame: the file (required). For log: limit history to this path.",
			},
			"ref": map[string]any{
				"type": "string",
				"description": "For diff: what to compare against (default: the working tree vs HEAD; " +
					"'HEAD~3' or a branch name works). For show: the commit.",
			},
			"staged": map[string]any{
				"type":        "boolean",
				"description": "For diff: show what is staged rather than the working tree.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "For log: how many commits (default 15, max 100).",
			},
		},
		"required": []string{"action"},
	})
}

// gitTimeout is generous for blame on a large file and still short
// enough that a hung git (a lock, a prompt for credentials) does not
// hold the turn.
const gitTimeout = 30 * time.Second

// diffBudget is how much patch text goes back in one call. Past this
// the model is told to name a path — a whole refactor's diff is not
// something it can hold, or needs to.
const diffBudget = 12000

func (g *GitTool) Execute(args map[string]any) (string, error) {
	action, _ := args["action"].(string)
	path, _ := args["path"].(string)
	ref, _ := args["ref"].(string)
	staged, _ := args["staged"].(bool)

	if !insideRepo() {
		return "", fmt.Errorf("not a git repository (or git is not installed)")
	}

	switch action {
	case "status":
		return gitStatus()
	case "diff":
		return gitDiff(path, ref, staged)
	case "log":
		limit := 15
		if l, ok := args["limit"].(float64); ok && l > 0 {
			limit = min(int(l), 100)
		}
		return gitLog(limit, path)
	case "show":
		if ref == "" {
			ref = "HEAD"
		}
		return run("show", "--stat", "--patch", "--no-color", ref)
	case "blame":
		if path == "" {
			return "", fmt.Errorf("blame needs a path")
		}
		out, err := run("blame", "--date=short", "-w", "--", path)
		if err != nil {
			return out, err
		}
		return cap(out, diffBudget, "blame"), nil
	default:
		return "", fmt.Errorf("unknown action %q — use status, diff, log, show or blame", action)
	}
}

// gitStatus answers "where am I and what have I touched" in the few
// lines that question deserves.
func gitStatus() (string, error) {
	branch, _ := run("rev-parse", "--abbrev-ref", "HEAD")
	porcelain, err := run("status", "--porcelain=v1", "--branch")
	if err != nil {
		return porcelain, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "branch: %s\n", strings.TrimSpace(branch))
	lines := strings.Split(strings.TrimSpace(porcelain), "\n")
	changed := 0
	for _, l := range lines {
		if l == "" || strings.HasPrefix(l, "##") {
			continue
		}
		changed++
		if changed <= 50 {
			sb.WriteString(l + "\n")
		}
	}
	switch {
	case changed == 0:
		sb.WriteString("clean — nothing uncommitted\n")
	case changed > 50:
		fmt.Fprintf(&sb, "… and %d more\n", changed-50)
	}
	return sb.String(), nil
}

// gitDiff gives the shape of a change before its contents: a real
// refactor's patch is thousands of lines, and a model that asked "what
// did I change" wants the answer, not the bytes.
func gitDiff(path, ref string, staged bool) (string, error) {
	base := []string{"diff", "--no-color"}
	if staged {
		base = append(base, "--staged")
	}
	if ref != "" {
		base = append(base, ref)
	}
	if path != "" {
		out, err := run(append(append([]string{}, base...), "--", path)...)
		if err != nil {
			return out, err
		}
		if strings.TrimSpace(out) == "" {
			return fmt.Sprintf("no changes in %s\n", path), nil
		}
		return cap(out, diffBudget, "diff"), nil
	}

	stat, err := run(append(append([]string{}, base...), "--stat")...)
	if err != nil {
		return stat, err
	}
	if strings.TrimSpace(stat) == "" {
		return "no changes\n", nil
	}
	full, err := run(base...)
	if err != nil {
		return stat, nil // the summary is still worth having
	}
	if len(full) <= diffBudget {
		return full, nil
	}
	return stat + fmt.Sprintf(
		"\n(the full patch is %d bytes — too much to read at once. "+
			"Call git again with path=<one file> for that file's patch.)\n", len(full)), nil
}

func gitLog(limit int, path string) (string, error) {
	argv := []string{"log", "--no-color", fmt.Sprintf("-%d", limit),
		"--pretty=format:%h %ad %an — %s", "--date=short"}
	if path != "" {
		argv = append(argv, "--", path)
	}
	return run(argv...)
}

func insideRepo() bool {
	out, err := run("rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// run executes git in the process's working directory — which is the
// repo the user picked, since the server chdirs into it.
func run(argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", argv...)
	// git must never stop to ask for a password or open a pager: both
	// would hang a turn with nothing on screen to explain it.
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "PAGER=cat")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w", strings.Join(argv, " "), err)
	}
	return string(out), nil
}

// cap truncates at a line boundary and says so, so the model knows it
// is looking at part of something rather than all of it.
func cap(s string, budget int, what string) string {
	if len(s) <= budget {
		return s
	}
	cut := s[:budget]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + fmt.Sprintf("\n… (%s truncated at %d bytes; narrow it with path=)\n", what, budget)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
