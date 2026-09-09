// Package server hosts kincode's HTTP+SSE control plane. It mirrors
// the kinclaw kernel's transport choices (single SSE stream out,
// short POSTs in) so a desktop shell can drive both kernels with
// identical client code.
//
// The intended consumer is KinClaw Mac, which spawns kincode on
// :5002 alongside kinclaw on :5001 and routes the user's "Code mode"
// turns through this server. The shape is also browser-loadable for
// future debugging UIs — but unlike kinclaw, kincode does not embed
// an index.html in v1 (the desktop shell renders Code mode natively).
//
// Routes:
//
//	GET  /api/health                — readiness probe (200 always once listening)
//	GET  /api/state                 — JSON {repo, model, provider, message_count}
//	POST /api/repo {"path": "..."}  — chdir the agent into a repo
//	POST /api/chat {"message": ...} — kick a turn (202 immediately, output via SSE)
//	DELETE /api/chat                — interrupt the in-flight turn
//	GET  /api/events                — SSE stream of {type, ...} events
//
// Event types emitted on /api/events:
//
//	user_message       — echoed user input
//	text_delta         — streaming assistant token chunk
//	tool_call          — model requested a tool, BEFORE execution
//	tool_result        — tool finished, with output
//	turn_done          — round complete; next turn can be sent
//	error              — turn-level error
//	usage              — token counts at end of turn
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Event is a single update pushed to all SSE subscribers. Flat shape
// matching kinclaw — frontend dispatches on Type, missing fields are
// zero values.
type Event struct {
	Type string `json:"type"`

	// user_message / text_delta / assistant content.
	Text string `json:"text,omitempty"`

	// tool_call / tool_result.
	//
	// Params is JSON-named "params" (not "args") to match the kinclaw
	// kernel's event shape — same Swift struct decodes both kernels.
	// Values are stringified (map[string]string) for the same reason:
	// kinclaw stringifies its args at emission, frontend renders them
	// as text labels anyway.
	ID      string            `json:"id,omitempty"`
	Name    string            `json:"name,omitempty"`
	Summary string            `json:"summary,omitempty"`
	Params  map[string]string `json:"params,omitempty"`
	Output  string            `json:"output,omitempty"`

	// usage at turn end.
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`

	// user_message: number of image attachments on this turn (the
	// images themselves aren't echoed — the UI already has them
	// locally and base64 over SSE would balloon the stream). Lets
	// the UI render "📎 N images" alongside the user text bubble.
	ImageCount int `json:"image_count,omitempty"`

	// plan_mode events broadcast the toggle state so multi-window
	// UIs sync. omitempty would drop `false`, so we emit the bool
	// directly and let UIs key off Type=="plan_mode".
	PlanMode bool `json:"plan_mode,omitempty"`

	// error / status hints.
	Message string `json:"message,omitempty"`
}

// ChatAttachment is one image (or future media kind) attached to a
// user turn. MediaType ∈ {"image/png", "image/jpeg", "image/gif",
// "image/webp"}. Data is the raw base64-encoded image bytes (no
// data: URL prefix). Mirrors agent.Attachment, but kept duplicated
// here so pkg/server stays free of an agent import.
type ChatAttachment struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// ChatHandler is invoked for every accepted POST /api/chat. The server
// runs it in its own goroutine — the handler should respect ctx
// cancellation (sent by DELETE /api/chat or server shutdown) and call
// back into Server.Push to stream events.
//
// attachments is nil for plain text turns; non-nil when the request
// body included an `images` array. Handlers that don't support
// multimodal input can ignore the slice.
type ChatHandler func(ctx context.Context, message string, attachments []ChatAttachment)

// State is the structured response of GET /api/state. Everything
// optional — UI fills missing fields with defaults.
type State struct {
	Repo         string `json:"repo,omitempty"`
	Model        string `json:"model,omitempty"`
	Provider     string `json:"provider,omitempty"`
	MessageCount int    `json:"message_count"`
	PlanMode     bool   `json:"plan_mode"`
	// PermissionMode is "ask" or "auto" — what the approval gate is
	// doing right now, so the shell's mode picker shows the truth
	// after a reconnect rather than its own last guess.
	PermissionMode string `json:"permission_mode,omitempty"`
}

// StateHandler returns the agent's current state. Server caches
// nothing — every request asks the kernel.
type StateHandler func() State

// InterruptHandler is invoked when DELETE /api/chat fires. Should
// cancel the in-flight turn's ctx; no-op when nothing is running.
type InterruptHandler func()

// ClearHandler is invoked when POST /api/clear fires. Should reset
// the agent's conversation memory back to "fresh session" state
// (system prompt only, no user/assistant turns). Used by the desktop
// shell to recover from stuck error states without restarting the
// kincode subprocess.
type ClearHandler func()

// BrainSwitchRequest is the body of POST /api/brain. APIKey and
// Endpoint are optional — if omitted, the server reads from env
// (ANTHROPIC_API_KEY / OPENAI_API_KEY) and per-provider defaults.
type BrainSwitchRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

// BrainSwitchHandler swaps the running agent's provider/model. Should
// build a new provider object using the same logic the boot path
// uses, then call agent.SetProvider. Returns an error if the new
// config is invalid (e.g. anthropic without a key) so the UI can
// surface "couldn't switch — check API key" without entering a
// half-broken state.
type BrainSwitchHandler func(req BrainSwitchRequest) error

// Server owns the HTTP listener + SSE subscriber map.
type Server struct {
	addr             string
	chatHandler      ChatHandler
	interruptHandler InterruptHandler
	clearHandler     ClearHandler
	brainHandler     BrainSwitchHandler
	stateHandler     StateHandler
	planModeHandler  PlanModeHandler
	permModeHandler  PermissionModeHandler
	repoChanged      func(dir string)
	asks             *askWaiters

	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// New constructs a server bound to addr (e.g. ":5002"). The chat
// handler is required — it's the only way to do anything useful with
// this server. Other handlers are wired separately via setters and
// fall back to 501 / empty defaults when unset.
func New(addr string, h ChatHandler) *Server {
	return &Server{
		addr:        addr,
		chatHandler: h,
		subs:        make(map[chan Event]struct{}),
		asks:        newAskWaiters(),
	}
}

// SetInterruptHandler wires DELETE /api/chat. Without it the endpoint
// returns 501 — UI's interrupt button stays a no-op until normal
// turn_done.
func (s *Server) SetInterruptHandler(h InterruptHandler) { s.interruptHandler = h }

// SetClearHandler wires POST /api/clear. Without it the endpoint
// returns 501 — UI's "new session" button can only clear local
// state, not the agent's server-side message history.
func (s *Server) SetClearHandler(h ClearHandler) { s.clearHandler = h }

// SetBrainSwitchHandler wires POST /api/brain. Without it the
// endpoint returns 501 — UI's brain dropdown will fail with a
// clear "not wired" error.
func (s *Server) SetBrainSwitchHandler(h BrainSwitchHandler) { s.brainHandler = h }

// SetStateHandler wires GET /api/state. Without it the endpoint
// returns an empty State{} (UI shows zero counts / no repo).
func (s *Server) SetStateHandler(h StateHandler) { s.stateHandler = h }

// PlanModeHandler is invoked when POST /api/plan_mode fires. The
// handler should toggle the agent's plan-mode state and return the
// new state so the UI can confirm the switch took effect (round-
// trip the bool rather than trusting the request value).
type PlanModeHandler func(enabled bool) (newState bool)

// SetPlanModeHandler wires POST /api/plan_mode. Without it the
// endpoint returns 501 — UI's plan-mode toggle has no effect.
func (s *Server) SetPlanModeHandler(h PlanModeHandler) { s.planModeHandler = h }

// PermissionModeHandler switches the approval gate between "auto" and
// "ask" and returns the mode actually in force.
type PermissionModeHandler func(mode string) string

// SetRepoChangedHandler is called after POST /api/repo has chdir'd,
// so the agent can re-derive anything it knows about the repo.
func (s *Server) SetRepoChangedHandler(f func(dir string)) { s.repoChanged = f }

// SetPermissionModeHandler wires POST /api/permission_mode.
func (s *Server) SetPermissionModeHandler(h PermissionModeHandler) { s.permModeHandler = h }

// Push fans an event out to every subscriber non-blockingly. Slow
// browsers/clients drop events past their 64-deep buffer rather than
// stalling the agent.
func (s *Server) Push(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (s *Server) subscribe() chan Event {
	ch := make(chan Event, 64)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func (s *Server) unsubscribe(ch chan Event) {
	s.mu.Lock()
	delete(s.subs, ch)
	s.mu.Unlock()
	close(ch)
}

// ListenAndServe binds the listener and serves until ctx fires.
// Returns once Shutdown completes (or immediately on bind failure).
func (s *Server) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/repo", s.handleRepo)
	mux.HandleFunc("/api/chat", s.handleChat)
	mux.HandleFunc("/api/clear", s.handleClear)
	mux.HandleFunc("/api/brain", s.handleBrain)
	mux.HandleFunc("/api/plan_mode", s.handlePlanMode)
	mux.HandleFunc("/api/permission", s.handlePermission)
	mux.HandleFunc("/api/permission_mode", s.handlePermissionMode)
	mux.HandleFunc("/api/events", s.handleEvents)

	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		// Same AirPlay-Receiver-on-5000 footgun applies, plus our
		// chosen :5002 is unusual enough that surfacing the bind error
		// helps anyone who ran two kincodes by accident.
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}

	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(os.Stderr, "  serve: http://%s\n", s.addr)
	if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	var st State
	if s.stateHandler != nil {
		st = s.stateHandler()
	}
	if st.Repo == "" {
		// Best-effort fallback so the UI gets *some* repo label even
		// when the kernel doesn't report one.
		if cwd, err := os.Getwd(); err == nil {
			st.Repo = cwd
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// handleRepo chdirs the kincode process into the requested directory.
// Coarse — affects every tool call after the chdir lands. Fine for v1
// since kincode runs as a single-session subprocess per Mac shell;
// multi-session repo isolation is a later story.
func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	abs, err := filepath.Abs(strings.TrimSpace(body.Path))
	if err != nil {
		http.Error(w, "invalid path: "+err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		http.Error(w, "path not found: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !info.IsDir() {
		http.Error(w, "path is not a directory", http.StatusBadRequest)
		return
	}
	if err := os.Chdir(abs); err != nil {
		http.Error(w, "chdir failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// The agent's system prompt describes the repo it is in — branch,
	// uncommitted files, recent commits. That paragraph is now about
	// the wrong project.
	if s.repoChanged != nil {
		s.repoChanged(abs)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"repo": abs})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleChatPost(w, r)
	case http.MethodDelete:
		s.handleChatDelete(w, r)
	default:
		http.Error(w, "POST or DELETE only", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleChatPost(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string           `json:"message"`
		Images  []ChatAttachment `json:"images,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	msg := strings.TrimSpace(body.Message)
	// An images-only message (e.g. user drops an image with no
	// caption) is valid — the model can describe what it sees. So
	// only reject when both text and images are absent.
	if msg == "" && len(body.Images) == 0 {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}

	// Echo back into the SSE stream so the frontend doesn't need a
	// separate "you said:" rendering path. Image presence is signaled
	// by an image_count field — the actual base64 data isn't echoed
	// (too large; UI already has the image locally).
	echo := Event{Type: "user_message", Text: msg}
	if len(body.Images) > 0 {
		echo.ImageCount = len(body.Images)
	}
	s.Push(echo)

	// Run the turn async; respond 202 so the POST resolves and the
	// SSE stream is the only long-lived connection.
	go s.chatHandler(context.Background(), msg, body.Images)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleChatDelete(w http.ResponseWriter, _ *http.Request) {
	if s.interruptHandler == nil {
		http.Error(w, "interrupt not wired", http.StatusNotImplemented)
		return
	}
	s.interruptHandler()
	w.WriteHeader(http.StatusAccepted)
}

// handleBrain swaps the running agent's provider/model live. Body:
// {provider, model, api_key?, endpoint?}. On success returns 202
// + the post-switch state JSON; on failure returns the error from
// the brain handler (e.g. "anthropic API key required") with 400.
//
// Cancels any in-flight turn first — same reason as handleClear:
// swapping providers mid-iteration would race against the agent's
// Stream call.
func (s *Server) handleBrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.brainHandler == nil {
		http.Error(w, "brain switch not wired", http.StatusNotImplemented)
		return
	}
	var req BrainSwitchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Provider == "" || req.Model == "" {
		http.Error(w, "provider and model required", http.StatusBadRequest)
		return
	}
	if s.interruptHandler != nil {
		s.interruptHandler()
	}
	if err := s.brainHandler(req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if s.stateHandler != nil {
		_ = json.NewEncoder(w).Encode(s.stateHandler())
	}
}

// handleClear resets the agent's conversation memory back to a fresh
// "system prompt only" state. Used by the desktop shell to recover
// from stuck error states (e.g. provider rejected a malformed history
// and every retry hits the same 400) without restarting the kincode
// subprocess.
func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.clearHandler == nil {
		http.Error(w, "clear not wired", http.StatusNotImplemented)
		return
	}
	// Cancel any in-flight turn first — clearing messages while the
	// agent loop is mid-iteration would race against append calls.
	if s.interruptHandler != nil {
		s.interruptHandler()
	}
	s.clearHandler()
	w.WriteHeader(http.StatusAccepted)
}

// handlePlanMode flips the agent's plan-mode flag. Body:
// {"enabled": true|false}. Returns the post-toggle state — UI uses
// the round-tripped bool to confirm rather than trusting the
// request value (handler may decide to refuse e.g. mid-turn).
//
// Plan mode is "describe what you'd do without doing it" — read-only
// tools allowed, write/exec/spawn denied with a message that teaches
// the model to emit a markdown plan instead.
func (s *Server) handlePlanMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.planModeHandler == nil {
		http.Error(w, "plan_mode not wired", http.StatusNotImplemented)
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	newState := s.planModeHandler(body.Enabled)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": newState})

	// Also broadcast over SSE so any other connected UI (e.g. a
	// second window) updates. Frontend listens for type:plan_mode.
	s.Push(Event{Type: "plan_mode", PlanMode: newState})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // bypass proxy buffering

	ch := s.subscribe()
	defer s.unsubscribe(ch)

	// Hello so the client flips to "connected" without waiting for
	// the first real event.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
