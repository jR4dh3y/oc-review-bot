// Package review turns agent output into GitHub review comments.
package review

import (
	"bytes"
	"encoding/json"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// Keep model-generated fields below GitHub's comment size limit, with room
	// reserved for rendered metadata. Byte caps are conservative for UTF-8 text.
	maxGitHubCommentBytes = 65_536
	maxAgentTextBytes     = 1 << 20
	maxJSONBlockBytes     = 256 << 10
	maxSummaryBytes       = 8 << 10
	maxFindingBodyBytes   = 4 << 10
	maxFindingPathBytes   = 4 << 10
	maxFindings           = 10
	maxFindingCandidates  = 100
	maxReviewLine         = 1_000_000_000
	truncationMarker      = "…"
)

// Finding is one inline review comment requested by the agent.
type Finding struct {
	Path     string `json:"path"`
	Line     int64  `json:"line"`
	Side     string `json:"side"`
	Severity string `json:"severity"`
	Body     string `json:"body"`
}

// ReviewResult is the structured review extracted from the agent's final
// message: a PR-level summary plus optional line findings.
type ReviewResult struct {
	SummaryMD string
	Findings  []Finding
}

type rawReview struct {
	Summary  json.RawMessage `json:"summary"`
	Findings json.RawMessage `json:"findings"`
}

// ExtractReview parses the agent's final message. It looks for the last
// fenced JSON block matching the agreed contract; when the agent ignored the
// contract, the whole message becomes the summary with no inline findings.
func ExtractReview(agentText string) ReviewResult {
	if r, ok := parseFencedJSON(agentText); ok {
		return r
	}
	return ReviewResult{SummaryMD: normalizeMarkdown(prefixUTF8(agentText, maxAgentTextBytes), maxSummaryBytes)}
}

func parseFencedJSON(text string) (ReviewResult, bool) {
	lines := strings.Split(tailUTF8(text, maxAgentTextBytes), "\n")
	var (
		last       ReviewResult
		found      bool
		inFence    bool
		parseFence bool
		contentAt  int
	)
	for i, line := range lines {
		marker := strings.TrimSpace(line)
		if !inFence {
			if !strings.HasPrefix(marker, "```") {
				continue
			}
			inFence = true
			parseFence = isJSONFence(marker)
			contentAt = i + 1
			continue
		}

		if marker != "```" {
			continue
		}
		if parseFence {
			body := strings.Join(lines[contentAt:i], "\n")
			if len(body) <= maxJSONBlockBytes {
				if r, ok := decodeReview(body); ok {
					last, found = r, true
				}
			}
		}
		inFence = false
	}
	return last, found
}

func decodeReview(body string) (ReviewResult, bool) {
	if len(body) > maxJSONBlockBytes {
		return ReviewResult{}, false
	}
	var raw rawReview
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return ReviewResult{}, false
	}

	var summary string
	if len(raw.Summary) == 0 || json.Unmarshal(raw.Summary, &summary) != nil {
		return ReviewResult{}, false
	}
	out := ReviewResult{SummaryMD: normalizeMarkdown(summary, maxSummaryBytes)}
	if out.SummaryMD == "" {
		return ReviewResult{}, false
	}

	if len(raw.Findings) == 0 {
		return out, true
	}
	var rawFindings []json.RawMessage
	if err := json.Unmarshal(raw.Findings, &rawFindings); err != nil {
		return out, true // retain a valid summary when findings are malformed
	}
	for i, rawFinding := range rawFindings {
		if i >= maxFindingCandidates || len(out.Findings) >= maxFindings {
			break
		}
		if finding, ok := decodeFinding(rawFinding); ok {
			out.Findings = append(out.Findings, finding)
		}
	}
	return out, true
}

func isJSONFence(marker string) bool {
	if !strings.HasPrefix(marker, "```") {
		return false
	}
	language := strings.TrimSpace(strings.TrimPrefix(marker, "```"))
	return language == "" || strings.EqualFold(language, "json")
}

func decodeFinding(raw json.RawMessage) (Finding, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Finding{}, false
	}

	path, ok := requiredJSONString(fields, "path")
	if !ok {
		return Finding{}, false
	}
	line, ok := requiredJSONLine(fields)
	if !ok {
		return Finding{}, false
	}
	side, ok := optionalJSONString(fields, "side")
	if !ok {
		return Finding{}, false
	}
	severity, ok := optionalJSONString(fields, "severity")
	if !ok {
		return Finding{}, false
	}
	body, ok := requiredJSONString(fields, "body")
	if !ok {
		return Finding{}, false
	}

	return normalizeFinding(Finding{
		Path:     path,
		Line:     line,
		Side:     side,
		Severity: severity,
		Body:     body,
	})
}

func requiredJSONString(fields map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := fields[name]
	if !ok || isJSONNull(raw) {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func optionalJSONString(fields map[string]json.RawMessage, name string) (string, bool) {
	_, ok := fields[name]
	if !ok {
		return "", true
	}
	return requiredJSONString(fields, name)
}

func requiredJSONLine(fields map[string]json.RawMessage) (int64, bool) {
	raw, ok := fields["line"]
	if !ok || isJSONNull(raw) {
		return 0, false
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func normalizeFinding(f Finding) (Finding, bool) {
	var ok bool
	if f.Path, ok = normalizePath(f.Path); !ok {
		return Finding{}, false
	}
	if f.Line < 1 || f.Line > maxReviewLine {
		return Finding{}, false
	}
	if f.Side, ok = normalizeSide(f.Side); !ok {
		return Finding{}, false
	}
	if f.Severity, ok = normalizeSeverity(f.Severity); !ok {
		return Finding{}, false
	}
	if !isSafeFindingBody(f.Body) {
		return Finding{}, false
	}
	f.Body = normalizeMarkdown(f.Body, maxFindingBodyBytes)
	if f.Body == "" {
		return Finding{}, false
	}
	return f, true
}

func normalizeSide(side string) (string, bool) {
	side = strings.ToUpper(strings.TrimSpace(side))
	switch side {
	case "":
		return "RIGHT", true
	case "LEFT", "RIGHT":
		return side, true
	default:
		return "", false
	}
}

func normalizeSeverity(severity string) (string, bool) {
	severity = strings.ToLower(strings.TrimSpace(severity))
	switch severity {
	case "":
		return "info", true
	case "critical", "warning", "info", "praise":
		return severity, true
	default:
		return "", false
	}
}

func normalizePath(value string) (string, bool) {
	if value == "" || len(value) > maxFindingPathBytes || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return "", false
	}
	if path.IsAbs(value) || value == "." || path.Clean(value) != value {
		return "", false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return "", false
		}
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return value, true
}

func isSafeFindingBody(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		switch r {
		case '\n', '\r', '\t':
			continue
		default:
			if unicode.IsControl(r) {
				return false
			}
		}
	}
	return true
}

func normalizeMarkdown(value string, maxBytes int) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\t':
			return r
		case '\r':
			return '\n'
		default:
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}
	}, value)
	return truncateUTF8(strings.TrimSpace(value), maxBytes)
}

func prefixUTF8(value string, maxBytes int) string {
	return cutUTF8(value, maxBytes)
}

func tailUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 || value == "" {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	start := len(value) - maxBytes
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:]
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	if maxBytes <= len(truncationMarker) {
		return cutUTF8(value, maxBytes)
	}
	return cutUTF8(value, maxBytes-len(truncationMarker)) + truncationMarker
}

func cutUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && end < len(value) && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}
