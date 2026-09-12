package review

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jR4dh3y/samik-bot/internal/gh"
)

const sampleAgentOutput = `I reviewed the pull request.

The change adds a cache layer. Overall solid, but there is an unhandled error
and a missing test.

` + "```json" + `
{
  "summary": "Adds a cache layer with decent structure.",
  "sequence_diagram": "sequenceDiagram\n    participant API\n    participant Cache\n    API->>Cache: Get value\n    Cache-->>API: Return value",
  "findings": [
    {"path": "internal/cache/cache.go", "line": 42, "side": "RIGHT", "severity": "warning",
     "body": "Get ignores the decode error."},
    {"path": "internal/cache/cache.go", "line": 30, "side": "LEFT", "severity": "info",
     "body": "Removed the old mutex; the new one is equivalent."},
    {"path": "", "line": 1, "severity": "info", "body": "malformed, dropped"},
    {"path": "internal/cache/cache.go", "line": 0, "severity": "info", "body": "malformed, dropped"}
  ]
}
` + "```" + `

Trailing chatter that must be ignored.`

func TestExtractReviewContract(t *testing.T) {
	r := ExtractReview(sampleAgentOutput)

	if !strings.HasPrefix(r.SummaryMD, "Adds a cache layer") {
		t.Fatalf("summary = %q", r.SummaryMD)
	}
	if !strings.Contains(r.SequenceDiagram, "API->>Cache: Get value") {
		t.Fatalf("sequence diagram = %q", r.SequenceDiagram)
	}
	if len(r.Findings) != 2 {
		t.Fatalf("want 2 valid findings, got %d: %+v", len(r.Findings), r.Findings)
	}
	if r.Findings[0].Side != "RIGHT" || r.Findings[1].Side != "LEFT" {
		t.Fatalf("sides wrong: %+v", r.Findings)
	}
}

func TestExtractReviewDefaultSide(t *testing.T) {
	out := "text\n" + "```json" + `
{"summary": "s", "findings": [{"path": "a.go", "line": 3, "body": "b"}]}
` + "```"
	r := ExtractReview(out)
	if len(r.Findings) != 1 || r.Findings[0].Side != "RIGHT" || r.Findings[0].Severity != "info" {
		t.Fatalf("defaults not applied: %+v", r.Findings)
	}
}

func TestExtractReviewNormalizesAndAllowlistsFindingFields(t *testing.T) {
	out := fencedJSON(t, map[string]any{
		"summary": "s",
		"findings": []any{
			map[string]any{"path": "a.go", "line": 3, "side": " right ", "severity": " WARNING ", "body": "valid"},
			map[string]any{"path": "b.go", "line": 4, "side": "MIDDLE", "severity": "info", "body": "invalid side"},
			map[string]any{"path": "c.go", "line": 5, "side": "LEFT", "severity": "urgent", "body": "invalid severity"},
		},
	})

	r := ExtractReview(out)
	if len(r.Findings) != 1 {
		t.Fatalf("want one allowlisted finding, got %+v", r.Findings)
	}
	if got := r.Findings[0]; got.Side != "RIGHT" || got.Severity != "warning" {
		t.Fatalf("finding fields not normalized: %+v", got)
	}
}

func TestExtractReviewRejectsNullAndUnsafeFindingFields(t *testing.T) {
	out := fencedJSON(t, map[string]any{
		"summary": "s",
		"findings": []any{
			map[string]any{"path": "valid.go", "line": 1, "side": "RIGHT", "severity": "info", "body": "valid"},
			map[string]any{"path": "null-side.go", "line": 2, "side": nil, "severity": "info", "body": "invalid"},
			map[string]any{"path": "null-severity.go", "line": 3, "side": "RIGHT", "severity": nil, "body": "invalid"},
			map[string]any{"path": "wrong-side.go", "line": 4, "side": []string{"RIGHT"}, "severity": "info", "body": "invalid"},
			map[string]any{"path": "wrong-line.go", "line": 5.5, "side": "RIGHT", "severity": "info", "body": "invalid"},
			map[string]any{"path": "control-body.go", "line": 6, "side": "RIGHT", "severity": "info", "body": "unsafe\x00body"},
		},
	})

	r := ExtractReview(out)
	if len(r.Findings) != 1 || r.Findings[0].Path != "valid.go" {
		t.Fatalf("malformed findings were retained: %+v", r.Findings)
	}
}

func TestExtractReviewDropsMalformedAndUnsafeFindings(t *testing.T) {
	out := fencedJSON(t, map[string]any{
		"summary": "s",
		"findings": []any{
			map[string]any{"path": "good.go", "line": 1, "side": "RIGHT", "severity": "info", "body": "valid"},
			map[string]any{"path": "wrong-type.go", "line": "2", "side": "RIGHT", "severity": "info", "body": "malformed"},
			"not a finding",
			map[string]any{"path": "../escape.go", "line": 3, "side": "RIGHT", "severity": "info", "body": "unsafe path"},
			map[string]any{"path": "/absolute.go", "line": 4, "side": "RIGHT", "severity": "info", "body": "unsafe path"},
			map[string]any{"path": "nested/../file.go", "line": 5, "side": "RIGHT", "severity": "info", "body": "unsafe path"},
			map[string]any{"path": "negative.go", "line": -1, "side": "RIGHT", "severity": "info", "body": "bad line"},
			map[string]any{"path": "empty.go", "line": 6, "side": "RIGHT", "severity": "info", "body": "\u0000"},
		},
	})

	r := ExtractReview(out)
	if len(r.Findings) != 1 || r.Findings[0].Path != "good.go" {
		t.Fatalf("unsafe findings were retained: %+v", r.Findings)
	}
}

func TestExtractReviewBoundsUntrustedOutput(t *testing.T) {
	findings := []any{
		map[string]any{
			"path":     strings.Repeat("p", maxFindingPathBytes+1),
			"line":     1,
			"side":     "RIGHT",
			"severity": "info",
			"body":     "path is too long",
		},
	}
	for i := 0; i < maxFindings+2; i++ {
		findings = append(findings, map[string]any{
			"path":     "file.go",
			"line":     i + 1,
			"side":     "RIGHT",
			"severity": "info",
			"body":     strings.Repeat("b", maxFindingBodyBytes+128),
		})
	}
	out := fencedJSON(t, map[string]any{
		"summary":  strings.Repeat("s", maxSummaryBytes+128),
		"findings": findings,
	})

	r := ExtractReview(out)
	if len(r.SummaryMD) != maxSummaryBytes || !strings.HasSuffix(r.SummaryMD, truncationMarker) {
		t.Fatalf("summary was not safely bounded: len=%d summary=%q", len(r.SummaryMD), r.SummaryMD)
	}
	if len(r.Findings) != maxFindings {
		t.Fatalf("want %d bounded findings, got %d", maxFindings, len(r.Findings))
	}
	for _, finding := range r.Findings {
		if len(finding.Path) > maxFindingPathBytes || len(finding.Body) > maxFindingBodyBytes {
			t.Fatalf("finding exceeds output bounds: %+v", finding)
		}
		if !strings.HasSuffix(finding.Body, truncationMarker) {
			t.Fatalf("oversized body was not truncated: %q", finding.Body)
		}
	}
}

func TestExtractReviewNoContract(t *testing.T) {
	out := "The PR looks great overall, ship it.\nSecond line."
	r := ExtractReview(out)
	if r.SummaryMD != out || len(r.Findings) != 0 || !strings.HasPrefix(r.SequenceDiagram, "sequenceDiagram") {
		t.Fatalf("fallback failed: %+v", r)
	}
}

func TestExtractReviewLastBlockWins(t *testing.T) {
	out := "```json\n{\"summary\": \"old\"}\n```\nmiddle\n" +
		"```json\n{\"summary\": \"new\", \"findings\": []}\n```"
	r := ExtractReview(out)
	if r.SummaryMD != "new" {
		t.Fatalf("summary = %q, want new", r.SummaryMD)
	}
}

func TestExtractReviewIgnoresInvalidJSON(t *testing.T) {
	out := "```json\n{not json}\n```"
	r := ExtractReview(out)
	if r.SummaryMD != out {
		t.Fatalf("invalid block should fall back to raw text, got %q", r.SummaryMD)
	}
}

func TestExtractReviewIgnoresNonJSONFences(t *testing.T) {
	out := "```yaml\n{\"summary\": \"not a review\", \"findings\": []}\n```"
	r := ExtractReview(out)
	if r.SummaryMD != out || len(r.Findings) != 0 {
		t.Fatalf("non-JSON fence was accepted: %+v", r)
	}
}

func TestExtractReviewFindsJSONAfterOtherFences(t *testing.T) {
	out := "```yaml\nsummary: example\n```\n" +
		"```json\n{\"summary\": \"review\", \"findings\": []}\n```"
	r := ExtractReview(out)
	if r.SummaryMD != "review" || len(r.Findings) != 0 {
		t.Fatalf("JSON contract after another fence was missed: %+v", r)
	}
}

func sampleFiles() []gh.File {
	return []gh.File{
		{
			Filename: "internal/cache/cache.go",
			Patch: "@@ -10,7 +10,9 @@ func (c *Cache) Get(k string) string {\n" +
				" context line\n" +
				"-old line\n" +
				"+new line 1\n" +
				"+new line 2\n" +
				" more context\n",
		},
		{Filename: "deleted_only.go", Patch: "@@ -1,3 +0,0 @@\n-removed one\n-removed two\n"},
		{Filename: "renamed.txt"}, // no patch
	}
}

func TestDiffIndexMapping(t *testing.T) {
	idx := NewDiffIndex(sampleFiles())

	// New lines exist on RIGHT.
	if !idx.InDiff("internal/cache/cache.go", "RIGHT", 11) {
		t.Fatal("new line 11 should be in diff")
	}
	if !idx.InDiff("internal/cache/cache.go", "RIGHT", 12) {
		t.Fatal("new line 12 should be in diff")
	}
	// Context line 13 on both sides.
	if !idx.InDiff("internal/cache/cache.go", "RIGHT", 13) || !idx.InDiff("internal/cache/cache.go", "LEFT", 10) {
		t.Fatal("context lines should be mapped on both sides")
	}
	// Removed line exists on LEFT only.
	if !idx.InDiff("internal/cache/cache.go", "LEFT", 11) {
		t.Fatal("old line 11 should be in diff on LEFT")
	}
	if idx.InDiff("internal/cache/cache.go", "RIGHT", 11) && !idx.InDiff("internal/cache/cache.go", "RIGHT", 12) {
		t.Fatal("side mapping inconsistent")
	}
	// Deletion-only file has no RIGHT lines.
	if idx.InDiff("deleted_only.go", "RIGHT", 1) {
		t.Fatal("deleted file has no RIGHT lines")
	}
	if !idx.InDiff("deleted_only.go", "LEFT", 1) {
		t.Fatal("deleted line 1 should be on LEFT")
	}
	// Unknown file and out-of-range line.
	if idx.InDiff("nope.go", "RIGHT", 1) || idx.InDiff("internal/cache/cache.go", "RIGHT", 999) {
		t.Fatal("out-of-diff line reported as in diff")
	}
	if idx.InDiff("internal/cache/cache.go", "MIDDLE", 11) {
		t.Fatal("invalid side must not be treated as RIGHT")
	}
	if !idx.InDiff("internal/cache/cache.go", " right ", 11) {
		t.Fatal("normalized RIGHT side should be accepted")
	}
}

func TestHunkHeaderVariants(t *testing.T) {
	idx := NewDiffIndex([]gh.File{{Filename: "a.go", Patch: "@@ -3 +3,2 @@\n ctx\n+a\n+b\n"}})
	if !idx.InDiff("a.go", "RIGHT", 4) || !idx.InDiff("a.go", "RIGHT", 5) {
		t.Fatal("single-count hunk start misparsed")
	}
	if !idx.InDiff("a.go", "LEFT", 3) {
		t.Fatal("old side start misparsed")
	}
}

func TestDiffIndexRejectsUnsafeFilePaths(t *testing.T) {
	idx := NewDiffIndex([]gh.File{{
		Filename: "../outside.go",
		Patch:    "@@ -0,0 +1 @@\n+unsafe\n",
	}})
	if len(idx.files) != 0 {
		t.Fatalf("unsafe filename was indexed: %+v", idx.files)
	}
}

func TestRenderSummaryComment(t *testing.T) {
	r := ExtractReview(sampleAgentOutput)
	out := RenderSummaryComment(r, "samik-bot", "opencode/big-pickle")

	for _, want := range []string{
		"## 🤖 samik-bot review",
		"### Sequence diagram",
		"```mermaid",
		"API->>Cache: Get value",
		"internal/cache/cache.go:42",
		"🟠 **warning**",
		"Individual findings are posted as inline comments",
		"@samik-bot",
		"opencode/big-pickle",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary comment missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Get ignores the decode error") {
		t.Fatalf("summary should not duplicate individual finding bodies:\n%s", out)
	}
	if strings.Index(out, "Adds a cache layer") > strings.Index(out, "### Sequence diagram") {
		t.Fatal("summary should precede sequence diagram")
	}
}

func TestSummaryFooterNamesTheModelGateway(t *testing.T) {
	r := ExtractReview(sampleAgentOutput)
	zen := RenderSummaryComment(r, "samik-bot", "opencode/big-pickle")
	if !strings.Contains(zen, "(OpenCode Zen)") {
		t.Fatalf("Zen summary footer must name OpenCode Zen:\n%s", zen)
	}
	orca := RenderSummaryComment(r, "samik-bot", "orcarouter/auto")
	if !strings.Contains(orca, "(OrcaRouter)") || strings.Contains(orca, "OpenCode Zen") {
		t.Fatalf("OrcaRouter summary footer must name OrcaRouter only:\n%s", orca)
	}
	if GatewayName("orcarouter/auto") != "OrcaRouter" || GatewayName("openai/gpt-5") != "OpenCode Zen" {
		t.Fatalf("GatewayName rule = %q / %q", GatewayName("orcarouter/auto"), GatewayName("openai/gpt-5"))
	}
}

func TestNormalizeSequenceDiagramStripsFences(t *testing.T) {
	out := ExtractReview("```json\n{" +
		`"summary":"s","sequence_diagram":"` +
		"```mermaid\\nsequenceDiagram\\n    A->>B: Call\\n```" +
		`","findings":[]}` + "\n```")
	if want := "sequenceDiagram\n    A->>B: Call"; out.SequenceDiagram != want {
		t.Fatalf("diagram = %q, want %q", out.SequenceDiagram, want)
	}
}

func TestNormalizeSequenceDiagramFallsBackAndBoundsOutput(t *testing.T) {
	malformed := ExtractReview(fencedJSON(t, map[string]any{
		"summary":          "s",
		"sequence_diagram": "this is not Mermaid",
	}))
	if malformed.SequenceDiagram != fallbackSequenceDiagram {
		t.Fatalf("malformed diagram = %q, want fallback", malformed.SequenceDiagram)
	}

	oversized := ExtractReview(fencedJSON(t, map[string]any{
		"summary":          "s",
		"sequence_diagram": "sequenceDiagram\nA->>B: Call\n" + strings.Repeat("participant A\n", maxSequenceDiagramBytes),
	}))
	if len(oversized.SequenceDiagram) > maxSequenceDiagramBytes ||
		!strings.Contains(oversized.SequenceDiagram, "A->>B: Call") {
		t.Fatalf("oversized diagram was not safely bounded: len=%d, diagram=%q", len(oversized.SequenceDiagram), oversized.SequenceDiagram)
	}
}

func TestBuildPromptRequestsDiagram(t *testing.T) {
	prompt := BuildPrompt("review-diff.patch")
	for _, want := range []string{"sequence_diagram", "Mermaid sequence diagram", "JSON-escaped"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestRenderInlineBody(t *testing.T) {
	got := RenderInlineBody(Finding{Severity: "critical", Body: "SQL injection here"})
	if !strings.HasPrefix(got, "🔴 **critical**\n\nSQL injection") {
		t.Fatalf("inline body = %q", got)
	}
}

func TestRenderSummaryCommentDropsUnsafeFindings(t *testing.T) {
	result := ReviewResult{
		SummaryMD: "summary",
		Findings: []Finding{
			{Path: "valid.go", Line: 1, Side: "RIGHT", Severity: "info", Body: "valid"},
			{Path: "invalid-side.go", Line: 2, Side: "MIDDLE", Severity: "info", Body: "invalid"},
			{Path: "invalid-severity.go", Line: 3, Side: "RIGHT", Severity: "urgent", Body: "invalid"},
			{Path: "../invalid-path.go", Line: 4, Side: "RIGHT", Severity: "info", Body: "invalid"},
			{Path: "invalid-body.go", Line: 5, Side: "RIGHT", Severity: "info", Body: "unsafe\x00body"},
		},
	}

	out := RenderSummaryComment(result, "samik-bot", "model")
	if !strings.Contains(out, "valid.go:1") {
		t.Fatalf("valid finding missing: %q", out)
	}
	for _, path := range []string{"invalid-side.go", "invalid-severity.go", "invalid-path.go", "invalid-body.go"} {
		if strings.Contains(out, path) {
			t.Fatalf("unsafe finding was rendered for %q: %q", path, out)
		}
	}
}

func TestRenderedCommentsStayWithinGitHubLimit(t *testing.T) {
	findings := make([]Finding, maxFindings)
	for i := range findings {
		findings[i] = Finding{
			Path:     "file.go",
			Line:     int64(i + 1),
			Side:     "RIGHT",
			Severity: "info",
			Body:     strings.Repeat("x\n", maxFindingBodyBytes),
		}
	}
	result := ReviewResult{
		SummaryMD: strings.Repeat("s", maxSummaryBytes*2),
		Findings:  findings,
	}
	if got := RenderInlineBody(Finding{Body: strings.Repeat("x", maxGitHubCommentBytes)}); len(got) > maxGitHubCommentBytes {
		t.Fatalf("inline body exceeds GitHub limit: %d", len(got))
	}
	if got := RenderSummaryComment(result, "samik-bot", "model"); len(got) > maxGitHubCommentBytes {
		t.Fatalf("summary body exceeds GitHub limit: %d", len(got))
	} else if !strings.Contains(got, "Reviewed by samik-bot") {
		t.Fatalf("summary footer was truncated: %q", got)
	}
}

func fencedJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal review output: %v", err)
	}
	return "```json\n" + string(body) + "\n```"
}
