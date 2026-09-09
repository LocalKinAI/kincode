package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo builds a git repository with one commit and chdirs into it —
// the tool reads the process's working directory, the way the server
// leaves it after the user picks a folder.
func repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, argv := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", argv...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", argv, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"add", "."},
		{"commit", "-qm", "first: the thing"},
	} {
		cmd := exec.Command("git", argv...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", argv, err, out)
		}
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return dir
}

func exec_(t *testing.T, args map[string]any) string {
	t.Helper()
	out, err := (&GitTool{}).Execute(args)
	if err != nil {
		t.Fatalf("git tool: %v\n%s", err, out)
	}
	return out
}

func TestStatusNamesTheBranchAndWhatIsDirty(t *testing.T) {
	dir := repo(t)
	if got := exec_(t, map[string]any{"action": "status"}); !strings.Contains(got, "branch: main") ||
		!strings.Contains(got, "clean") {
		t.Errorf("clean repo status:\n%s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() { println(1) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := exec_(t, map[string]any{"action": "status"})
	if !strings.Contains(got, "main.go") || strings.Contains(got, "clean") {
		t.Errorf("dirty repo status:\n%s", got)
	}
}

func TestDiffShowsTheChangeAndScopesToAPath(t *testing.T) {
	dir := repo(t)
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() { println(1) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := exec_(t, map[string]any{"action": "diff"}); !strings.Contains(got, "println") {
		t.Errorf("diff should carry the change:\n%s", got)
	}
	if got := exec_(t, map[string]any{"action": "diff", "path": "main.go"}); !strings.Contains(got, "println") {
		t.Errorf("scoped diff:\n%s", got)
	}
	if got := exec_(t, map[string]any{"action": "diff", "path": "nothere.go"}); !strings.Contains(got, "no changes") {
		t.Errorf("a file with no changes should say so, not return empty:\n%s", got)
	}
}

func TestCleanRepoDiffSaysSoRatherThanReturningNothing(t *testing.T) {
	repo(t)
	if got := exec_(t, map[string]any{"action": "diff"}); !strings.Contains(got, "no changes") {
		t.Errorf("got %q", got)
	}
}

func TestLogAndShow(t *testing.T) {
	repo(t)
	if got := exec_(t, map[string]any{"action": "log"}); !strings.Contains(got, "first: the thing") {
		t.Errorf("log:\n%s", got)
	}
	if got := exec_(t, map[string]any{"action": "show"}); !strings.Contains(got, "first: the thing") {
		t.Errorf("show:\n%s", got)
	}
}

func TestBlameNeedsAPath(t *testing.T) {
	repo(t)
	if _, err := (&GitTool{}).Execute(map[string]any{"action": "blame"}); err == nil {
		t.Error("blame without a path should be an error, not an empty answer")
	}
	if got := exec_(t, map[string]any{"action": "blame", "path": "main.go"}); !strings.Contains(got, "package main") {
		t.Errorf("blame:\n%s", got)
	}
}

func TestUnknownActionExplainsTheOptions(t *testing.T) {
	repo(t)
	_, err := (&GitTool{}).Execute(map[string]any{"action": "commit"})
	if err == nil {
		t.Fatal("the tool is read-only; commit is not an action it has")
	}
	if !strings.Contains(err.Error(), "status") {
		t.Errorf("the error should list what it can do: %v", err)
	}
}

func TestOutsideARepoIsAClearError(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()
	if _, err := (&GitTool{}).Execute(map[string]any{"action": "status"}); err == nil ||
		!strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("got %v", err)
	}
}

func TestCapTruncatesAtALineBoundaryAndSaysSo(t *testing.T) {
	long := strings.Repeat("a line of patch\n", 2000)
	got := cap(long, 500, "diff")
	if len(got) > 700 || !strings.Contains(got, "truncated") || !strings.Contains(got, "path=") {
		t.Errorf("cap produced %d bytes:\n%s", len(got), got[max(0, len(got)-200):])
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
