package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// kinfer answers a stream request that carries tools with one plain JSON
// body — it buffers the reply so a tool call never arrives in fragments.
// Read as SSE that body is a line without "data: ", and the turn used to
// end with no text and no tool calls.
func TestOpenAIStreamAnsweredWithPlainJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"finish_reason":"tool_calls","index":0,"message":{"content":"checking","role":"assistant","tool_calls":[{"function":{"arguments":"{\"location\":\"Tokyo\"}","name":"weather"},"id":"call_1","type":"function"}]}}],"object":"chat.completion","usage":{"prompt_tokens":12,"completion_tokens":5}}`))
	}))
	defer srv.Close()

	var streamed string
	p := NewOpenAI("", "m", srv.URL)
	resp, err := p.Stream(context.Background(), []Message{{Role: "user", Content: "weather in Tokyo?"}}, nil,
		func(s string) { streamed += s })
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want one", resp.ToolCalls)
	}
	if tc := resp.ToolCalls[0]; tc.ID != "call_1" || tc.Function.Name != "weather" || tc.Function.Arguments != `{"location":"Tokyo"}` {
		t.Errorf("tool call = %+v", tc)
	}
	if resp.Content != "checking" || streamed != "checking" {
		t.Errorf("content = %q, streamed = %q, want both %q", resp.Content, streamed, "checking")
	}
	if resp.StopReason != "tool_calls" || resp.Usage.Input != 12 || resp.Usage.Output != 5 {
		t.Errorf("stop = %q, usage = %+v", resp.StopReason, resp.Usage)
	}
}

func TestOpenAIStreamStillReadsSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	var streamed string
	p := NewOpenAI("", "m", srv.URL)
	resp, err := p.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil,
		func(s string) { streamed += s })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello" || streamed != "hello" || resp.StopReason != "stop" {
		t.Errorf("content = %q, streamed = %q, stop = %q", resp.Content, streamed, resp.StopReason)
	}
}
