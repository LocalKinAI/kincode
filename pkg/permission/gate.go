package permission

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// The approval gate: allow / ask / deny per tool call, decided by the
// harness rather than by the model's good intentions.
//
// kincode had plan mode and a bash blocklist and nothing in between —
// and server mode forced yolo, so the desktop shell ran an agent that
// could edit any file and run any command without asking once. That
// was honest when the shell had no approval UI. It has one now.
//
// The rule grammar is kinclaw's, deliberately: a person should learn
// "shell(git push*)" once and have it mean the same thing in both.
//
//	bash                 — every bash call
//	file_*               — a prefix, matching file_read/file_write/…
//	bash(git push*)      — a call whose primary parameter starts with it
//	file_write(~/tmp*)   — same, with ~ expanded on both sides
type Mode string

const (
	// ModeAuto runs everything the persona exposes (kincode's old
	// behaviour, and still what -yolo means).
	ModeAuto Mode = "auto"
	// ModeAsk routes calls matching the ask rules to the Asker.
	ModeAsk Mode = "ask"
)

// Answer is what an Asker returns.
type Answer int

const (
	Deny Answer = iota
	AllowOnce
	// AllowSession approves this tool for the rest of the process.
	AllowSession
)

// Request is what the human is shown.
type Request struct {
	ID      string            `json:"id"`
	Tool    string            `json:"tool"`
	Params  map[string]string `json:"params,omitempty"`
	Summary string            `json:"summary"`
	// Reason says why the gate stopped here: which rule matched, or
	// that the command looked irreversible.
	Reason string `json:"reason"`
}

// Asker presents a Request and returns the human's Answer. It must
// honour ctx — an interrupted turn should not leave a prompt dangling.
type Asker interface {
	Ask(ctx context.Context, req Request) (Answer, error)
}

// AskerFunc adapts a function to Asker.
type AskerFunc func(ctx context.Context, req Request) (Answer, error)

// Ask implements Asker.
func (f AskerFunc) Ask(ctx context.Context, req Request) (Answer, error) { return f(ctx, req) }

// Gate holds the rules and the session's answers.
type Gate struct {
	mu      sync.Mutex
	mode    Mode
	ask     []rule
	allow   []rule
	asker   Asker
	session map[string]bool
	seq     int
}

// NewGate builds a gate. Empty ask falls back to the default set.
func NewGate(mode Mode, ask, allow []string, asker Asker) *Gate {
	if mode == "" {
		mode = ModeAuto
	}
	if len(ask) == 0 {
		ask = DefaultAsk()
	}
	return &Gate{
		mode: mode, ask: parseRules(ask), allow: parseRules(allow),
		asker: asker, session: map[string]bool{},
	}
}

// DefaultAsk is what a coding agent should stop for when the persona
// says `mode: ask` and lists nothing: everything that changes a file,
// runs a command, or reaches the network on its own.
func DefaultAsk() []string {
	return []string{"bash", "file_write", "file_edit", "multi_edit", "agent_spawn"}
}

// Mode reports the gate's current mode.
func (g *Gate) Mode() Mode { g.mu.Lock(); defer g.mu.Unlock(); return g.mode }

// SetMode changes it at runtime. Unknown values are ignored so a typo
// cannot silently open the gate.
func (g *Gate) SetMode(m Mode) {
	if m != ModeAuto && m != ModeAsk {
		return
	}
	g.mu.Lock()
	g.mode = m
	g.mu.Unlock()
}

// SetAsker installs (or replaces) the human-facing prompt.
func (g *Gate) SetAsker(a Asker) { g.mu.Lock(); g.asker = a; g.mu.Unlock() }

// AllowSession approves a tool for the rest of the process.
func (g *Gate) AllowSession(tool string) { g.mu.Lock(); g.session[tool] = true; g.mu.Unlock() }

// SessionAllowed lists the tools approved for the session, sorted.
func (g *Gate) SessionAllowed() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.session))
	for k := range g.session {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Check decides one call. It blocks while an Asker is consulted, and
// returns the reason when it refuses so the model can adapt instead of
// retrying the same thing.
func (g *Gate) Check(ctx context.Context, tool string, params map[string]string) (bool, string) {
	g.mu.Lock()
	mode, asker, allowRules, askRules := g.mode, g.asker, g.allow, g.ask
	sessionOK := g.session[tool]
	g.seq++
	id := fmt.Sprintf("perm-%d", g.seq)
	g.mu.Unlock()

	if mode != ModeAsk {
		return true, ""
	}
	if matchAny(allowRules, tool, params) || sessionOK {
		return true, ""
	}

	reason := ""
	switch {
	case matchAny(askRules, tool, params):
		reason = "matches a permissions.ask rule"
	case tool == "bash" && DangerousCommand(params["command"]):
		// Irreversible shell always asks, whatever the rules say —
		// unless an allow rule covered it above.
		reason = "the command looks irreversible"
	default:
		return true, ""
	}

	if asker == nil {
		return false, fmt.Sprintf(
			"Permission required for %s (%s) but no approver is attached. "+
				"Run kincode interactively, or grant it in the persona under "+
				"permissions.allow.", tool, reason)
	}

	ans, err := asker.Ask(ctx, Request{
		ID: id, Tool: tool, Params: params,
		Summary: Summary(tool, params), Reason: reason,
	})
	if err != nil {
		return false, fmt.Sprintf("Permission request failed: %v", err)
	}
	switch ans {
	case AllowSession:
		g.AllowSession(tool)
		return true, ""
	case AllowOnce:
		return true, ""
	default:
		return false, fmt.Sprintf(
			"The user refused this %s call. Do not retry it — take another "+
				"route, or explain what you need and why.", tool)
	}
}

// ─── Rules ───────────────────────────────────────────────────────────

type rule struct {
	tool        string
	toolPrefix  bool
	param       string
	hasParam    bool
	paramPrefix bool
}

func parseRules(specs []string) []rule {
	out := make([]rule, 0, len(specs))
	for _, s := range specs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		var r rule
		if i := strings.IndexByte(s, '('); i > 0 && strings.HasSuffix(s, ")") {
			r.tool = s[:i]
			r.param = s[i+1 : len(s)-1]
			r.hasParam = true
			if strings.HasSuffix(r.param, "*") {
				r.param = strings.TrimSuffix(r.param, "*")
				r.paramPrefix = true
			}
		} else {
			r.tool = s
		}
		if strings.HasSuffix(r.tool, "*") {
			r.tool = strings.TrimSuffix(r.tool, "*")
			r.toolPrefix = true
		}
		out = append(out, r)
	}
	return out
}

func (r rule) matches(tool string, params map[string]string) bool {
	if r.toolPrefix {
		if !strings.HasPrefix(tool, r.tool) {
			return false
		}
	} else if tool != r.tool {
		return false
	}
	if !r.hasParam {
		return true
	}
	key := primaryParam(tool)
	if key == "" {
		return false
	}
	v := strings.TrimSpace(params[key])
	want := r.param
	if key == "file_path" {
		v, want = expandTilde(v), expandTilde(want)
	}
	if r.paramPrefix {
		return strings.HasPrefix(v, want)
	}
	return v == want
}

func matchAny(rules []rule, tool string, params map[string]string) bool {
	// A shell command is not one string to prefix-match; it is however
	// many commands the shell will actually run. See matchBash.
	if tool == "bash" {
		return matchBash(rules, params["command"])
	}
	for _, r := range rules {
		if r.matches(tool, params) {
			return true
		}
	}
	return false
}

// Matches reports whether any spec matches the call. Exported for
// callers that reuse the grammar.
func Matches(specs []string, tool string, params map[string]string) bool {
	return matchAny(parseRules(specs), tool, params)
}

// matchBash decides whether the allow list covers a shell command.
//
// Testing a prefix rule against the whole command string makes every
// allow-listed verb a doorway: `bash(ls*)` would cover `ls; rm -rf ~`,
// and `bash(echo*)` would cover `echo hi > ~/.zshrc`. (kinclaw shipped
// exactly that bug and it was found the hard way — a file appeared on
// the Desktop with no approval asked for.) So the command is split
// into the simple commands the shell would run and every one has to be
// covered on its own; anything that writes or substitutes is never
// covered by a prefix rule, because the danger is in the target rather
// than the verb.
func matchBash(rules []rule, cmd string) bool {
	bashRules := make([]rule, 0, len(rules))
	for _, r := range rules {
		if r.toolPrefix && !strings.HasPrefix("bash", r.tool) {
			continue
		}
		if !r.toolPrefix && r.tool != "bash" {
			continue
		}
		if !r.hasParam {
			return true // `allow: [bash]` — the user said everything.
		}
		bashRules = append(bashRules, r)
	}
	if len(bashRules) == 0 {
		return false
	}
	segments, safe := shellSegments(cmd)
	if !safe || len(segments) == 0 {
		return false
	}
	for _, seg := range segments {
		covered := false
		for _, r := range bashRules {
			if r.paramPrefix && strings.HasPrefix(seg, r.param) {
				covered = true
				break
			}
			if !r.paramPrefix && seg == r.param {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

// shellSegments splits a command on the operators that start a new
// command, respecting quotes so `grep "a && b" f` stays one segment.
// safe is false when the command contains something a prefix rule has
// no business approving: a redirect, a substitution, a subshell, or a
// backgrounding `&`.
func shellSegments(cmd string) (segments []string, safe bool) {
	var cur strings.Builder
	var quote byte
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segments = append(segments, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
			if quote == '"' {
				if c == '\\' && i+1 < len(cmd) {
					cur.WriteByte(c)
					i++
					cur.WriteByte(cmd[i])
					continue
				}
				if c == '$' && i+1 < len(cmd) && cmd[i+1] == '(' {
					return nil, false
				}
				if c == '`' {
					return nil, false
				}
			}
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case '\\':
			if i+1 < len(cmd) {
				cur.WriteByte(c)
				i++
				cur.WriteByte(cmd[i])
			}
		case '`', '<', '>', '(', ')':
			return nil, false
		case '$':
			if i+1 < len(cmd) && (cmd[i+1] == '(' || cmd[i+1] == '{') {
				return nil, false
			}
			cur.WriteByte(c)
		case ';', '\n':
			flush()
		case '|':
			if i+1 < len(cmd) && cmd[i+1] == '|' {
				i++
			}
			flush()
		case '&':
			if i+1 < len(cmd) && cmd[i+1] == '&' {
				i++
				flush()
				continue
			}
			return nil, false
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return segments, true
}

// primaryParam names the parameter a `tool(...)` rule constrains.
func primaryParam(tool string) string {
	switch tool {
	case "bash":
		return "command"
	case "file_read", "file_write", "file_edit", "multi_edit":
		return "file_path"
	case "glob", "grep":
		return "pattern"
	case "web_fetch":
		return "url"
	case "web_search":
		return "query"
	case "agent_spawn":
		return "agent"
	case "memory":
		return "action"
	}
	return ""
}

// Summary renders a call as one line a person can approve at a glance.
func Summary(tool string, params map[string]string) string {
	const max = 160
	var sb strings.Builder
	sb.WriteString(tool)
	if p := primaryParam(tool); p != "" {
		if v := params[p]; v != "" {
			sb.WriteString(": " + v)
		}
	}
	s := strings.ReplaceAll(sb.String(), "\n", " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return strings.Replace(p, "~", home, 1)
		}
	}
	return p
}

// DangerousCommand reports whether a shell command looks irreversible.
// The existing blocklist refuses a handful outright; this is the wider,
// softer net — these get a human rather than a refusal, because
// `git push` and `rm -rf ./build` are things people legitimately ask
// for and things people are legitimately surprised by.
func DangerousCommand(cmd string) bool {
	c := strings.ToLower(strings.TrimSpace(cmd))
	if c == "" {
		return false
	}
	for _, re := range dangerousCommand {
		if re.MatchString(c) {
			return true
		}
	}
	return false
}

var dangerousCommand = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+(-[a-z]*[rf][a-z]*\s+)`),
	regexp.MustCompile(`\bsudo\b`),
	regexp.MustCompile(`\bgit\s+push\b`),
	regexp.MustCompile(`\bgit\s+reset\s+--hard\b`),
	regexp.MustCompile(`\bgit\s+clean\s+-[a-z]*f`),
	regexp.MustCompile(`\bgit\s+checkout\s+--\s`),
	regexp.MustCompile(`\bkill(all)?\b`),
	regexp.MustCompile(`\bchmod\s+-R\b`),
	regexp.MustCompile(`\bchown\b`),
	regexp.MustCompile(`\bdd\s+if=`),
	regexp.MustCompile(`\bmkfs\b`),
	regexp.MustCompile(`>\s*/dev/(sd|disk)`),
	regexp.MustCompile(`\bnpm\s+publish\b`),
	regexp.MustCompile(`\b(pip|npm|yarn|brew)\s+(uninstall|remove)\b`),
	regexp.MustCompile(`\bshutdown\b|\breboot\b`),
	regexp.MustCompile(`\bcurl\b.*\|\s*(ba)?sh\b`),
}
