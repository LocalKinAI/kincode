// Package verify runs a project's cheapest correctness check after the
// agent changes a file, and hands the result back to the model.
//
// The persona already said to do this — "Test your changes — Run the
// build, run the tests" — but a rule in a prompt is a wish. What makes
// a coding agent trustworthy is the loop being mechanical: edit, see
// the compiler's answer, fix, repeat, and only then claim to be done.
// Left to the model, a small one edits three files, says "done", and
// leaves a build that has not compiled since the second edit.
//
// The check is deliberately the build, not the test suite. A build or
// typecheck catches the mistakes an agent actually makes — a renamed
// symbol used in one more place, a wrong signature, an unclosed brace —
// runs in seconds, and has no side effects. Tests are slower, often
// need fixtures or a network, and are the user's call to run.
package verify

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Checker is one project type: how to recognise it and what to run.
type Checker struct {
	// Name is what the user sees in the log line.
	Name string
	// Marker is the file at the repo root that identifies the project.
	Marker string
	// Cmd is argv. Chosen for speed and for having no side effects:
	// nothing here writes outside a build cache.
	Cmd []string
	// Exts limits the check to edits that could plausibly break it, so
	// touching README.md does not trigger a compile.
	Exts []string
}

// checkers are tried in order; the first whose marker exists at the
// root wins. Go and Rust first because their compilers are the ones
// that actually catch things.
var checkers = []Checker{
	{"go", "go.mod", []string{"go", "build", "./..."}, []string{".go"}},
	{"rust", "Cargo.toml", []string{"cargo", "check", "--quiet"}, []string{".rs"}},
	{"typescript", "tsconfig.json", []string{"npx", "--no-install", "tsc", "--noEmit"},
		[]string{".ts", ".tsx"}},
	{"swift", "Package.swift", []string{"swift", "build"}, []string{".swift"}},
}

// Result is what the model is told.
type Result struct {
	// Ran is the checker's name, or "" when nothing applied.
	Ran string
	// OK is true when the project still builds.
	OK bool
	// Output is the compiler's complaint, trimmed. Empty when OK.
	Output string
}

// Note renders the result as the line appended to a tool result.
// Empty when nothing ran — silence is right when there is nothing to
// say, and a "no checker for this project" note on every edit would be
// noise the model has to read every round.
func (r Result) Note() string {
	switch {
	case r.Ran == "":
		return ""
	case r.OK:
		return fmt.Sprintf("\n\n[verify] %s: ok", r.Ran)
	default:
		return fmt.Sprintf("\n\n[verify] %s FAILED — you broke the build with this "+
			"change. Fix it now, in this turn, before doing anything else:\n%s",
			r.Ran, r.Output)
	}
}

// Broke reports whether the project stopped building.
func (r Result) Broke() bool { return r.Ran != "" && !r.OK }

// Run picks a checker for the repo at root and runs it, if any of the
// changed paths is a file that checker cares about.
//
// A timeout, because a first `cargo check` on a cold target directory
// can take minutes and the user is watching a turn, not a CI run. A
// check that times out reports as ok rather than as a failure: telling
// the model it broke the build when the truth is "we did not find out"
// would send it editing code that was fine.
func Run(ctx context.Context, root string, changed []string, timeout time.Duration) Result {
	if root == "" || len(changed) == 0 {
		return Result{}
	}
	c, ok := pick(root, changed)
	if !ok {
		return Result{}
	}
	if _, err := exec.LookPath(c.Cmd[0]); err != nil {
		return Result{} // toolchain not installed; not the agent's problem
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Cmd[0], c.Cmd[1:]...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err == nil {
		return Result{Ran: c.Name, OK: true}
	}
	if ctx.Err() != nil {
		return Result{} // timed out — we did not find out, so say nothing
	}
	return Result{Ran: c.Name, OK: false, Output: trim(string(out))}
}

// pick finds the checker for this repo, and returns false when none of
// the changed files could affect it.
func pick(root string, changed []string) (Checker, bool) {
	for _, c := range checkers {
		if _, err := os.Stat(filepath.Join(root, c.Marker)); err != nil {
			continue
		}
		for _, p := range changed {
			ext := strings.ToLower(filepath.Ext(p))
			for _, e := range c.Exts {
				if ext == e {
					return c, true
				}
			}
		}
		return Checker{}, false // right project, irrelevant edit
	}
	return Checker{}, false
}

// trim keeps the head of the output. Compilers put the first and most
// useful error first, and a thousand-line cascade from one bad edit
// costs the model its context for no extra information.
func trim(s string) string {
	const maxLines, maxBytes = 40, 4000
	s = strings.TrimSpace(s)
	if len(s) > maxBytes {
		s = s[:maxBytes] + "\n… (truncated)"
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		return strings.Join(lines[:maxLines], "\n") + "\n… (truncated)"
	}
	return s
}
