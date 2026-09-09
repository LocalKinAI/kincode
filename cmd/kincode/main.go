// kincode is a lightweight AI coding assistant for the terminal.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/LocalKinAI/kincode/internal/mcp"
	"github.com/LocalKinAI/kincode/pkg/agent"
	"github.com/LocalKinAI/kincode/pkg/permission"
	"github.com/LocalKinAI/kincode/pkg/provider"
	"github.com/LocalKinAI/kincode/pkg/repl"
	"github.com/LocalKinAI/kincode/pkg/server"
	"github.com/LocalKinAI/kincode/pkg/tools"
	"gopkg.in/yaml.v3"
)

const version = "0.10.0"

func main() {
	// Subprocess hygiene: when kincode runs as a child (typically of
	// KinClaw Mac), exit cleanly when the parent dies instead of
	// being reparented to launchd and leaking the bound port. No-op
	// when launched standalone from the CLI.
	startOrphanWatch()

	providerName := flag.String("provider", "anthropic", "LLM provider: anthropic, openai, ollama")
	model := flag.String("model", "claude-sonnet-4-6", "Model name")
	soulFile := flag.String("soul", "", "Path to .soul.md file for personality/rules")
	apiKey := flag.String("api-key", "", "API key (or use ANTHROPIC_API_KEY / OPENAI_API_KEY env)")
	endpoint := flag.String("endpoint", "", "Custom API endpoint (for ollama/compatible APIs)")
	mcpConfig := flag.String("mcp", "", "Path to MCP servers config JSON file")
	yolo := flag.Bool("yolo", false, "Auto-approve all tool calls without confirmation")
	showVersion := flag.Bool("version", false, "Show version and exit")
	login := flag.Bool("login", false, "Login via Claude OAuth (use your Claude account, no API key needed)")
	serve := flag.Bool("serve", false, "Run as HTTP+SSE server instead of REPL (for desktop shells)")
	port := flag.Int("port", 5002, "Port for -serve mode (default 5002, sits next to kinclaw on 5001)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("kincode v%s\n", version)
		os.Exit(0)
	}

	// Handle -login: run OAuth flow and exit.
	if *login {
		if _, err := provider.OAuthLogin(); err != nil {
			fmt.Fprintf(os.Stderr, "Login failed: %s\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Track which flags the user set explicitly. Used below to let
	// CLI flags override soul brain config — explicit always wins.
	explicitFlag := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicitFlag[f.Name] = true })

	// Load soul file early — its brain config can change the provider /
	// model / endpoint we pick below, so we need it before key resolution.
	systemPrompt := defaultSystemPrompt()
	var soulFM *soulFrontmatter
	if *soulFile != "" {
		sp, fm, err := loadSoulFile(*soulFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading soul file: %s\n", err)
			os.Exit(1)
		}
		systemPrompt = sp
		soulFM = fm
	}

	// Apply soul brain config — fills in any flag the CLI didn't set
	// explicitly. Lets a soul like Pilot (brain.provider=ollama,
	// brain.model=kimi-k2.6:cloud) drive kincode just by passing
	// `-soul pilot.soul.md` — same shape kinclaw uses.
	if soulFM != nil {
		if soulFM.Brain != nil {
			if !explicitFlag["provider"] && soulFM.Brain.Provider != "" {
				*providerName = soulFM.Brain.Provider
			}
			if !explicitFlag["model"] && soulFM.Brain.Model != "" {
				*model = soulFM.Brain.Model
			}
			if !explicitFlag["endpoint"] && soulFM.Brain.Endpoint != "" {
				*endpoint = soulFM.Brain.Endpoint
			}
		} else if soulFM.Model != "" && !explicitFlag["model"] {
			// Legacy top-level model: field — older kincode souls.
			*model = soulFM.Model
		}
	}

	// Resolve API key.
	key := *apiKey
	isOAuth := false
	if key == "" {
		switch *providerName {
		case "anthropic":
			key = os.Getenv("ANTHROPIC_API_KEY")
		case "openai":
			key = os.Getenv("OPENAI_API_KEY")
		case "ollama":
			// Ollama doesn't need a key.
		}
	}

	// Serve-mode auto-fallback to Ollama when the user explicitly
	// asked for Anthropic but has no creds available. The desktop
	// shell (KinClaw Mac) spawns kincode with `-provider anthropic`
	// as the default; if the user didn't set ANTHROPIC_API_KEY and
	// hasn't OAuth'd, switching to Ollama / kimi-k2.6:cloud is the
	// graceful fallback — matches what kinclaw kernel uses by default
	// (per pilot.soul.md), so kincode "just works" on the same Ollama
	// install the user already has running for kinclaw.
	//
	// Skipped when the user explicitly passed -provider on the CLI
	// (their choice wins) or already has Anthropic creds.
	if *serve && *providerName == "anthropic" && key == "" {
		if _, oauthErr := provider.GetValidToken(); oauthErr != nil {
			fmt.Fprintln(os.Stderr,
				"[serve] no Anthropic creds — falling back to ollama / kimi-k2.6:cloud")
			*providerName = "ollama"
			if *model == "claude-sonnet-4-6" {
				*model = "kimi-k2.6:cloud"
			}
		}
	}

	// Set default endpoints and models per provider.
	ep := *endpoint
	mdl := *model
	switch *providerName {
	case "anthropic":
		if key == "" {
			// Try OAuth token as fallback.
			token, err := provider.GetValidToken()
			if err != nil {
				if *serve {
					// Server mode: don't hard-exit. The server is the
					// "always available" surface for desktop shells —
					// boot it, let the user resolve creds via env or
					// /api/login (future), and surface the missing-key
					// state through chat error events on first turn.
					fmt.Fprintf(os.Stderr, "[serve] no Anthropic key (env or OAuth); chat turns will fail until creds are added\n")
				} else {
					fmt.Fprintf(os.Stderr, "OAuth error: %v\n\n", err)
					fmt.Fprintln(os.Stderr, "No API key available. Either:")
					fmt.Fprintln(os.Stderr, "  1. Run 'kincode -login' to use your Claude account")
					fmt.Fprintln(os.Stderr, "  2. Set ANTHROPIC_API_KEY environment variable")
					fmt.Fprintln(os.Stderr, "  3. Use -api-key flag")
					os.Exit(1)
				}
			} else {
				key = token
				isOAuth = true
				// Default to Haiku 4.5 for OAuth users (included in all Claude plans).
				if mdl == "claude-sonnet-4-6" {
					mdl = "claude-haiku-4-5-20251001"
				}
				fmt.Println("Using Claude OAuth session (model: " + mdl + ")")
			}
		}
	case "openai":
		if key == "" {
			if *serve {
				fmt.Fprintln(os.Stderr, "[serve] no OpenAI key; chat turns will fail until creds are added")
			} else {
				fmt.Fprintln(os.Stderr, "Error: OPENAI_API_KEY not set. Use -api-key or set the environment variable.")
				os.Exit(1)
			}
		}
		if mdl == "claude-sonnet-4-6" {
			mdl = "gpt-4o" // default for OpenAI
		}
	case "ollama":
		if ep == "" {
			ep = "http://localhost:11434/v1/chat/completions"
		}
		if mdl == "claude-sonnet-4-6" {
			mdl = "qwen3:8b" // default for Ollama
		}
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown provider %q (use anthropic, openai, or ollama)\n", *providerName)
		os.Exit(1)
	}

	// Create provider.
	var p provider.Provider
	switch *providerName {
	case "anthropic":
		ap := provider.NewAnthropic(key, mdl)
		if isOAuth {
			ap.SetOAuth(true)
		}
		p = ap
	case "openai", "ollama":
		p = provider.NewOpenAI(key, mdl, ep)
	}

	// (Soul was loaded earlier — soul.brain config influenced the
	//  provider/model/endpoint resolution above.)

	// Configure extended thinking if enabled in soul file.
	if soulFM != nil && soulFM.Thinking {
		if ap, ok := p.(*provider.AnthropicProvider); ok {
			ap.SetThinking(true, soulFM.ThinkingBudget)
			ap.SetOnThinking(func(text string) {
				fmt.Print(text)
			})
			budget := soulFM.ThinkingBudget
			if budget <= 0 {
				budget = 10000
			}
			fmt.Printf("Extended thinking enabled (budget: %d tokens)\n", budget)
		}
	}

	// Discover named subagent personas at boot. Each .md file under
	// ~/.kincode/agents/ or ~/.localkin/agents/ becomes a dispatchable
	// persona that the parent model can call via:
	//   agent_spawn(agent="code-reviewer", task="...")
	// Without an `agent` param, agent_spawn falls back to the legacy
	// generic-subagent flow (inherits parent's system prompt).
	personas := agent.ListSubAgentPersonas()
	personaInfos := make([]tools.PersonaInfo, len(personas))
	for i, p := range personas {
		personaInfos[i] = tools.PersonaInfo{
			Name:        p.Name,
			Description: p.Description,
		}
		fmt.Fprintf(os.Stderr, "[persona] loaded %s — %s\n", p.Name, p.Description)
	}

	// Initialize tools.
	registry := tools.NewRegistry()
	// Register agent_spawn with a factory that creates sub-agents
	// per call. The factory takes a persona name; "" means generic
	// subagent (inherits parent's system prompt).
	tools.RegisterDefaultsWithAgent(registry, func(personaName string) tools.SubAgentRunner {
		subRegistry := tools.NewRegistry()
		tools.RegisterDefaults(subRegistry)

		// Default to parent's system prompt.
		subPrompt := systemPrompt
		// Persona override: load the named persona's body and use it
		// as the sub-agent's system prompt. Failures fall through to
		// parent's prompt — better to run the task with a generic
		// agent than refuse outright.
		if personaName != "" {
			if persona, err := agent.LoadSubAgentPersona(personaName); err == nil {
				subPrompt = persona.SystemPrompt
				fmt.Fprintf(os.Stderr,
					"[persona] dispatching to %q: %s\n",
					persona.Name, persona.Path)
			} else {
				fmt.Fprintf(os.Stderr,
					"[persona] %q not found (%v) — falling back to parent's prompt\n",
					personaName, err)
			}
		}

		return agent.New(agent.Config{
			Provider:     p,
			Tools:        subRegistry,
			Permissions:  permission.New(true), // auto-approve for sub-agents
			SystemPrompt: subPrompt,
			MaxRounds:    10,
		})
	}, personaInfos...)

	// Load external skills — same SKILL.md format kinclaw uses, so
	// the LocalKin family shares one skill marketplace at
	// ~/.localkin/skills/. kincode-specific overrides at
	// ~/.kincode/skills/. Each skill becomes a regular tool the
	// agent can invoke alongside the 10 builtins.
	for _, skill := range tools.LoadAllExternalSkills() {
		registry.Register(skill)
		fmt.Fprintf(os.Stderr, "[skill] loaded %s — %s\n",
			skill.Name(), skill.Description())
	}

	// Load MCP servers if config provided.
	var mcpClients []*mcp.Client
	if *mcpConfig != "" {
		mcpClients = loadMCPServers(*mcpConfig, registry)
	}
	defer func() {
		for _, c := range mcpClients {
			c.Close()
		}
	}()

	// Initialize permissions.
	//
	// Server mode used to force -yolo here, on the grounds that there
	// was no prompt loop to gate through and the desktop shell had no
	// permission UI. Both stopped being true: the gate below asks
	// through whatever Asker is attached, and the shell shows an
	// approval card. An agent that edits files and runs commands
	// without ever asking is not a default anyone chose — it was a
	// stopgap that outlived its reason.
	perms := permission.New(*yolo)
	if !*yolo {
		mode := permission.ModeAuto
		var askRules, allowRules []string
		if soulFM.Permissions != nil {
			mode = permission.Mode(soulFM.Permissions.Mode)
			askRules, allowRules = soulFM.Permissions.Ask, soulFM.Permissions.Allow
		}
		perms.SetGate(permission.NewGate(mode, askRules, allowRules, nil))
	}

	// Project memory — load KINCODE.md / CLAUDE.md from cwd (walks
	// up parent dirs) and append to the system prompt. Same pattern
	// as Claude Code's CLAUDE.md auto-inject; KINCODE.md takes
	// priority for kincode-specific overrides. Re-read on every
	// boot so editing the file then relaunching kincode picks it up.
	if memory := agent.LoadProjectMemory(""); memory != "" {
		systemPrompt += agent.FormatProjectMemory(memory)
		fmt.Fprintf(os.Stderr, "[memory] loaded %d chars from project memory\n",
			len(memory))
	}

	// Create agent.
	a := agent.New(agent.Config{
		Provider:     p,
		Tools:        registry,
		Permissions:  perms,
		SystemPrompt: systemPrompt,
	})

	ctx := context.Background()

	// Server mode: run as HTTP+SSE host instead of REPL. Spawned by
	// KinClaw Mac (or any other desktop shell) on a known port.
	if *serve {
		runServe(ctx, a, *port, *providerName, mdl)
		return
	}

	// Check if there's a message from command line args.
	if args := flag.Args(); len(args) > 0 {
		message := strings.Join(args, " ")
		r := repl.New(a, repl.WithMCPClients(mcpClients))
		if err := r.RunOnce(ctx, message); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", err)
			os.Exit(1)
		}
		return
	}

	// Start interactive REPL.
	r := repl.New(a, repl.WithMCPClients(mcpClients))
	if err := r.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

// runServe is the -serve mode entrypoint. Wires the agent to the
// HTTP server and translates Agent.Events callbacks into SSE events.
//
// Concurrency: only one turn runs at a time — handleChatPost holds
// turnCancel while the chatHandler goroutine is in flight, and a
// second POST while busy still echoes user_message + 202 but the
// agent.RunWithEvents call serializes on a.messages naturally
// (mutex would be cleaner but Stage 1 keeps it minimal — KinClaw Mac
// gates send button on turn_done anyway).
func runServe(ctx context.Context, a *agent.Agent, port int, providerName, model string) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// currentBrain captures the active provider+model. Mutated by the
	// brain switch handler so /api/state always reports the live
	// values, not the boot-time ones. Behind brainMu since stateHandler
	// reads on /api/state's request goroutine.
	var (
		turnMu      sync.Mutex
		turnCancel  context.CancelFunc
		brainMu     sync.Mutex
		curProvider = providerName
		curModel    = model
		srv         *server.Server // declared up-front so the chat handler closure can reference it
	)

	srv = server.New(addr, func(_ context.Context, message string, images []server.ChatAttachment) {
		// Cancel any prior turn that's somehow still in flight, then
		// install our own cancel for the new one.
		turnMu.Lock()
		if turnCancel != nil {
			turnCancel()
		}
		turnCtx, cancel := context.WithCancel(context.Background())
		turnCancel = cancel
		turnMu.Unlock()

		defer func() {
			turnMu.Lock()
			if turnCancel != nil {
				turnCancel = nil
			}
			turnMu.Unlock()
		}()

		// Translate server.ChatAttachment → agent.Attachment. The
		// duplication is intentional — pkg/server stays free of an
		// agent import, and main.go is the natural bridge layer.
		var atts []agent.Attachment
		if len(images) > 0 {
			atts = make([]agent.Attachment, len(images))
			for i, img := range images {
				atts[i] = agent.Attachment{
					MediaType: img.MediaType,
					Base64:    img.Data,
				}
			}
		}

		_, usage, err := a.RunWithImagesAndEvents(turnCtx, message, atts, agent.Events{
			OnText: func(chunk string) {
				if chunk == "" {
					return
				}
				srv.Push(server.Event{Type: "text_delta", Text: chunk})
			},
			OnToolCall: func(id, name, summary string, args map[string]any) {
				srv.Push(server.Event{
					Type: "tool_call", ID: id, Name: name, Summary: summary,
					Params: stringifyArgs(args),
				})
			},
			OnToolResult: func(id, name, result string, err error) {
				ev := server.Event{Type: "tool_result", ID: id, Name: name, Output: result}
				if err != nil {
					ev.Message = err.Error()
				}
				srv.Push(ev)
			},
			OnAssistantDone: func(_ string) {
				// turn_done fires below after the whole loop, not per
				// round — UI cares about "agent finished, you can type"
				// not "model paused for tool call".
			},
		})
		if err != nil {
			srv.Push(server.Event{Type: "error", Message: err.Error()})
		}
		srv.Push(server.Event{
			Type:         "usage",
			InputTokens:  usage.Input,
			OutputTokens: usage.Output,
		})
		srv.Push(server.Event{Type: "turn_done"})
	})

	srv.SetInterruptHandler(func() {
		turnMu.Lock()
		defer turnMu.Unlock()
		if turnCancel != nil {
			turnCancel()
		}
	})

	srv.SetClearHandler(func() {
		// Drop the agent's conversation memory back to a fresh state.
		// agent.Clear() preserves the system prompt and resets messages.
		// (handleClear in pkg/server already cancels any in-flight turn
		// before calling us, so we don't race against the agent loop.)
		a.Clear()
	})

	srv.SetBrainSwitchHandler(func(req server.BrainSwitchRequest) error {
		// Build a new provider object using the same logic the boot
		// path uses. Failures (e.g. anthropic without a key) bubble
		// up as 400 to the caller — the server doesn't enter a
		// half-broken state.
		p, err := buildProvider(req.Provider, req.Model, req.APIKey, req.Endpoint)
		if err != nil {
			return err
		}
		a.SetProvider(p)
		brainMu.Lock()
		curProvider = req.Provider
		curModel = req.Model
		brainMu.Unlock()
		fmt.Fprintf(os.Stderr, "[serve] brain switched to %s / %s\n",
			req.Provider, req.Model)
		return nil
	})

	srv.SetStateHandler(func() server.State {
		cwd, _ := os.Getwd()
		brainMu.Lock()
		prov, mdl := curProvider, curModel
		brainMu.Unlock()
		return server.State{
			Repo:         cwd,
			Provider:     prov,
			Model:        mdl,
			MessageCount: len(a.Messages()),
			PlanMode:     a.PlanMode(),
			PermissionMode: func() string {
				if g := a.Permissions().Gate(); g != nil {
					return string(g.Mode())
				}
				return "auto" // -yolo, or a persona with no permissions block
			}(),
		}
	})

	// The approval card is the Asker now. Installed here rather than at
	// gate construction because the server does not exist yet up there.
	if g := a.Permissions().Gate(); g != nil {
		g.SetAsker(permission.AskerFunc(srv.AskPermission))
	}

	// POST /api/permission_mode — switch the gate mid-session. Allowed
	// mid-turn, unlike plan mode: "stop asking me, I'm watching" is
	// decided while watching a turn, and the next tool call reads it.
	srv.SetPermissionModeHandler(func(mode string) string {
		g := a.Permissions().Gate()
		if g == nil {
			return "auto" // -yolo: there is no gate to switch.
		}
		g.SetMode(permission.Mode(mode))
		return string(g.Mode())
	})

	srv.SetPlanModeHandler(func(enabled bool) bool {
		// Cancel any in-flight turn — flipping the gate mid-loop
		// would leave the model confused (allowed write tool half a
		// second ago, denied now). Match handleClear's semantics.
		turnMu.Lock()
		if turnCancel != nil {
			turnCancel()
		}
		turnMu.Unlock()

		a.SetPlanMode(enabled)
		return a.PlanMode()
	})

	if err := srv.ListenAndServe(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "serve failed: %s\n", err)
		os.Exit(1)
	}
}

// startOrphanWatch fires a goroutine that exits the process when the
// original parent dies. macOS doesn't SIGTERM children automatically
// when their parent goes away — they get reparented to launchd (pid
// 1) and keep running, leaking subprocess + port until manually
// killed. Polling os.Getppid() every 2s catches the reparenting and
// triggers a clean exit.
//
// Skipped when the recorded parent is already pid <=1 — that means
// we were launched directly by launchd (or already orphaned), so
// there's nothing to watch for.
func startOrphanWatch() {
	origParent := os.Getppid()
	if origParent <= 1 {
		return
	}
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			if os.Getppid() != origParent {
				fmt.Fprintln(os.Stderr,
					"[orphan-watch] parent died, exiting")
				os.Exit(0)
			}
		}
	}()
}

// buildProvider constructs a provider.Provider from a desired
// (provider, model, api_key, endpoint) combo — same logic the boot
// path uses, factored out so the live brain-switch endpoint can
// share it. Returns a useful error for the UI to surface (e.g.
// "anthropic API key required") rather than a half-built provider.
//
// Resolution: explicit apiKey/endpoint > matching env var > per-
// provider default. anthropic without a key falls back to OAuth
// (~/.kincode/oauth.json) before erroring; if neither works, hard
// error so the UI can prompt for creds instead of silently 401-ing
// every turn.
func buildProvider(providerName, model, apiKey, endpoint string) (provider.Provider, error) {
	switch providerName {
	case "anthropic":
		key := apiKey
		isOAuth := false
		if key == "" {
			key = os.Getenv("ANTHROPIC_API_KEY")
		}
		if key == "" {
			token, err := provider.GetValidToken()
			if err != nil {
				return nil, fmt.Errorf("anthropic: no API key (set ANTHROPIC_API_KEY, run kincode -login, or pass api_key in body)")
			}
			key = token
			isOAuth = true
		}
		ap := provider.NewAnthropic(key, model)
		if isOAuth {
			ap.SetOAuth(true)
		}
		return ap, nil
	case "openai":
		key := apiKey
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		if key == "" {
			return nil, fmt.Errorf("openai: no API key (set OPENAI_API_KEY or pass api_key in body)")
		}
		ep := endpoint
		return provider.NewOpenAI(key, model, ep), nil
	case "ollama":
		ep := endpoint
		if ep == "" {
			ep = "http://localhost:11434/v1/chat/completions"
		}
		// Ollama doesn't need a key, but the openai client wants
		// non-empty — pass any string ("ollama" by convention).
		return provider.NewOpenAI("ollama", model, ep), nil
	default:
		return nil, fmt.Errorf("unknown provider %q (use anthropic, openai, or ollama)", providerName)
	}
}

// stringifyArgs flattens the agent's structured tool arguments into a
// {string:string} map for SSE emission. Mirrors kinclaw's choice of
// stringifying values at emission — frontends render args as text
// labels anyway, and string-keyed-string maps decode trivially in
// every language without per-value type narrowing.
//
// Encoding rules:
//   - string: pass through verbatim
//   - nil: empty string
//   - bool / number: fmt.Sprint (compact, human-readable)
//   - array / map / struct: JSON via json.Marshal — gives the
//     desktop shell parseable structured data for tools that emit
//     complex arguments (todo_write's todos array, multi_edit's
//     edits array, etc.) instead of Go's default `[map[k:v]...]`
//     formatting which is parseable in theory but ugly and
//     language-specific.
func stringifyArgs(args map[string]any) map[string]string {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]string, len(args))
	for k, v := range args {
		switch s := v.(type) {
		case string:
			out[k] = s
		case nil:
			out[k] = ""
		case bool, int, int64, float64:
			out[k] = fmt.Sprint(s)
		default:
			// Arrays, maps, structs — JSON-encode for shell consumers.
			if blob, err := json.Marshal(s); err == nil {
				out[k] = string(blob)
			} else {
				out[k] = fmt.Sprint(s)
			}
		}
	}
	return out
}

func defaultSystemPrompt() string {
	return `You are kincode, an AI coding assistant running in the user's terminal.

You have access to tools for reading, writing, and editing files, running bash commands, and searching code.

Guidelines:
- Be concise and direct.
- When editing files, use the file_edit tool for surgical changes. Use file_write only for new files.
- Always use absolute file paths.
- Run bash commands to verify your changes work (e.g., compile, test).
- If you're unsure about the codebase structure, use glob and grep to explore first.
- Ask for clarification if the request is ambiguous.`
}

// soulFrontmatter represents the YAML frontmatter in a .soul.md file.
//
// Format compatibility: kincode souls now mirror the kinclaw kernel's
// soul shape, so the same .soul.md file works for both kernels (the
// SSE protocol unification + this brain unification together mean
// kincode is a drop-in alternate kernel for any kinclaw soul).
//
//	---
//	name: "KinClaw Coder"
//	rules:
//	  - "..."
//	brain:
//	  provider: "ollama"
//	  model: "kimi-k2.6:cloud"
//	  temperature: 0.3
//	  context_length: 131072
//	  endpoint: "http://localhost:11434/v1/chat/completions"
//	thinking: true
//	thinking_budget: 10000
//	---
//	You are a senior engineer...
//
// Resolution precedence (CLI flag > soul brain > soul legacy >
// hardcoded default):
//   - If the user passed -provider/-model/-endpoint on the CLI, those
//     win regardless of what the soul says.
//   - Otherwise, soul.brain takes effect.
//   - Otherwise, the legacy top-level `model:` field (kept for back-
//     compat with kincode <0.7 souls).
//   - Otherwise, the per-provider defaults baked into main.
type soulFrontmatter struct {
	Name  string     `yaml:"name"`
	Rules []string   `yaml:"rules"`
	Brain *soulBrain `yaml:"brain,omitempty"`
	// Permissions is the approval gate, same grammar as kinclaw's:
	//
	//	permissions:
	//	  mode: ask          # or auto
	//	  ask:  ["bash", "file_write", "file_edit", "multi_edit"]
	//	  allow: ["bash(go test*)", "bash(git diff*)"]
	//
	// Absent means auto, so every persona written before this keeps
	// running exactly as it did.
	Permissions    *soulPermissions `yaml:"permissions,omitempty"`
	Model          string           `yaml:"model"`       // legacy — top-level model string
	Temperature    float64          `yaml:"temperature"` // legacy — top-level temp (unused)
	Thinking       bool             `yaml:"thinking"`
	ThinkingBudget int              `yaml:"thinking_budget"`
}

// soulPermissions mirrors kinclaw's permissions block — the two share
// a rule grammar on purpose, so `bash(git push*)` means the same thing
// wherever it is written.
type soulPermissions struct {
	Mode  string   `yaml:"mode"`
	Ask   []string `yaml:"ask"`
	Allow []string `yaml:"allow"`
}

// soulBrain mirrors kinclaw's nested brain config. Provider + model
// are the meaningful fields; Endpoint lets ollama souls point at a
// non-default Ollama install (e.g. remote LAN box). ContextLength
// is parsed for kinclaw-soul fidelity but kincode doesn't enforce
// it client-side — providers expose their own context limits.
type soulBrain struct {
	Provider      string  `yaml:"provider"`
	Model         string  `yaml:"model"`
	Temperature   float64 `yaml:"temperature"`
	ContextLength int     `yaml:"context_length"`
	Endpoint      string  `yaml:"endpoint"`
}

// mcpConfigFile is the JSON structure for MCP server configuration.
type mcpConfigFile struct {
	MCPServers map[string]mcpServerConfig `json:"mcpServers"`
}

type mcpServerConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Env     []string `json:"env"`
}

// loadMCPServers reads the MCP config file, connects to each server, and
// registers their tools in the tool registry.
func loadMCPServers(path string, registry *tools.Registry) []*mcp.Client {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("Warning: cannot read MCP config %s: %v", path, err)
		return nil
	}

	var cfg mcpConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("Warning: invalid MCP config %s: %v", path, err)
		return nil
	}

	var clients []*mcp.Client
	for name, srv := range cfg.MCPServers {
		client, err := mcp.Connect(name, srv.Command, srv.Args, srv.Env)
		if err != nil {
			log.Printf("Warning: MCP server %q failed to connect: %v", name, err)
			continue
		}

		toolDefs, err := client.ListTools()
		if err != nil {
			log.Printf("Warning: MCP server %q failed to list tools: %v", name, err)
			client.Close()
			continue
		}

		// Register MCP tools in the tool registry.
		mcpTools := mcp.ToolsFromClient(client)
		for _, t := range mcpTools {
			registry.Register(t)
		}

		log.Printf("MCP server %q connected: %d tools", name, len(toolDefs))
		clients = append(clients, client)
	}

	return clients
}

func loadSoulFile(path string) (string, *soulFrontmatter, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read soul file: %w", err)
	}

	content := string(data)

	// Parse YAML frontmatter if present.
	if strings.HasPrefix(content, "---") {
		parts := strings.SplitN(content[3:], "---", 2)
		if len(parts) == 2 {
			var fm soulFrontmatter
			if err := yaml.Unmarshal([]byte(parts[0]), &fm); err != nil {
				return "", nil, fmt.Errorf("parse frontmatter: %w", err)
			}

			body := strings.TrimSpace(parts[1])

			// Build system prompt from frontmatter + body.
			var sb strings.Builder
			if fm.Name != "" {
				sb.WriteString(fmt.Sprintf("You are %s.\n\n", fm.Name))
			}
			if len(fm.Rules) > 0 {
				sb.WriteString("Rules:\n")
				for _, rule := range fm.Rules {
					sb.WriteString(fmt.Sprintf("- %s\n", rule))
				}
				sb.WriteString("\n")
			}
			sb.WriteString(body)
			return sb.String(), &fm, nil
		}
	}

	// No frontmatter — use the whole file as the system prompt.
	return strings.TrimSpace(content), nil, nil
}
