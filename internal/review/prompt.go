package review

import (
	"fmt"
	"strings"
)

const maxReviewMetadataBytes = 256

// OutputContract is the JSON shape the agent must end its message with.
const OutputContract = `{
  "summary": "one-paragraph PR review summary in markdown",
  "sequence_diagram": "Mermaid sequence diagram source with escaped newlines; no markdown fences",
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
- Paths must be clean, relative repository paths with no absolute, ".", or ".." segments.
- Every finding field must use its schema type; do not use null values.
- Report at most %d findings, most important first. If the diff is clean, return an empty findings array.
- Keep the summary under %d bytes, each finding body under %d bytes, and each path under %d bytes.
- Write finding bodies in markdown, concise and actionable, addressed to the PR author.
- Include one concise sequence diagram showing the important components and interactions changed by the PR.
- "sequence_diagram" must be a Mermaid sequence diagram source string. Start it with "sequenceDiagram",
  use JSON-escaped newline characters between lines, and do not wrap it in markdown fences. Keep it to 4–12 messages.

Your final message MUST end with a fenced JSON block exactly matching this
schema and nothing after it:

`+"```json\n%s\n```"+`

The "summary" field must be your full PR-level review in markdown: what the PR
does, overall verdict, and themes of the findings. Do not post any comments,
do not modify files, do not run git commands that change state. Output only.`,
		diffPath, maxFindings, maxSummaryBytes, maxFindingBodyBytes, maxFindingPathBytes, OutputContract)
}

// severityEmoji maps severities to a badge for inline bodies.
func severityBadge(sev string) string {
	severity, ok := normalizeSeverity(sev)
	if !ok {
		severity = "info"
	}
	switch severity {
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
	body := normalizeMarkdown(f.Body, maxFindingBodyBytes)
	return truncateUTF8(severityBadge(f.Severity)+"\n\n"+body, maxGitHubCommentBytes)
}

// GatewayName returns the human-readable gateway a review model runs on. It
// mirrors the runner's gateway rule for display: only orcarouter/… models
// leave the built-in OpenCode Zen provider.
func GatewayName(model string) string {
	if strings.HasPrefix(model, "orcarouter/") {
		return "OrcaRouter"
	}
	return "OpenCode Zen"
}

// RenderSummaryComment formats the PR-level summary comment.
func RenderSummaryComment(result ReviewResult, botName, model string) string {
	botName = normalizeMetadata(botName)
	model = normalizeMetadata(model)
	footer := "\n\n<sub>Reviewed by " + botName + " with `" + model +
		"` (" + GatewayName(model) + "). Trigger me by mentioning `@" + botName + "` in a comment.</sub>\n"

	var b strings.Builder
	b.WriteString(fmt.Sprintf("## 🤖 %s review\n\n", botName))
	b.WriteString(normalizeMarkdown(result.SummaryMD, maxSummaryBytes))

	b.WriteString("\n\n### Sequence diagram\n\n```mermaid\n")
	b.WriteString(normalizeSequenceDiagram(result.SequenceDiagram))
	b.WriteString("\n```\n\n")

	renderedFindings := 0
	for i, candidate := range result.Findings {
		if i >= maxFindingCandidates || renderedFindings >= maxFindings {
			break
		}
		f, ok := normalizeFinding(candidate)
		if !ok {
			continue
		}
		if renderedFindings == 0 {
			b.WriteString("### Findings\n\n")
		}
		finding := fmt.Sprintf("- `%s:%d` — %s\n", f.Path, f.Line, severityBadge(f.Severity))
		if b.Len()+len(finding)+len("\nIndividual findings are posted as inline comments on the changed lines below.")+len(footer) > maxGitHubCommentBytes {
			break
		}
		b.WriteString(finding)
		renderedFindings++
	}
	if renderedFindings > 0 {
		b.WriteString("\nIndividual findings are posted as inline comments on the changed lines below.")
	} else {
		b.WriteString("✅ No inline findings were reported.")
	}

	b.WriteString(footer)
	return truncateUTF8(b.String(), maxGitHubCommentBytes)
}

func normalizeMetadata(value string) string {
	value = normalizeMarkdown(value, maxReviewMetadataBytes)
	value = strings.NewReplacer("\n", " ", "\t", " ").Replace(value)
	return strings.TrimSpace(value)
}
