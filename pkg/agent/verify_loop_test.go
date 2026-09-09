package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LocalKinAI/kincode/pkg/permission"
	"github.com/LocalKinAI/kincode/pkg/provider"
	"github.com/LocalKinAI/kincode/pkg/tools"
)

// scriptedProvider plays a fixed list of responses, one per round, so a
// tool round can be driven without a model.
type scriptedProvider struct {
	responses []*provider.Response
	round     int
}

func (s *scriptedProvider) next() *provider.Response {
	if s.round >= len(s.responses) {
		return &provider.Response{Content: "done"}
	}
	r := s.responses[s.round]
	s.round++
	return r
}

func (s *scriptedProvider) Chat(context.Context, []provider.Message, []provider.ToolDef) (*provider.Response, error) {
	return s.next(), nil
}

func (s *scriptedProvider) Stream(_ context.Context, _ []provider.Message, _ []provider.ToolDef, _ func(string)) (*provider.Response, error) {
	return s.next(), nil
}

func (s *scriptedProvider) Name() string { return "scripted" }

func editCall(id, path, oldStr, newStr string) provider.ToolCall {
	args, _ := json.Marshal(map[string]string{
		"file_path": path, "old_string": oldStr, "new_string": newStr,
	})
	tc := provider.ToolCall{ID: id}
	tc.Function.Name = "file_edit"
	tc.Function.Arguments = string(args)
	return tc
}

// goRepo writes a module that compiles, in a directory the test can
// chdir into — the verifier builds the process's working directory,
// which is where the server puts the user's repo.
func goRepo(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"go.mod":  "module vloop\n\ngo 1.21\n",
		"main.go": body,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return dir
}

// runEdit drives one round that edits main.go and returns the tool
// result the model would read.
func runEdit(t *testing.T, dir string, verify bool, oldStr, newStr string) string {
	t.Helper()
	reg := tools.NewRegistry()
	tools.RegisterDefaults(reg)
	a := New(Config{
		Provider: &scriptedProvider{responses: []*provider.Response{
			{Content: "editing", ToolCalls: []provider.ToolCall{
				editCall("t1", filepath.Join(dir, "main.go"), oldStr, newStr),
			}},
			{Content: "done"},
		}},
		Tools:       reg,
		Permissions: permission.New(true),
		Verify:      verify,
	})
	if _, _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, m := range a.messages {
		if m.Role == "tool" && m.ToolCallID == "t1" {
			return m.Content
		}
	}
	t.Fatal("no tool result for the edit")
	return ""
}

func TestBreakingTheBuildIsReportedInTheSameRound(t *testing.T) {
	dir := goRepo(t, "package main\n\nimport \"fmt\"\n\nfunc greet() string { return \"hi\" }\n\nfunc main() { fmt.Println(greet()) }\n")
	// Renaming the definition without its call site: exactly the
	// mistake an agent makes, and exactly what the model must be told
	// about before it says it is done.
	got := runEdit(t, dir, true, "func greet()", "func hello()")
	if !strings.Contains(got, "[verify] go FAILED") {
		t.Errorf("tool result did not carry the failure:\n%s", got)
	}
	if !strings.Contains(got, "greet") {
		t.Errorf("the compiler's own words should reach the model:\n%s", got)
	}
	if !strings.Contains(got, "in this turn") {
		t.Errorf("the model should be told to fix it now:\n%s", got)
	}
}

func TestAGoodEditReportsOK(t *testing.T) {
	dir := goRepo(t, "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"a\") }\n")
	got := runEdit(t, dir, true, `"a"`, `"b"`)
	if !strings.Contains(got, "[verify] go: ok") {
		t.Errorf("a clean edit should say so:\n%s", got)
	}
}

func TestVerifyOffAddsNothing(t *testing.T) {
	dir := goRepo(t, "package main\n\nimport \"fmt\"\n\nfunc greet() string { return \"hi\" }\n\nfunc main() { fmt.Println(greet()) }\n")
	got := runEdit(t, dir, false, "func greet()", "func hello()")
	if strings.Contains(got, "[verify]") {
		t.Errorf("-no-verify should stay silent:\n%s", got)
	}
}
