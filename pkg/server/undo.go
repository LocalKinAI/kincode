package server

import (
	"encoding/json"
	"net/http"
)

// handleUndo takes back the last turn's file changes.
//
// GET describes what would happen — which turn, which files — so the
// desktop shell can label the button with the thing it will undo and
// grey it out when there is nothing. POST does it.
//
// Only files the agent itself changed are in scope. Whatever the user
// was editing at the same time is not, which is the whole reason this
// exists rather than a `git checkout`.
func (s *Server) handleUndo(w http.ResponseWriter, r *http.Request) {
	if s.undoHandler == nil {
		http.Error(w, "undo not wired", http.StatusNotImplemented)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		prompt, files, ok := s.undoHandler.Peek()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"available": ok, "prompt": prompt, "files": files,
		})
	case http.MethodPost:
		files, err := s.undoHandler.Undo()
		if err != nil && len(files) == 0 {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		body := map[string]any{"restored": files}
		if err != nil {
			// Partially restored: say which, and say what went wrong.
			// Silence here would leave the tree in a state nobody
			// described.
			body["error"] = err.Error()
		}
		_ = json.NewEncoder(w).Encode(body)
		s.Push(Event{Type: "undone", Params: map[string]string{
			"count": itoa(len(files)),
		}})
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
