package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
)

func store(t *testing.T) (*Store, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // snapshots go under $HOME/.kincode
	repo := t.TempDir()
	s, err := New(repo)
	if err != nil {
		t.Fatal(err)
	}
	return s, repo
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUndoRestoresWhatTheAgentChanged(t *testing.T) {
	s, repo := store(t)
	f := filepath.Join(repo, "main.go")
	write(t, f, "original\n")

	s.Begin("change main.go")
	s.Save(f)
	write(t, f, "agent's version\n")
	s.Commit()

	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := read(t, f); got != "original\n" {
		t.Errorf("got %q, want the original back", got)
	}
}

func TestUndoDeletesAFileTheAgentCreated(t *testing.T) {
	s, repo := store(t)
	f := filepath.Join(repo, "new.go")

	s.Begin("add a file")
	s.Save(f) // does not exist yet
	write(t, f, "package main\n")
	s.Commit()

	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("a file the agent created should be gone after undo")
	}
}

func TestUndoLeavesTheUsersOwnWorkAlone(t *testing.T) {
	// The reason this exists instead of `git checkout`: the user is
	// editing something else at the same time, and taking back the
	// agent's turn must not touch it.
	s, repo := store(t)
	agentFile := filepath.Join(repo, "agent.go")
	mine := filepath.Join(repo, "mine.go")
	write(t, agentFile, "before\n")
	write(t, mine, "my uncommitted work\n")

	s.Begin("edit agent.go")
	s.Save(agentFile)
	write(t, agentFile, "after\n")
	s.Commit()

	// The user keeps typing while the agent works.
	write(t, mine, "my uncommitted work, now longer\n")

	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := read(t, agentFile); got != "before\n" {
		t.Errorf("agent's file: got %q", got)
	}
	if got := read(t, mine); got != "my uncommitted work, now longer\n" {
		t.Errorf("the user's file was touched: %q", got)
	}
}

func TestUndoTwiceWalksBackTwoTurns(t *testing.T) {
	s, repo := store(t)
	f := filepath.Join(repo, "f.txt")
	write(t, f, "v1\n")

	s.Begin("first")
	s.Save(f)
	write(t, f, "v2\n")
	s.Commit()
	s.Begin("second")
	s.Save(f)
	write(t, f, "v3\n")
	s.Commit()

	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := read(t, f); got != "v2\n" {
		t.Fatalf("after one undo: %q", got)
	}
	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := read(t, f); got != "v1\n" {
		t.Errorf("after two undos: %q", got)
	}
}

func TestOnlyTheFirstStateOfATurnIsKept(t *testing.T) {
	// Two edits to one file in one turn: undo goes back to before the
	// turn, not to the middle of it.
	s, repo := store(t)
	f := filepath.Join(repo, "f.txt")
	write(t, f, "start\n")

	s.Begin("two edits")
	s.Save(f)
	write(t, f, "middle\n")
	s.Save(f) // second edit, same turn
	write(t, f, "end\n")
	s.Commit()

	if _, err := s.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := read(t, f); got != "start\n" {
		t.Errorf("got %q, want the state before the whole turn", got)
	}
}

func TestATurnThatChangedNothingIsNotUndoable(t *testing.T) {
	s, _ := store(t)
	s.Begin("just a question")
	s.Commit()
	if _, _, _, ok := s.Last(); ok {
		t.Error("a conversation with no edits should offer no undo")
	}
	if _, err := s.Undo(); err == nil {
		t.Error("undoing nothing should be an error, not a silent no-op")
	}
}

func TestPeekNamesTheTurnAndItsFiles(t *testing.T) {
	s, repo := store(t)
	f := filepath.Join(repo, "a.txt")
	write(t, f, "x")
	s.Begin("rename the greet function")
	s.Save(f)
	write(t, f, "y")
	s.Commit()

	_, prompt, files, ok := s.Last()
	if !ok || prompt != "rename the greet function" || len(files) != 1 {
		t.Errorf("peek: ok=%v prompt=%q files=%v", ok, prompt, files)
	}
}

func TestSnapshotsSurviveARestart(t *testing.T) {
	// kincode restarts (a rebuild, a crash); the turn before it should
	// still be undoable, so the store reads its history off disk.
	s, repo := store(t)
	f := filepath.Join(repo, "f.txt")
	write(t, f, "old\n")
	s.Begin("edit")
	s.Save(f)
	write(t, f, "new\n")
	s.Commit()

	reopened, err := New(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := reopened.Last(); !ok {
		t.Fatal("a reopened store should see the previous turn")
	}
	if _, err := reopened.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := read(t, f); got != "old\n" {
		t.Errorf("got %q", got)
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	var s *Store
	s.Begin("x")
	s.Save("/tmp/whatever")
	s.Commit()
	if _, _, _, ok := s.Last(); ok {
		t.Error("a nil store has nothing")
	}
	if _, err := s.Undo(); err == nil {
		t.Error("undo on a nil store should error, not panic")
	}
}
