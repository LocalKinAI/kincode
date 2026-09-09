package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKinBrowser puts a stand-in on PATH so the escalation can be
// tested without the network or the real binary.
func fakeKinBrowser(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kinbrowser")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestKinBrowserOutputIsUsedWhenAvailable(t *testing.T) {
	fence := "\x60\x60\x60"
	fakeKinBrowser(t, "echo '# Title'; echo; echo '"+fence+"go'; echo 'func main() {}'; echo '"+fence+"'")
	got, err := (&WebFetchTool{}).Execute(map[string]any{"url": "https://example.invalid/doc"})
	if err != nil {
		t.Fatal(err)
	}
	// The point of the whole change: code fences survive.
	if !strings.Contains(got, fence+"go") || !strings.Contains(got, "func main() {}") {
		t.Errorf("markdown structure was lost:\n%s", got)
	}
}

func TestFallsBackWhenKinBrowserIsMissing(t *testing.T) {
	// An empty PATH — its absence must not be an error, because the
	// old path still works, just worse.
	t.Setenv("PATH", t.TempDir())
	if _, ok := viaKinBrowser("https://example.invalid/x"); ok {
		t.Error("should report unavailable rather than succeeding")
	}
}

func TestFallsBackWhenKinBrowserFails(t *testing.T) {
	fakeKinBrowser(t, `exit 1`)
	if _, ok := viaKinBrowser("https://example.invalid/x"); ok {
		t.Error("a failing fetch should fall back, not propagate")
	}
}

func TestEmptyOutputCountsAsFailure(t *testing.T) {
	// A page that renders to nothing is not an answer; the plain HTTP
	// path may still get something.
	// `true` rather than `echo -n ""`: /bin/sh on this platform prints
	// a literal "-n", so the fake would not actually be silent.
	fakeKinBrowser(t, `true`)
	if _, ok := viaKinBrowser("https://example.invalid/x"); ok {
		t.Error("empty output should fall back")
	}
}

func TestMaxLengthStillApplies(t *testing.T) {
	fakeKinBrowser(t, `head -c 40000 /dev/zero | tr '\0' 'x'`)
	got, err := (&WebFetchTool{}).Execute(map[string]any{
		"url": "https://example.invalid/big", "max_length": float64(500),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "truncated") || len(got) > 1200 {
		t.Errorf("cap not applied: %d bytes", len(got))
	}
}

func TestRealBinaryIfInstalled(t *testing.T) {
	if _, err := exec.LookPath("kinbrowser"); err != nil {
		t.Skip("kinbrowser not installed")
	}
	// No network in the sandbox necessarily; just check the plumbing
	// reports cleanly rather than hanging or panicking.
	_, _ = viaKinBrowser("https://example.invalid/definitely-not-real")
}
