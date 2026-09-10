# kincode

A lightweight AI coding assistant for your terminal. Written in Go. Single binary. Zero dependencies.

Like Claude Code, but open-source and 10x lighter.

## Features

- Single binary (~10MB), zero runtime dependencies
- Multi-provider: Anthropic, OpenAI, Ollama (any OpenAI-compatible endpoint)
- **14 built-in tools**: bash, bash_output, file_read / file_write / file_edit / multi_edit, glob, grep, git, web_fetch, web_search, memory, agent_spawn, todo_write
- **Approval gate** — `permissions: {mode: ask, ask: [...], allow: [...]}` in the soul. A matching call stops for a human: a terminal prompt, or an approval card in [KinClaw Mac](https://github.com/LocalKinAI/kinclaw-mac). Same rule grammar as kinclaw, and a `bash(...)` allow rule is checked against every simple command the shell would run — `bash(go test*)` does not wave through `go test ./... > ~/.zshrc`
- **Builds after it edits** — a round that changed files is followed by `go build ./...` / `cargo check` / `tsc --noEmit` / `swift build`, and the compiler's answer goes into the tool result the model reads. The edit → verify → fix loop is mechanical rather than something the prompt asks for
- **Undo** — `POST /api/undo` takes back the last turn's file changes, and *only* those: your own uncommitted work is out of scope by construction, which `git checkout` cannot say
- **Reads the repo** — a read-only `git` tool (status / diff / log / show / blame, allowed in plan mode) plus branch, dirty files and recent commits in the system prompt, so a session does not open with four rounds of orientation
- **Background commands** — `bash(..., background: true)` for a dev server or a slow suite; `bash_output` follows its log. And `cd` carries into the next call, the way a terminal does it
- **Image input** — vision blocks for Anthropic claude-3+ and OpenAI gpt-4o. Send screenshots, mockups, diagrams via `POST /api/chat {message, images:[{media_type, data}]}`
- **Plan mode** — `POST /api/plan_mode {enabled}` toggles a read-only gate. Agent investigates + drafts a markdown plan, doesn't modify until you approve
- **Named subagents** — drop a `.md` file at `~/.kincode/agents/<name>.md` or `~/.localkin/agents/<name>.md` (family-shared) with YAML frontmatter; the parent model dispatches via `agent_spawn(agent="code-reviewer", task="...")`
- **External skill forge** — `~/.kincode/skills/` and `~/.localkin/skills/` SKILL.md files become regular tools at boot. Same format kinclaw uses, so the family shares one skill marketplace
- **Project memory** — auto-loads `KINCODE.md` (or `CLAUDE.md` for cross-tool portability) from any parent dir of cwd
- Permission system with tool call confirmation (or `-yolo` to skip)
- **Soul files** with `brain:` config — switch persona AND provider/model per soul, kinclaw-kernel-compatible format
- **`-serve` mode** — HTTP+SSE server for desktop shell integration (paired with [KinClaw Mac](https://github.com/LocalKinAI/kinclaw-mac) Code mode)
- Streaming responses with markdown rendering
- Context compaction: auto-summarizes when context gets large
- MCP support: connect any MCP-compatible tool server
- Persistent memory across sessions
- Web tools: fetch URLs and search the web (DuckDuckGo by default; set `TAVILY_API_KEY` to use Tavily for LLM-tuned results)
- Extended thinking: deep reasoning mode for complex problems
- Session persistence: auto-save/restore conversations across restarts
- Fast: Go concurrency, minimal memory footprint

## Quick Start

```bash
# Install
go install github.com/LocalKinAI/kincode/cmd/kincode@latest

# Or download binary
curl -fsSL https://github.com/LocalKinAI/kincode/releases/latest/download/kincode-$(uname -s)-$(uname -m) -o kincode
chmod +x kincode

# Run with Anthropic
export ANTHROPIC_API_KEY=sk-ant-...
kincode

# Run with Ollama (free, local)
kincode -provider ollama -model qwen3:8b

# Run with OpenAI
OPENAI_API_KEY=sk-... kincode -provider openai -model gpt-4o

# Run with a soul file
kincode -soul coder.soul.md

# One-shot mode (non-interactive)
kincode "explain this codebase"

# YOLO mode (auto-approve all tool calls)
kincode -yolo "fix the failing tests"
```

## Claude Login (No API Key Needed)

Use your Claude account directly — works with Free, Pro, and Max:

```bash
# First time: login via browser
kincode -login

# Then just use it (defaults to Haiku 4.5)
kincode

# Or specify a different model
kincode -model claude-sonnet-4-6
```

Your session auto-refreshes. No API key needed.

## Soul Files

Define custom personas with `.soul.md` files. Soul format is **compatible with the kinclaw kernel** — same file drives either:

```yaml
---
name: "kincode"
brain:
  provider: "ollama"          # anthropic | openai | ollama
  model: "kimi-k2.6:cloud"    # picks the brain when no -provider/-model on CLI
  temperature: 0.3
  context_length: 131072
permissions:                  # optional; absent means auto, as before
  mode: ask
  ask:   ["bash", "file_write", "file_edit", "multi_edit", "agent_spawn"]
  allow: ["bash(go test*)", "bash(git diff*)", "bash(ls*)"]
rules:
  - "Read before you write"
  - "Stdlib first, deps last resort"
  - "Don't apologize. Don't hedge."
---

You are kincode, a senior coding agent. Ship clean, correct, minimal code...
```

`permissions:` is the approval gate. `mode: ask` stops matching calls
for a human; `allow` beats `ask`, and read-only tools (`file_read`,
`glob`, `grep`, `git`) never ask. The rule grammar is kinclaw's — a
tool name, a `prefix*`, or `tool(param-prefix*)` — so `bash(git push*)`
means the same thing in both. A `bash(...)` rule is checked against
**every simple command the shell would run**, split on `;` `&&` `||`
`|` and newlines: without that, an allow-listed verb is a doorway for
whatever follows it. Redirects and substitutions are never covered by a
prefix rule. Irreversible commands reach a human even when no rule
matches. `-yolo` skips the gate entirely.

CLI flag (`-provider` / `-model`) > soul `brain:` > legacy top-level `model:` > hardcoded default. Pass `-soul` to load:

```bash
kincode -soul ~/Documents/Workspace/kincode/souls/coder.soul.md
```

## Server Mode

Run kincode as a daemon for desktop shell integration:

```bash
kincode -serve -port 5002 -soul souls/coder.soul.md
```

Exposes:

| Route | Method | Purpose |
|---|---|---|
| `/api/health` | GET | readiness probe |
| `/api/state` | GET | `{repo, model, provider, message_count}` |
| `/api/repo` | POST | chdir agent into a repo: `{"path": "..."}` |
| `/api/chat` | POST | kick a turn: `{"message": "..."}` (returns 202; output via SSE) |
| `/api/chat` | DELETE | interrupt the in-flight turn |
| `/api/events` | GET | SSE stream of `{type, ...}` events |

Event types: `user_message`, `text_delta`, `tool_call`, `tool_result`, `turn_done`, `error`, `usage`.

[KinClaw Mac](https://github.com/LocalKinAI/kinclaw-mac)'s **Code mode** drives kincode through this surface. Server mode forces `-yolo` (no permission loop over HTTP) and falls back to `ollama / kimi-k2.6:cloud` if no Anthropic creds are configured — same Ollama install kinclaw uses, no extra setup.

### Extended Thinking

Enable deep reasoning for complex problems:

```yaml
---
name: "Architect"
thinking: true
thinking_budget: 15000
---
```

When enabled, the model will show its reasoning process in dim text before the final response. Useful for complex debugging, architecture decisions, and multi-step analysis.

## Slash Commands

| Command | Description |
|---|---|
| `/help` | Show all commands |
| `/clear` | Clear conversation history |
| `/compact` | Manually compress context |
| `/model <name>` | Switch model mid-session |
| `/provider <name>` | Switch provider |
| `/soul <file>` | Load a soul file |
| `/memory` | Show persistent memory |
| `/save <file>` | Save conversation to file |
| `/load <file>` | Load conversation from file |
| `/tokens` | Show estimated token usage |
| `/diff` | Show last file edit as colored diff |
| `/mcp` | List connected MCP servers and tools |
| `/skill` | List available skill templates |
| `/skill <name>` | Load a skill template for next message |
| `/skill create <name>` | Create a new skill interactively |
| `/version` | Show version |
| `/quit` | Exit |

## Architecture

```
kincode (9MB single binary)
├── cmd/kincode/       # CLI entry point, flag parsing, soul loading
├── pkg/
│   ├── agent/          # Core loop: message → LLM → tool calls → loop
│   │                   # Context compaction (auto-summarize at 80%)
│   ├── provider/       # LLM providers (raw HTTP, no SDKs)
│   │   ├── anthropic   # Anthropic Messages API + SSE streaming
│   │   └── openai      # OpenAI-compatible (OpenAI/Ollama/DeepSeek/Gemini/...)
│   ├── tools/          # 10 built-in tools
│   │   ├── bash        # Shell execution (30s default, persistent cd, background tasks)
│   │   ├── git         # Read-only: status / diff / log / show / blame
│   │   ├── file_*      # Read, write, edit (with diff visualization)
│   │   ├── glob/grep   # File and content search
│   │   ├── web_*       # Fetch URLs, DuckDuckGo search
│   │   ├── memory      # Persistent key-value store
│   │   └── agent_spawn # Sub-agent for parallel tasks
│   ├── repl/           # Interactive terminal
│   │                   # Readline, markdown rendering, 14 slash commands
│   └── permission/     # Tool call approval (yolo / confirm)
└── internal/
    └── mcp/            # MCP protocol client (JSON-RPC 2.0 over stdio)
```

## Comparison

| | Claude Code | claw-code (Rust) | nano-claude-code (Python) | **kincode (Go)** |
|---|---|---|---|---|
| Binary size | ~100MB | ~15MB | N/A (needs Python) | **9MB** |
| Memory usage | ~150MB | ~30MB | ~80MB | **~20MB** |
| Dependencies | Node.js + npm | Rust toolchain | Python 3.10+ | **zero** |
| Install | npm install | cargo build | pip install | **download & run** |
| Providers | Anthropic | Multi | 10+ | **any OpenAI-compatible** |
| Built-in tools | 40+ | ~20 | 13 | **10 + MCP** |
| Sub-agents | ✅ | ❌ | ✅ | **✅** |
| Memory | ✅ | ❌ | ✅ | **✅** |
| Context compaction | ✅ | ✅ | ✅ | **✅** |
| MCP protocol | ✅ | ✅ | ❌ | **✅** |
| Markdown rendering | ✅ | ✅ | ❌ | **✅** |
| Diff visualization | ❌ | ❌ | ✅ | **✅** |
| Web search | ❌ | ❌ | ✅ | **✅** |
| Session persistence | ✅ | ✅ | ✅ | **✅** |
| Soul files | ❌ | ❌ | ❌ | **✅ unique** |
| Open source | ❌ | ✅ | ✅ | **✅** |

## MCP Support

Connect to any [MCP](https://modelcontextprotocol.io/)-compatible tool server:

```bash
# Create mcp.json
cat > mcp.json << 'EOF'
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
    },
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": ["GITHUB_TOKEN=ghp_xxx"]
    }
  }
}
EOF

# Run with MCP servers
kincode -mcp mcp.json

# List connected servers and tools
> /mcp
```

MCP tools are automatically registered with a `mcp_` prefix (e.g., `mcp_read_file`, `mcp_search_repositories`). The LLM can call them like any built-in tool.

## Build from Source

```bash
git clone https://github.com/LocalKinAI/kincode.git
cd kincode
go build -o kincode ./cmd/kincode/
```

## License

MIT

---

Built by the team behind [LocalKin](https://localkin.dev) -- a self-evolving AI agent swarm with 78 specialized agents.
