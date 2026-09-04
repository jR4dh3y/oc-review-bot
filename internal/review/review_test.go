package review

import (
	"strings"
	"testing"

	"github.com/jR4dh3y/oc-review-bot/internal/gh"
)

const sampleAgentOutput = `I reviewed the pull request.

The change adds a cache layer. Overall solid, but there is an unhandled error
and a missing test.

` + "```json" + `
{
  "summary": "Adds a cache layer with decent structure.",
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

func TestExtractReviewNoContract(t *testing.T) {
	out := "The PR looks great overall, ship it.\nSecond line."
	r := ExtractReview(out)
	if r.SummaryMD != out || len(r.Findings) != 0 {
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

func TestRenderSummaryComment(t *testing.T) {
	r := ExtractReview(sampleAgentOutput)
	out := RenderSummaryComment(r, "oc-review-bot", "opencode/big-pickle")

	for _, want := range []string{
		"## 🤖 oc-review-bot review",
		"internal/cache/cache.go:42",
		"🟠 **warning**",
		"@oc-review-bot",
		"opencode/big-pickle",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary comment missing %q:\n%s", want, out)
		}
	}
}

func TestRenderInlineBody(t *testing.T) {
	got := RenderInlineBody(Finding{Severity: "critical", Body: "SQL injection here"})
	if !strings.HasPrefix(got, "🔴 **critical**\n\nSQL injection") {
		t.Fatalf("inline body = %q", got)
	}
}
