package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// RepoContext is the paragraph that tells the agent where it is before
// it is asked anything.
//
// Without it every session opens the same way: the model runs pwd, ls,
// git status and git log to work out what it is looking at — four
// rounds and a few thousand tokens to learn something the harness
// already knows. Worse, it often does not bother, and edits a file
// while three of its neighbours have uncommitted work in them.
//
// Deliberately small — a branch, a count, five subject lines. It is
// orientation, not a briefing; anything more is what the `git` tool is
// for, on demand.
func RepoContext(dir string) string {
	git := func(argv ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", argv...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat")
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	if git("rev-parse", "--is-inside-work-tree") != "true" {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n\n## This repository\n\n")
	if b := git("rev-parse", "--abbrev-ref", "HEAD"); b != "" {
		fmt.Fprintf(&sb, "On branch `%s`", b)
		if up := git("rev-parse", "--abbrev-ref", "@{upstream}"); up != "" {
			if ab := git("rev-list", "--left-right", "--count", "@{upstream}...HEAD"); ab != "" {
				f := strings.Fields(ab)
				if len(f) == 2 && (f[0] != "0" || f[1] != "0") {
					fmt.Fprintf(&sb, " (%s ahead, %s behind `%s`)", f[1], f[0], up)
				}
			}
		}
		sb.WriteString(".\n")
	}

	// Uncommitted work is the thing worth knowing before touching
	// anything: an agent that rewrites a file someone was halfway
	// through editing destroys work that was never committed.
	if st := git("status", "--porcelain=v1"); st != "" {
		lines := strings.Split(st, "\n")
		fmt.Fprintf(&sb, "\n%d file(s) uncommitted", len(lines))
		if len(lines) <= 12 {
			sb.WriteString(":\n")
			for _, l := range lines {
				sb.WriteString("  " + l + "\n")
			}
		} else {
			sb.WriteString(" — run `git status` to see them.\n")
		}
		sb.WriteString("\nSome of this is the user's work in progress, not yours. " +
			"Read a file before you rewrite it, and do not revert changes you did not make.\n")
	} else {
		sb.WriteString("\nWorking tree is clean.\n")
	}

	// Recent subjects teach the house style — how this project words a
	// commit, how big one usually is — for free.
	if lg := git("log", "-5", "--pretty=format:%s", "--no-color"); lg != "" {
		sb.WriteString("\nRecent commits:\n")
		for _, l := range strings.Split(lg, "\n") {
			sb.WriteString("  " + l + "\n")
		}
	}
	return sb.String()
}
