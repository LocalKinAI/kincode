package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/LocalKinAI/kincode/pkg/provider"
)

const (
	webFetchTimeout    = 15 * time.Second
	webFetchUserAgent  = "kincode/1.0"
	webFetchMaxDefault = 10000
)

// WebFetchTool fetches a URL and returns readable content.
//
// Reading documentation is most of what a coding agent uses the web
// for, and the thing that makes docs readable is structure: which part
// is prose, which part is the code you are supposed to copy. Stripping
// tags with a regex — the fallback below — loses exactly that. Fetched
// that way, pkg.go.dev's os/exec page arrives as 30KB opening with the
// site navigation and not one code fence in it; through kinbrowser it
// is 16KB that starts at "Package exec runs external commands" with
// its sixteen examples still marked as code.
//
// So kinbrowser first when it is installed: a separate LocalKinAI
// binary that escalates HTTP+readability → Lightpanda → headless
// Chrome and returns markdown, which also means JS-rendered docs
// (increasingly most of them) come back as content rather than an
// empty shell. Shelling out rather than importing keeps kincode a
// self-contained binary with no new module dependencies, and its
// absence is not an error — the old path still works, just worse.
type WebFetchTool struct{}

func (w *WebFetchTool) Name() string { return "web_fetch" }

func (w *WebFetchTool) Description() string {
	return "Fetch a URL and return its main content — markdown with code blocks intact when " +
		"kinbrowser is installed (it also renders JS-heavy pages), plain text otherwise. " +
		"Navigation, ads and scripts are stripped either way."
}

func (w *WebFetchTool) Def() provider.ToolDef {
	return provider.NewToolDef("web_fetch", w.Description(), map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The URL to fetch",
			},
			"max_length": map[string]any{
				"type":        "integer",
				"description": "Maximum content length in characters (default 10000)",
			},
		},
		"required": []string{"url"},
	})
}

// stripHTMLTags removes HTML tags and decodes common entities.
var htmlTagRe = regexp.MustCompile(`<[^>]*>`)
var htmlSpaceRe = regexp.MustCompile(`\s{3,}`)

func stripHTMLTags(s string) string {
	// Remove script and style blocks.
	scriptRe := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	s = scriptRe.ReplaceAllString(s, "")
	styleRe := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	s = styleRe.ReplaceAllString(s, "")

	// Remove tags.
	s = htmlTagRe.ReplaceAllString(s, " ")

	// Decode common HTML entities.
	replacer := strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&nbsp;", " ",
	)
	s = replacer.Replace(s)

	// Collapse whitespace.
	s = htmlSpaceRe.ReplaceAllString(s, "\n\n")
	s = strings.TrimSpace(s)

	return s
}

func (w *WebFetchTool) Execute(args map[string]any) (string, error) {
	url, ok := args["url"].(string)
	if !ok || url == "" {
		return "", fmt.Errorf("url is required")
	}

	maxLength := webFetchMaxDefault
	if ml, ok := args["max_length"].(float64); ok && ml > 0 {
		maxLength = int(ml)
	}

	// kinbrowser when it is there. Its own escalation and timeouts are
	// better than anything repeated here, so it gets a longer leash
	// than the plain HTTP path.
	if content, ok := viaKinBrowser(url); ok {
		if len(content) > maxLength {
			content = content[:maxLength] + "\n... (truncated)"
		}
		return fmt.Sprintf("---BEGIN WEB CONTENT---\n%s\n---END WEB CONTENT---", content), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), webFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", webFetchUserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch url: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// Read body with a cap to avoid huge downloads.
	limited := io.LimitReader(resp.Body, int64(maxLength*4)) // allow extra for HTML tags
	body, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	content := stripHTMLTags(string(body))

	// Truncate to max length.
	if len(content) > maxLength {
		content = content[:maxLength] + "\n... (truncated)"
	}

	return fmt.Sprintf("---BEGIN WEB CONTENT---\n%s\n---END WEB CONTENT---", content), nil
}

// kinBrowserTimeout covers the whole escalation chain — HTTP first,
// then a JS engine, then headless Chrome — so it is generous compared
// with the plain fetch. Past this the page is not worth the wait.
const kinBrowserTimeout = 45 * time.Second

// viaKinBrowser returns the page as markdown, and false when
// kinbrowser is not installed or could not read the URL — in which
// case the caller falls back rather than failing, because a worse
// answer beats no answer.
func viaKinBrowser(url string) (string, bool) {
	bin, err := exec.LookPath("kinbrowser")
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), kinBrowserTimeout)
	defer cancel()
	// --quiet keeps its layer/timing chatter off stderr, which would
	// otherwise read to the model as part of the page.
	cmd := exec.CommandContext(ctx, bin, "open", "--quiet", url)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", false
	}
	content := strings.TrimSpace(out.String())
	if content == "" {
		return "", false
	}
	return content, true
}
