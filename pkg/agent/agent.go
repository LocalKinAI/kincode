// Package agent implements the core agent loop: message -> provider -> tools -> loop.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/LocalKinAI/kincode/pkg/permission"
	"github.com/LocalKinAI/kincode/pkg/provider"
	"github.com/LocalKinAI/kincode/pkg/tools"
)

const defaultMaxRounds = 25

// Agent orchestrates the conversation between user, provider, and tools.
type Agent struct {
	provider     provider.Provider
	tools        *tools.Registry
	permissions  *permission.Manager
	messages     []provider.Message
	systemPrompt string
	maxRounds    int
}

// Config holds agent configuration.
type Config struct {
	Provider     provider.Provider
	Tools        *tools.Registry
	Permissions  *permission.Manager
	SystemPrompt string
	MaxRounds    int
}

// New creates a new Agent.
func New(cfg Config) *Agent {
	maxRounds := cfg.MaxRounds
	if maxRounds <= 0 {
		maxRounds = defaultMaxRounds
	}

	a := &Agent{
		provider:     cfg.Provider,
		tools:        cfg.Tools,
		permissions:  cfg.Permissions,
		systemPrompt: cfg.SystemPrompt,
		maxRounds:    maxRounds,
	}

	// Add system prompt as first message.
	if cfg.SystemPrompt != "" {
		a.messages = append(a.messages, provider.Message{
			Role:    "system",
			Content: cfg.SystemPrompt,
		})
	}

	return a
}

// Messages returns the current conversation messages (for external inspection).
func (a *Agent) Messages() []provider.Message {
	return a.messages
}

// Provider returns the agent's provider.
func (a *Agent) Provider() provider.Provider {
	return a.provider
}

// SystemPrompt returns the agent's system prompt.
func (a *Agent) SystemPrompt() string {
	return a.systemPrompt
}

// SetProvider replaces the agent's provider (for mid-session switching).
func (a *Agent) SetProvider(p provider.Provider) {
	a.provider = p
}

// SaveSession writes conversation to a JSON file.
func (a *Agent) SaveSession(path string) error {
	// Skip system prompt — it's regenerated on load.
	var msgs []provider.Message
	for _, m := range a.messages {
		if m.Role != "system" {
			msgs = append(msgs, m)
		}
	}
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// LoadSession reads conversation from a JSON file.
func (a *Agent) LoadSession(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var msgs []provider.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		return err
	}
	// Keep system prompt, append loaded messages.
	var system []provider.Message
	for _, m := range a.messages {
		if m.Role == "system" {
			system = append(system, m)
		}
	}
	a.messages = append(system, msgs...)
	return nil
}

// ClearMessages resets to just the system prompt.
func (a *Agent) ClearMessages() {
	var system []provider.Message
	for _, m := range a.messages {
		if m.Role == "system" {
			system = append(system, m)
		}
	}
	a.messages = system
}

// estimateTokens gives a rough token count (4 chars ~= 1 token).
func estimateTokens(messages []provider.Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content) / 4
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Arguments) / 4
		}
	}
	return total
}

// compactIfNeeded checks if messages exceed 80% of estimated token budget and compacts.
// Uses a rough budget of 100k tokens (typical context window).
func (a *Agent) compactIfNeeded(ctx context.Context) {
	const tokenBudget = 100000
	estimated := estimateTokens(a.messages)
	if estimated < int(float64(tokenBudget)*0.8) {
		return
	}

	// Keep system prompt and last 5 messages.
	var systemMsg *provider.Message
	start := 0
	if len(a.messages) > 0 && a.messages[0].Role == "system" {
		systemMsg = &a.messages[0]
		start = 1
	}

	rest := a.messages[start:]
	if len(rest) <= 5 {
		return // not enough to compact
	}

	older := rest[:len(rest)-5]
	recent := rest[len(rest)-5:]

	// Ask the LLM to summarize older messages.
	summaryReq := []provider.Message{
		{Role: "system", Content: "Summarize this conversation in 3 sentences. Preserve key file paths, decisions, and code changes."},
		{Role: "user", Content: formatMessages(older)},
	}

	resp, err := a.provider.Chat(ctx, summaryReq, nil)
	if err != nil {
		// If summary fails, just continue without compaction.
		return
	}

	// Rebuild messages.
	a.messages = nil
	if systemMsg != nil {
		a.messages = append(a.messages, *systemMsg)
	}
	a.messages = append(a.messages, provider.Message{
		Role:    "user",
		Content: "Previous conversation summary: " + resp.Content,
	})
	a.messages = append(a.messages, provider.Message{
		Role:    "assistant",
		Content: "Understood. I have the context from our previous conversation.",
	})
	a.messages = append(a.messages, recent...)
}

// Events bundles streaming hooks the agent calls during a turn. Any
// nil func is treated as a no-op. Used by both Run (terminal-style
// stdout sink) and RunWithEvents (server-style SSE sink) — same loop,
// different output surfaces.
type Events struct {
	// OnText fires for every streamed assistant token chunk.
	OnText func(chunk string)
	// OnToolCall fires when the model requests a tool, BEFORE execution.
	// Useful for the UI to render a "🔨 running bash: ls" placeholder
	// while the tool runs.
	OnToolCall func(id, name, summary string, args map[string]any)
	// OnToolResult fires after tool execution. err is non-nil only for
	// hard failures (parse errors, denied permission); tool stderr/non-
	// zero exit is folded into result so the model sees the same string
	// the user does.
	OnToolResult func(id, name, result string, err error)
	// OnAssistantDone fires once the model finishes a streaming response
	// (after each round). Lets the UI commit the assistant bubble and
	// reset for the next round.
	OnAssistantDone func(content string)
}

// noop returns a callback that does nothing — saves nil-checks at every
// call site.
func (e Events) text(s string) {
	if e.OnText != nil {
		e.OnText(s)
	}
}
func (e Events) toolCall(id, name, summary string, args map[string]any) {
	if e.OnToolCall != nil {
		e.OnToolCall(id, name, summary, args)
	}
}
func (e Events) toolResult(id, name, result string, err error) {
	if e.OnToolResult != nil {
		e.OnToolResult(id, name, result, err)
	}
}
func (e Events) assistantDone(content string) {
	if e.OnAssistantDone != nil {
		e.OnAssistantDone(content)
	}
}

// Run processes a user message through the agent loop, streaming
// output to the terminal. Convenience wrapper around RunWithEvents
// for REPL/CLI use.
func (a *Agent) Run(ctx context.Context, userMessage string) (string, *provider.Usage, error) {
	return a.RunWithEvents(ctx, userMessage, Events{
		OnText: func(chunk string) { fmt.Print(chunk) },
		OnToolCall: func(_, name, summary string, _ map[string]any) {
			fmt.Fprintf(os.Stderr, "\033[2m[%s] %s\033[0m\n", name, summary)
		},
		OnAssistantDone: func(content string) {
			if content != "" {
				fmt.Println() // newline after streaming
			}
		},
	})
}

// Attachment is one image (or future media kind) attached to a user
// message. MediaType ∈ {"image/png", "image/jpeg", "image/gif",
// "image/webp"} — the four formats both Anthropic and OpenAI vision
// accept. Base64 is the raw base64 string (no data: URL prefix).
type Attachment struct {
	MediaType string
	Base64    string
}

// RunWithEvents is the same loop as Run but routes streaming output
// through a caller-supplied Events sink. The HTTP server uses this
// to fan tokens + tool calls into SSE; tests use it to assert against
// captured events. Tool permissions still go through the configured
// permission.Manager — pass permission.New(true) (yolo) for
// non-interactive callers like the server.
func (a *Agent) RunWithEvents(ctx context.Context, userMessage string, ev Events) (string, *provider.Usage, error) {
	return a.RunWithImagesAndEvents(ctx, userMessage, nil, ev)
}

// Permissions exposes the manager so the server can attach an Asker
// and switch the gate's mode once it exists — the agent is built
// before the server is.
func (a *Agent) Permissions() *permission.Manager { return a.permissions }

// SetPlanMode toggles plan mode on the underlying permission manager.
// Lets the server flip the gate live (POST /api/plan_mode) without
// reconstructing the agent.
func (a *Agent) SetPlanMode(enabled bool) {
	if a.permissions != nil {
		a.permissions.SetPlanMode(enabled)
	}
}

// PlanMode reports the current plan-mode state. Used by /api/state.
func (a *Agent) PlanMode() bool {
	if a.permissions == nil {
		return false
	}
	return a.permissions.PlanMode()
}

// planModeDirective is prepended to every user message while plan
// mode is on. It teaches the model the rules in addition to the
// hard enforcement at CheckPlanMode time — a model that only sees
// "tool denied" errors might thrash; a model that sees the directive
// up front knows to plan rather than execute.
const planModeDirective = "[PLAN MODE — read-only tools only " +
	"(file_read, glob, grep, web_fetch, web_search). " +
	"Investigate as needed, then end your response with a clear " +
	"markdown plan for the user to approve. Do NOT modify files, " +
	"run bash, or spawn sub-agents.]\n\n"

// RunWithImagesAndEvents is the multimodal version of RunWithEvents:
// the user message can include attached images. Each Attachment is
// translated to a provider.ContentBlock and the message is sent as
// a multimodal user turn. With no images, the resulting message is
// identical to the text-only path.
//
// Image-bearing turns work on Anthropic claude-3+ models and OpenAI
// gpt-4o / gpt-4-vision. Non-vision models will reject the request
// at the provider level — kincode doesn't pre-validate the model
// since the list of vision-capable models is moving target.
func (a *Agent) RunWithImagesAndEvents(ctx context.Context, userMessage string, images []Attachment, ev Events) (string, *provider.Usage, error) {
	// Plan-mode prepend. Done at message-construction time (not as
	// a separate system message) so the directive ages out naturally
	// with conversation compaction — same way Claude Code does it.
	if a.PlanMode() {
		userMessage = planModeDirective + userMessage
	}

	userMsg := provider.Message{
		Role:    "user",
		Content: userMessage,
	}
	if len(images) > 0 {
		userMsg.Blocks = make([]provider.ContentBlock, 0, len(images))
		for _, img := range images {
			userMsg.Blocks = append(userMsg.Blocks,
				provider.ImageBlock(img.MediaType, img.Base64))
		}
	}
	a.messages = append(a.messages, userMsg)

	totalUsage := &provider.Usage{}

	for round := 0; round < a.maxRounds; round++ {
		// Compact if context is getting large.
		a.compactIfNeeded(ctx)

		toolDefs := a.tools.Defs()

		resp, err := a.provider.Stream(ctx, a.messages, toolDefs, ev.text)
		if err != nil {
			return "", totalUsage, fmt.Errorf("provider error: %w", err)
		}

		totalUsage.Input += resp.Usage.Input
		totalUsage.Output += resp.Usage.Output

		// Add assistant response to history.
		assistantMsg := provider.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		a.messages = append(a.messages, assistantMsg)

		ev.assistantDone(resp.Content)

		// If no tool calls, we're done.
		if len(resp.ToolCalls) == 0 {
			return resp.Content, totalUsage, nil
		}

		// Execute tool calls.
		for _, tc := range resp.ToolCalls {
			args := parseToolArgs(tc)
			summary := toolSummary(tc.Function.Name, args)
			ev.toolCall(tc.ID, tc.Function.Name, summary, args)

			result, execErr := a.executeTool(ctx, tc)
			if execErr != nil {
				result = fmt.Sprintf("Error: %s", execErr)
			}
			ev.toolResult(tc.ID, tc.Function.Name, result, execErr)

			// Add tool result to messages.
			a.messages = append(a.messages, provider.Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
	}

	return "", totalUsage, fmt.Errorf("reached max rounds (%d) without completing", a.maxRounds)
}

// parseToolArgs decodes a tool call's JSON arguments into a map.
// Returns an empty map on parse error so the summary code can still
// run (it just produces "" instead of a useful label).
func parseToolArgs(tc provider.ToolCall) map[string]any {
	var args map[string]any
	_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
	return args
}

func (a *Agent) executeTool(ctx context.Context, tc provider.ToolCall) (string, error) {
	tool, err := a.tools.Get(tc.Function.Name)
	if err != nil {
		return "", err
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("parse tool arguments: %w", err)
	}

	// Plan mode gate: when on, deny non-read-only tools with an
	// error shaped to teach the model to pivot to a read-only tool
	// or finish with a markdown plan. Returning the error as a tool
	// result (vs. a hard agent error) lets the model see the denial
	// and self-correct in the same turn.
	if err := a.permissions.CheckPlanMode(tc.Function.Name); err != nil {
		return "", err
	}

	// Permission check for bash commands.
	if tc.Function.Name == "bash" {
		if cmd, ok := args["command"].(string); ok {
			if err := a.permissions.CheckBash(cmd); err != nil {
				return "", err
			}
		}
	}

	// The approval gate. Its refusal text is written for the model —
	// it becomes the tool result, so the model adapts instead of
	// retrying the same call into the same wall.
	if ok, reason := a.permissions.Check(ctx, tc.Function.Name, stringParams(args)); !ok {
		if reason == "" {
			reason = "Tool call denied by user."
		}
		return reason, nil
	}

	// Display side: callers (Run, RunWithEvents) emit a tool_call event
	// BEFORE invoking executeTool, so we don't print here.

	result, err := tool.Execute(args)
	if err != nil {
		return fmt.Sprintf("Tool error: %s\nOutput:\n%s", err, result), nil
	}

	return result, nil
}

// stringParams flattens a tool's arguments for the gate and for the
// approval card. Values a person cannot read at a glance (a whole file
// body) are truncated here rather than in the UI, so every surface
// shows the same thing.
func stringParams(args map[string]any) map[string]string {
	out := make(map[string]string, len(args))
	for k, v := range args {
		s, ok := v.(string)
		if !ok {
			s = fmt.Sprintf("%v", v)
		}
		if len(s) > 400 {
			s = s[:400] + "…"
		}
		out[k] = s
	}
	return out
}

func toolSummary(name string, args map[string]any) string {
	switch name {
	case "bash":
		if cmd, ok := args["command"].(string); ok {
			if len(cmd) > 80 {
				return cmd[:80] + "..."
			}
			return cmd
		}
	case "file_read":
		if fp, ok := args["file_path"].(string); ok {
			return fp
		}
	case "file_write":
		if fp, ok := args["file_path"].(string); ok {
			return fp
		}
	case "file_edit":
		if fp, ok := args["file_path"].(string); ok {
			return fp
		}
	case "glob":
		if p, ok := args["pattern"].(string); ok {
			return p
		}
	case "grep":
		if p, ok := args["pattern"].(string); ok {
			parts := []string{p}
			if path, ok := args["path"].(string); ok {
				parts = append(parts, "in "+path)
			}
			return strings.Join(parts, " ")
		}
	case "web_fetch":
		if u, ok := args["url"].(string); ok {
			return u
		}
	case "web_search":
		if q, ok := args["query"].(string); ok {
			return q
		}
	case "memory":
		if a, ok := args["action"].(string); ok {
			if k, ok := args["key"].(string); ok {
				return a + " " + k
			}
			return a
		}
	case "agent_spawn":
		if t, ok := args["task"].(string); ok {
			if len(t) > 80 {
				return t[:80] + "..."
			}
			return t
		}
	}
	return ""
}

// Clear resets the conversation history, keeping the system prompt.
func (a *Agent) Clear() {
	var msgs []provider.Message
	if a.systemPrompt != "" {
		msgs = append(msgs, provider.Message{
			Role:    "system",
			Content: a.systemPrompt,
		})
	}
	a.messages = msgs
}

// Compact summarizes the conversation to reduce context size.
func (a *Agent) Compact(ctx context.Context) error {
	if len(a.messages) < 4 {
		return nil
	}

	// Keep system prompt and ask provider to summarize.
	summaryReq := []provider.Message{
		{Role: "system", Content: "Summarize the following conversation concisely, preserving key decisions, file paths, and code changes. Be brief."},
		{Role: "user", Content: fmt.Sprintf("Summarize this conversation:\n\n%s", formatMessages(a.messages))},
	}

	resp, err := a.provider.Chat(ctx, summaryReq, nil)
	if err != nil {
		return fmt.Errorf("compact failed: %w", err)
	}

	// Reset with system prompt + summary.
	a.messages = nil
	if a.systemPrompt != "" {
		a.messages = append(a.messages, provider.Message{
			Role:    "system",
			Content: a.systemPrompt,
		})
	}
	a.messages = append(a.messages, provider.Message{
		Role:    "user",
		Content: "[Previous conversation summary]\n" + resp.Content,
	})
	a.messages = append(a.messages, provider.Message{
		Role:    "assistant",
		Content: "Understood. I have the context from our previous conversation. How can I help?",
	})

	return nil
}

func formatMessages(msgs []provider.Message) string {
	var parts []string
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		parts = append(parts, fmt.Sprintf("[%s]: %s", m.Role, m.Content))
	}
	return strings.Join(parts, "\n\n")
}
