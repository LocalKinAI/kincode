// Package checkpoint remembers what a file looked like before the
// agent changed it, so one turn can be taken back.
//
// Without this the only way out of a bad turn is `git checkout`, which
// is both too much and not enough: too much because it also destroys
// the uncommitted work the user had in the tree before the agent
// started, and not enough because a file the agent created is
// untracked and survives. So this snapshots exactly the files the
// agent is about to touch, and undo puts back exactly those — the
// user's other work is never in scope.
//
// It is not a version-control system and does not try to be. It is the
// answer to "no, not like that", which is a thing people say within
// seconds of seeing the diff, and only ever about the last few turns.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// keptTurns is how far back undo goes. Beyond a handful the question
// stops being "undo that" and starts being "restore this file to how
// it was on Tuesday", which is git's job.
const keptTurns = 10

// maxFileBytes skips snapshotting anything enormous. A source file the
// agent edits is kilobytes; something 20MB is a build artifact or a
// dataset, and copying it on every turn to make it undoable is a bad
// trade.
const maxFileBytes = 20 << 20

// entry is one file's prior state.
type entry struct {
	Path string `json:"path"`
	// Existed is false when the agent created the file — undoing means
	// deleting it, not restoring bytes.
	Existed bool        `json:"existed"`
	Blob    string      `json:"blob,omitempty"`
	Mode    os.FileMode `json:"mode,omitempty"`
}

type manifest struct {
	Turn    int       `json:"turn"`
	Started time.Time `json:"started"`
	Prompt  string    `json:"prompt,omitempty"`
	Entries []entry   `json:"entries"`
}

// Store holds the snapshots for one repo.
type Store struct {
	root string // where snapshots live
	cur  *manifest
	seq  int
	seen map[string]bool // paths already saved this turn
}

// New opens (or creates) the store for a repo. A nil Store is valid
// and does nothing, so callers never have to nil-check.
func New(repo string) (*Store, error) {
	if repo == "" {
		return nil, fmt.Errorf("no repo")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	// Keyed by the repo's path so switching folders does not offer to
	// undo a turn that happened somewhere else.
	key := strings.NewReplacer("/", "_", " ", "_", ":", "_").Replace(strings.Trim(repo, "/"))
	if len(key) > 120 {
		key = key[len(key)-120:]
	}
	root := filepath.Join(home, ".kincode", "undo", key)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	s := &Store{root: root}
	s.seq = s.lastTurn()
	return s, nil
}

// Begin opens a new turn. prompt is kept only to label the undo in the
// UI ("undo: 把 greet 改名").
func (s *Store) Begin(prompt string) {
	if s == nil {
		return
	}
	s.seq++
	if len(prompt) > 120 {
		prompt = prompt[:120] + "…"
	}
	s.cur = &manifest{Turn: s.seq, Started: time.Now(), Prompt: prompt}
	s.seen = map[string]bool{}
}

// Save records a file's current state before the agent modifies it.
// Called once per path per turn: the first state is the one worth
// keeping, since undoing a turn means going back to before all of it.
func (s *Store) Save(path string) {
	if s == nil || s.cur == nil || path == "" {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil || s.seen[abs] {
		return
	}
	s.seen[abs] = true

	e := entry{Path: abs}
	info, statErr := os.Stat(abs)
	switch {
	case statErr != nil:
		// Does not exist yet — the agent is creating it.
	case info.IsDir():
		return
	case info.Size() > maxFileBytes:
		return
	default:
		data, err := os.ReadFile(abs)
		if err != nil {
			return
		}
		blob := fmt.Sprintf("%d-%s", len(s.cur.Entries), filepath.Base(abs))
		if err := os.MkdirAll(s.turnDir(s.cur.Turn), 0o755); err != nil {
			return
		}
		if err := os.WriteFile(filepath.Join(s.turnDir(s.cur.Turn), blob), data, 0o644); err != nil {
			return
		}
		e.Existed, e.Blob, e.Mode = true, blob, info.Mode()
	}
	s.cur.Entries = append(s.cur.Entries, e)
}

// Commit writes the turn's manifest, if it touched anything. A turn
// that changed nothing leaves no undo point — offering to undo a
// conversation would be a button that does nothing.
func (s *Store) Commit() {
	if s == nil || s.cur == nil {
		return
	}
	defer func() { s.cur = nil }()
	if len(s.cur.Entries) == 0 {
		_ = os.RemoveAll(s.turnDir(s.cur.Turn))
		s.seq--
		return
	}
	dir := s.turnDir(s.cur.Turn)
	_ = os.MkdirAll(dir, 0o755)
	data, err := json.MarshalIndent(s.cur, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644)
	s.prune()
}

// Last describes the most recent undoable turn, or false when there is
// none.
func (s *Store) Last() (turn int, prompt string, files []string, ok bool) {
	if s == nil {
		return 0, "", nil, false
	}
	m, err := s.read(s.lastTurn())
	if err != nil {
		return 0, "", nil, false
	}
	for _, e := range m.Entries {
		files = append(files, e.Path)
	}
	return m.Turn, m.Prompt, files, true
}

// Undo restores the most recent turn and forgets it, so undoing twice
// walks back two turns. Files the agent created are deleted; files it
// changed get their bytes back. Anything else in the tree is untouched
// — including whatever the user has been editing meanwhile.
func (s *Store) Undo() ([]string, error) {
	if s == nil {
		return nil, fmt.Errorf("no undo store")
	}
	n := s.lastTurn()
	m, err := s.read(n)
	if err != nil {
		return nil, fmt.Errorf("nothing to undo")
	}
	var restored []string
	var failed []string
	for _, e := range m.Entries {
		if !e.Existed {
			if err := os.Remove(e.Path); err != nil && !os.IsNotExist(err) {
				failed = append(failed, e.Path)
				continue
			}
			restored = append(restored, e.Path)
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.turnDir(n), e.Blob))
		if err != nil {
			failed = append(failed, e.Path)
			continue
		}
		mode := e.Mode
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(e.Path, data, mode); err != nil {
			failed = append(failed, e.Path)
			continue
		}
		restored = append(restored, e.Path)
	}
	_ = os.RemoveAll(s.turnDir(n))
	if s.seq >= n {
		s.seq = n - 1
	}
	if len(failed) > 0 {
		return restored, fmt.Errorf("could not restore %d file(s): %s",
			len(failed), strings.Join(failed, ", "))
	}
	return restored, nil
}

func (s *Store) turnDir(n int) string {
	return filepath.Join(s.root, fmt.Sprintf("turn-%06d", n))
}

func (s *Store) read(n int) (*manifest, error) {
	if n <= 0 {
		return nil, fmt.Errorf("no turn")
	}
	data, err := os.ReadFile(filepath.Join(s.turnDir(n), "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// lastTurn is the highest committed turn on disk. Read from the
// filesystem rather than remembered, so a restarted kincode can still
// undo the turn before the restart.
func (s *Store) lastTurn() int {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return 0
	}
	best := 0
	for _, e := range entries {
		var n int
		if _, err := fmt.Sscanf(e.Name(), "turn-%06d", &n); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.root, e.Name(), "manifest.json")); err != nil {
			continue
		}
		if n > best {
			best = n
		}
	}
	return best
}

// prune keeps the last keptTurns snapshots.
func (s *Store) prune() {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return
	}
	var turns []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "turn-") {
			turns = append(turns, e.Name())
		}
	}
	if len(turns) <= keptTurns {
		return
	}
	sort.Strings(turns)
	for _, old := range turns[:len(turns)-keptTurns] {
		_ = os.RemoveAll(filepath.Join(s.root, old))
	}
}
