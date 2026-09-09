package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/LocalKinAI/kincode/pkg/permission"
)

// Approval over HTTP: the gate parks the turn, the desktop shell shows
// a card, the answer comes back on another request.
//
// Server mode used to force -yolo — "there's no user-facing prompt
// loop to gate tool calls through" — which meant the desktop shell ran
// an agent that could edit any file and run any command without ever
// asking. The shell has an approval card now; this is the wire between
// them.

// pendingAsk is one parked request waiting for a human.
type pendingAsk struct {
	answer chan permission.Answer
}

type askWaiters struct {
	mu      sync.Mutex
	pending map[string]*pendingAsk
}

func newAskWaiters() *askWaiters {
	return &askWaiters{pending: map[string]*pendingAsk{}}
}

// AskPermission is the server's permission.Asker: it pushes the
// request to every connected client and blocks until one answers or
// the turn is cancelled.
//
// A cancelled turn resolves as Deny rather than hanging — a parked
// approval whose turn is already gone would keep the agent stuck
// behind a card nobody can see.
func (s *Server) AskPermission(ctx context.Context, req permission.Request) (permission.Answer, error) {
	w := &pendingAsk{answer: make(chan permission.Answer, 1)}
	s.asks.mu.Lock()
	s.asks.pending[req.ID] = w
	s.asks.mu.Unlock()
	defer func() {
		s.asks.mu.Lock()
		delete(s.asks.pending, req.ID)
		s.asks.mu.Unlock()
	}()

	s.Push(Event{
		Type:    "permission_request",
		ID:      req.ID,
		Name:    req.Tool,
		Summary: req.Summary,
		Message: req.Reason,
		Params:  req.Params,
	})

	select {
	case ans := <-w.answer:
		s.Push(Event{Type: "permission_resolved", ID: req.ID})
		return ans, nil
	case <-ctx.Done():
		s.Push(Event{Type: "permission_resolved", ID: req.ID})
		return permission.Deny, ctx.Err()
	}
}

// handlePermission answers a parked request.
// Body: {"id": "perm-3", "decision": "allow" | "allow_session" | "deny"}.
// 404 means it already resolved — the turn was cancelled, or a second
// window answered first — which the client treats as "never mind".
func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID       string `json:"id"`
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var ans permission.Answer
	switch body.Decision {
	case "allow", "allow_once":
		ans = permission.AllowOnce
	case "allow_session", "allow_always":
		// kincode has no persisted allow list yet, so "always" is
		// honoured as "for this session" rather than silently doing
		// less than the button said.
		ans = permission.AllowSession
	case "deny", "":
		ans = permission.Deny
	default:
		http.Error(w, fmt.Sprintf("unknown decision %q", body.Decision), http.StatusBadRequest)
		return
	}

	s.asks.mu.Lock()
	p := s.asks.pending[body.ID]
	s.asks.mu.Unlock()
	if p == nil {
		http.Error(w, "no such pending request", http.StatusNotFound)
		return
	}
	select {
	case p.answer <- ans:
	default:
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handlePermissionMode switches the gate between "ask" and "auto"
// mid-session. The reply says which mode is in force, which is not
// always the one asked for: an unknown value is ignored rather than
// defaulted, so a typo cannot open the gate.
func (s *Server) handlePermissionMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.permModeHandler == nil {
		http.Error(w, "permission_mode not wired", http.StatusNotImplemented)
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mode := s.permModeHandler(body.Mode)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"mode": mode})
	s.Push(Event{Type: "permission_mode", Name: mode})
}
