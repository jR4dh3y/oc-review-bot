package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractTextFromJSONL(t *testing.T) {
	fence := "```"
	stdout := strings.Join([]string{
		`{"type":"step.start","part":{"id":1}}`,
		`{"type":"text","text":"First part.\n"}`,
		`{"type":"text","text":"` + fence + `json"}`,
		`{"type":"text","text":"{\"summary\":\"s\",\"findings\":[]}"}`,
		`{"type":"text","text":"` + fence + `"}`,
	}, "\n")

	got := ExtractText(stdout)
	if !strings.Contains(got, "First part.") || !strings.Contains(got, "\"summary\":\"s\"") {
		t.Fatalf("extracted text incomplete: %q", got)
	}
	if strings.Contains(got, `"type":"step.start"`) {
		t.Fatalf("event metadata leaked into text: %q", got)
	}
}

func TestExtractTextPlainFallback(t *testing.T) {
	plain := "just text, no json"
	if got := ExtractText(plain); got != plain {
		t.Fatalf("got %q", got)
	}
}

func TestQuotaDetection(t *testing.T) {
	cases := map[string]bool{
		"HTTP 429 Too Many Requests": true,
		"You ran out of credits":     true,
		"Usage limit reached":        true,
		"file not found":             false,
	}
	for in, want := range cases {
		if got := matchesAny(strings.ToLower(in), quotaMarkers); got != want {
			t.Errorf("matchesAny(%q) = %v, want %v", in, got, want)
		}
	}
}

// fakeBin writes a shell script that mimics opencode2 run: it asserts its
// isolated environment and emits a JSONL text event with the review.
func fakeBin(t *testing.T, outFile, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-opencode2")
	script := fmt.Sprintf(`#!/bin/sh
set -e
{
  echo "HOME=$HOME"
  echo "XDG_CONFIG=$XDG_CONFIG_HOME"
  echo "KEY=$OC_REVIEW_ZEN_KEY"
} > "%s"
for d in "$XDG_DATA_HOME"/opencode "$XDG_DATA_HOME"/opencode2; do
  test -f "$d/auth.json" || exit 90
done
for f in "$XDG_CONFIG_HOME"/opencode/opencode.json "$XDG_CONFIG_HOME"/opencode2/opencode.json; do
  test -f "$f" || exit 91
done
# diff patch is written next to the clone, not inside it
test -f ../review-diff.patch || exit 92
cat <<'JSONL'
{"type":"text","text":%s}
JSONL
`, outFile, fmt.Sprintf("%q", text))
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// gitRun runs a git command and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, errb.String())
	}
	return out.String()
}

// initRemote creates a local bare repo exposing one commit as refs/pull/7/head.
func initRemote(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "origin.git")
	work := filepath.Join(t.TempDir(), "work")

	gitRun(t, "", "init", "-q", "--bare", bare)
	gitRun(t, "", "init", "-q", work)
	gitRun(t, work, "config", "user.email", "t@t")
	gitRun(t, work, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-qm", "init")
	gitRun(t, work, "push", "-q", bare, "main")
	head := strings.TrimSpace(gitRun(t, work, "rev-parse", "HEAD"))
	gitRun(t, bare, "update-ref", "refs/pull/7/head", head)
	return bare
}

func TestRunEndToEndWithFakeBin(t *testing.T) {
	outFile := filepath.Join(t.TempDir(), "env.out")
	remote := initRemote(t)
	bin := fakeBin(t, outFile, "Reviewed the diff.\n\n```json\n{\"summary\":\"ok\"}\n```")

	got, err := Run(context.Background(), Options{
		Bin:      bin,
		CloneURL: remote,
		Ref:      "refs/pull/7/head",
		Model:    "opencode/big-pickle",
		APIKey:   "sk-test-9999",
		Diff:     []byte("diff --git a/x b/x"),
		Prompt:   "review it",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(got, "Reviewed the diff.") {
		t.Fatalf("output = %q", got)
	}

	envData, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	env := string(envData)
	if !strings.Contains(env, "KEY=sk-test-9999") {
		t.Fatalf("agent did not receive the pooled key: %s", env)
	}
	if !strings.Contains(env, "XDG_CONFIG=/tmp/") || strings.Contains(env, os.Getenv("HOME")) {
		t.Fatalf("HOME/XDG not isolated from the real environment: %s", env)
	}
}

func TestRunFailsOnQuotaError(t *testing.T) {
	remote := initRemote(t)
	bin := filepath.Join(t.TempDir(), "quota-bin")
	script := `#!/bin/sh
echo "Error: 429 usage limit reached for this key" >&2
exit 1
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{
		Bin:      bin,
		CloneURL: remote,
		Ref:      "refs/pull/7/head",
		Model:    "opencode/big-pickle",
		APIKey:   "sk-test-9999",
		Diff:     []byte("diff"),
		Prompt:   "review it",
	})
	if err == nil {
		t.Fatal("expected quota error")
	}
	if msg := err.Error(); !strings.Contains(msg, "usage limit") {
		t.Fatalf("error should include CLI output: %v", err)
	}
}
