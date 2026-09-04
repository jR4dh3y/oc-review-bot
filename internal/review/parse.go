// Package review turns agent output into GitHub review comments.
package review

import (
	"encoding/json"
	"strings"
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
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

// ExtractReview parses the agent's final message. It looks for the last
// fenced JSON block matching the agreed contract; when the agent ignored the
// contract, the whole message becomes the summary with no inline findings.
func ExtractReview(agentText string) ReviewResult {
	if r, ok := parseFencedJSON(agentText); ok {
		return r
	}
	return ReviewResult{SummaryMD: strings.TrimSpace(agentText)}
}

func parseFencedJSON(text string) (ReviewResult, bool) {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "```" && !strings.HasPrefix(line, "```json") {
			continue
		}
		// Fence close found; scan upwards for the opener.
		for j := i - 1; j >= 0; j-- {
			open := strings.TrimSpace(lines[j])
			if strings.HasPrefix(open, "```") {
				body := strings.Join(lines[j+1:i], "\n")
				if r, ok := decodeReview(body); ok {
					return r, true
				}
				break
			}
		}
	}
	return ReviewResult{}, false
}

func decodeReview(body string) (ReviewResult, bool) {
	var raw rawReview
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return ReviewResult{}, false
	}
	if strings.TrimSpace(raw.Summary) == "" {
		return ReviewResult{}, false
	}
	out := ReviewResult{SummaryMD: strings.TrimSpace(raw.Summary)}
	for _, f := range raw.Findings {
		if strings.TrimSpace(f.Path) == "" || f.Line == 0 || strings.TrimSpace(f.Body) == "" {
			continue // drop malformed findings, keep the rest
		}
		if f.Side == "" {
			f.Side = "RIGHT"
		}
		if f.Severity == "" {
			f.Severity = "info"
		}
		f.Body = strings.TrimSpace(f.Body)
		out.Findings = append(out.Findings, f)
	}
	return out, true
}
