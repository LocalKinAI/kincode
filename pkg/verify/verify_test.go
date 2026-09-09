package verify

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A Go module that compiles, so the check has something real to say.
func goModule(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "go.mod", "module verifytest\n\ngo 1.21\n")
	write(t, dir, "main.go", body)
	return dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCleanBuildReportsOK(t *testing.T) {
	dir := goModule(t, "package main\n\nfunc main() {}\n")
	r := Run(context.Background(), dir, []string{"main.go"}, 60*time.Second)
	if r.Ran != "go" || !r.OK {
		t.Fatalf("got %+v, want a passing go check", r)
	}
	if r.Note() != "\n\n[verify] go: ok" {
		t.Errorf("note = %q", r.Note())
	}
	if r.Broke() {
		t.Error("a clean build is not broken")
	}
}

func TestBrokenBuildTellsTheModelToFixItNow(t *testing.T) {
	dir := goModule(t, "package main\n\nfunc main() { undefinedThing() }\n")
	r := Run(context.Background(), dir, []string{"main.go"}, 60*time.Second)
	if !r.Broke() {
		t.Fatalf("got %+v, want a failure", r)
	}
	note := r.Note()
	for _, want := range []string{"FAILED", "undefinedThing", "in this turn"} {
		if !contains(note, want) {
			t.Errorf("note missing %q:\n%s", want, note)
		}
	}
}

func TestIrrelevantEditSkipsTheBuild(t *testing.T) {
	// The project is Go, but a README change cannot break the build —
	// compiling it would be seconds spent to learn nothing.
	dir := goModule(t, "package main\n\nfunc main() {}\n")
	if r := Run(context.Background(), dir, []string{"README.md"}, 60*time.Second); r.Ran != "" {
		t.Errorf("got %+v, want nothing to run", r)
	}
}

func TestUnknownProjectStaysSilent(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "notes.txt", "hello")
	r := Run(context.Background(), dir, []string{"notes.txt"}, 60*time.Second)
	if r.Ran != "" || r.Note() != "" {
		t.Errorf("got %+v; a project we don't recognise should say nothing", r)
	}
}

func TestTimeoutReportsNothingRatherThanFailure(t *testing.T) {
	// Telling the model it broke the build when the truth is "we did
	// not find out" would send it editing code that was fine.
	dir := goModule(t, "package main\n\nfunc main() {}\n")
	r := Run(context.Background(), dir, []string{"main.go"}, time.Nanosecond)
	if r.Broke() {
		t.Errorf("got %+v, want silence on timeout", r)
	}
}

func TestNoChangesNoCheck(t *testing.T) {
	dir := goModule(t, "package main\n\nfunc main() {}\n")
	if r := Run(context.Background(), dir, nil, time.Minute); r.Ran != "" {
		t.Errorf("got %+v", r)
	}
}

func TestOutputIsTrimmed(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "some compiler noise on a line\n"
	}
	got := trim(long)
	if len(got) >= len(long) || !contains(got, "truncated") {
		t.Errorf("trim kept %d of %d bytes", len(got), len(long))
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
