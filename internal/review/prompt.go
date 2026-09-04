package review

import (
	"fmt"
	"strings"
)

// OutputContract is the JSON shape the agent must end its message with.
const OutputContract = `{
  "summary": "one-paragraph PR review summary in markdown",
  "findings": [
    {
      "path": "relative/file/path.go",
      "line": 42,
      "side": "RIGHT",
      "severity": "warning",
      "body": "markdown explanation of the issue"
    }
  ]
}`

// BuildPrompt writes the review instructions sent to the agent.
func BuildPrompt(diffPath string) string {
	return fmt.Sprintf(`You are a strict but fair code reviewer. Review the pull request diff attached at %q.

You are in the repository checkout at the PR's head commit, so read surrounding
code, types, and tests before judging a hunk. Focus on real issues:
- bugs, incorrect logic, and unhandled errors
- security problems (injection, authz, secret handling)
- breaking API/behavior changes and missing test coverage for new behavior
- clear performance or concurrency mistakes

Rules:
- Do not comment on style, formatting, or naming unless they actively hurt readability.
- Only report findings on lines that appear in the diff.
- "side" is "RIGHT" for lines in the new file version (most findings), "LEFT" for removed lines.
- "line" is the line number in that file version, not the diff offset.
- severity is one of: "critical", "warning", "info", "praise".
- Report at most 10 findings, most important first. If the diff is clean, return an empty findings array.
- Write finding bodies in markdown, concise and actionable, addressed to the PR author.

Your final message MUST end with a fenced JSON block exactly matching this
schema and nothing after it:

`+"```json\n%s\n```"+`

The "summary" field must be your full PR-level review in markdown: what the PR
does, overall verdict, and themes of the findings. Do not post any comments,
do not modify files, do not run git commands that change state. Output only.`,
		diffPath, OutputContract)
}

// severityEmoji maps severities to a badge for inline bodies.
func severityBadge(sev string) string {
	switch strings.ToLower(sev) {
	case "critical":
		return "🔴 **critical**"
	case "warning":
		return "🟠 **warning**"
	case "praise":
		return "🟢 **praise**"
	default:
		return "🔵 **info**"
	}
}

// RenderInlineBody formats one finding for a GitHub inline comment.
func RenderInlineBody(f Finding) string {
	return severityBadge(f.Severity) + "\n\n" + f.Body
}

// RenderSummaryComment formats the PR-level summary comment.
func RenderSummaryComment(result ReviewResult, botName, model string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## 🤖 %s review\n\n", botName))
	b.WriteString(result.SummaryMD)

	if len(result.Findings) > 0 {
		b.WriteString("\n\n### Findings\n\n")
		for _, f := range result.Findings {
			loc := fmt.Sprintf("`%s:%d`", f.Path, f.Line)
			b.WriteString(fmt.Sprintf("- %s — %s\n", loc, severityBadge(strings.ToLower(f.Severity))))
			b.WriteString("  \n" + indentLines(f.Body, "  "))
		}
	}

	b.WriteString("\n\n<sub>Reviewed by " + botName + " with `" + model +
		"` (OpenCode Zen). Trigger me by mentioning `@" + botName + "` in a comment.</sub>\n")
	return b.String()
}

// indentLines prefixes every line of s with pad, so list items nest cleanly.
func indentLines(s, pad string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}
