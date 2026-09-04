// Package runner executes one opencode2 review run against a PR checkout.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrQuota marks failures caused by the API key's quota or rate limit, which
// should send the key into cooldown.
var ErrQuota = errors.New("zen quota or rate limit hit")

const quotaMarkers = "rate limit|ratelimit|429|402|quota|usage limit|credit|exceeded"

// Options configures one run.
type Options struct {
	Bin      string   // opencode2 binary
	PreArgs  []string // global flags, e.g. --standalone
	CloneURL string   // https clone URL with credentials
	Ref      string   // git ref to fetch, e.g. refs/pull/7/head
	Model    string   // provider/model
	APIKey   string   // Zen key for this run
	Diff     []byte   // PR diff, attached to the message
	Prompt   string
}

// Run clones the repo, isolates opencode2's config with the pooled key, runs
// the agent, and returns its final message text.
func Run(ctx context.Context, o Options) (string, error) {
	if o.Bin == "" {
		o.Bin = "opencode2"
	}
	tmp, err := os.MkdirTemp("", "oc-review-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	if err := checkout(ctx, tmp, o); err != nil {
		return "", fmt.Errorf("checkout: %w", err)
	}

	diffPath := filepath.Join(tmp, "review-diff.patch")
	if err := os.WriteFile(diffPath, o.Diff, 0o600); err != nil {
		return "", err
	}

	env, err := isolatedEnv(tmp, o.APIKey)
	if err != nil {
		return "", err
	}

	args := o.PreArgs
	args = append(args, "run",
		"--model", o.Model,
		"--format", "json",
		"--file", diffPath,
		"--auto",
		o.Prompt,
	)
	cmd := exec.CommandContext(ctx, o.Bin, args...)
	cmd.Dir = filepath.Join(tmp, "clone")
	cmd.Env = env
	// Kill the whole process group: opencode2 spawns helper processes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start %s: %w", o.Bin, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return "", fmt.Errorf("review timed out; last stderr: %s", tail(stderr.String(), 2000))
	case err := <-done:
		if err != nil {
			combined := stderr.String() + stdout.String()
			if matchesAny(combined, quotaMarkers) {
				return "", fmt.Errorf("%w: %s", ErrQuota, tail(combined, 1000))
			}
			return "", fmt.Errorf("%s run failed: %w; stderr: %s", o.Bin, err, tail(combined, 2000))
		}
	}
	return ExtractText(stdout.String()), nil
}

// checkout does a shallow fetch of ref into tmp/clone.
func checkout(ctx context.Context, tmp string, o Options) error {
	dir := filepath.Join(tmp, "clone")
	git := func(args ...string) error {
		c := exec.CommandContext(ctx, "git", args...)
		c.Dir = dir
		var errb bytes.Buffer
		c.Stderr = &errb
		if err := c.Run(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, tail(errb.String(), 500))
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", o.CloneURL},
		{"-c", "protocol.version=2", "fetch", "--depth", "1", "origin", o.Ref},
		{"checkout", "-q", "FETCH_HEAD"},
		{"config", "user.email", "noreply@oc-review-bot.local"},
		{"config", "user.name", "oc-review-bot"},
	} {
		if err := git(args...); err != nil {
			return err
		}
	}
	return nil
}

// isolatedEnv builds the process environment: HOME/XDG point into the temp
// dir, and the Zen key is available both via auth.json and the env var that
// opencode.json's {env:...} syntax references.
func isolatedEnv(tmp, apiKey string) ([]string, error) {
	xdgConfig := filepath.Join(tmp, "xdg-config")
	xdgData := filepath.Join(tmp, "xdg-data")
	home := filepath.Join(tmp, "home")
	for _, d := range []string{xdgConfig, xdgData, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}

	// Config for both possible v2 config dirs; harmless if unused.
	cfg := fmt.Sprintf(`{"provider":{"opencode":{"options":{"apiKey":"{env:OC_REVIEW_ZEN_KEY}"}}}}`)
	for _, sub := range []string{"opencode", "opencode2"} {
		dir := filepath.Join(xdgConfig, sub)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(cfg), 0o600); err != nil {
			return nil, err
		}
	}

	// auth.json in the documented credentials location.
	auth := fmt.Sprintf(`{"opencode":{"type":"api","key":%s}}`, mustJSON(apiKey))
	for _, sub := range []string{"opencode", "opencode2"} {
		dir := filepath.Join(xdgData, sub)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0o600); err != nil {
			return nil, err
		}
	}

	env := append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+xdgConfig,
		"XDG_DATA_HOME="+xdgData,
		"OC_REVIEW_ZEN_KEY="+apiKey,
		"CI=1",
	)
	return env, nil
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ExtractText pulls the assistant's text out of `--format json` output. The
// v2 CLI emits JSONL events; if stdout is not parseable it is returned as-is.
func ExtractText(stdout string) string {
	texts, anyJSON := extractJSONLTexts(stdout)
	if anyJSON && len(texts) > 0 {
		return strings.Join(texts, "\n")
	}
	return stdout
}

// extractJSONLTexts walks each JSONL line and collects "text" values from
// objects shaped like {"type":"text","text":"..."}.
func extractJSONLTexts(stdout string) ([]string, bool) {
	var texts []string
	anyJSON := false
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			continue
		}
		anyJSON = true
		collectTexts(v, &texts)
	}
	return texts, anyJSON
}

func collectTexts(v any, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		if typ, _ := t["type"].(string); typ == "text" {
			if s, ok := t["text"].(string); ok {
				*out = append(*out, s)
			}
		}
		for _, child := range t {
			collectTexts(child, out)
		}
	case []any:
		for _, child := range t {
			collectTexts(child, out)
		}
	}
}

// matchesAny reports whether s contains any '|'-separated marker
// (case-insensitive).
func matchesAny(s, markers string) bool {
	s = strings.ToLower(s)
	for _, m := range strings.Split(markers, "|") {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
