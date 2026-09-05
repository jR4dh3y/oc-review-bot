package gh

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testApp(t *testing.T, handler http.Handler) *App {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &App{appID: "12345", key: key, secret: "whsec", botID: 707, base: srv.URL, http: srv.Client(), tokens: map[installationTokenKey]installationToken{}}
}

func signBody(t *testing.T, secret, body string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	a := testApp(t, http.NotFoundHandler())
	body := `{"action":"created"}`

	valid := signBody(t, "whsec", body)
	if !a.VerifySignature([]byte(body), valid) {
		t.Fatal("valid signature rejected")
	}
	if a.VerifySignature([]byte(body+"x"), valid) {
		t.Fatal("tampered payload accepted")
	}
	if a.VerifySignature([]byte(body), "sha256=deadbeef") {
		t.Fatal("wrong signature accepted")
	}
	if a.VerifySignature([]byte(body), "md5=abc") {
		t.Fatal("wrong scheme accepted")
	}
	if a.VerifySignature([]byte(body), "sha256=zz") {
		t.Fatal("malformed hex accepted")
	}
}

func TestSignAppJWT(t *testing.T) {
	a := testApp(t, http.NotFoundHandler())
	tok, err := a.SignAppJWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("want 3 JWT parts, got %d", len(parts))
	}
	claimBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimBytes, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["iss"] != "12345" {
		t.Fatalf("iss = %v", claims["iss"])
	}
}

func TestInstallationTokenCaches(t *testing.T) {
	calls := 0
	a := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/42/access_tokens":
			calls++
			if r.Method != http.MethodPost {
				t.Errorf("method = %s", r.Method)
			}
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
				t.Error("missing JWT auth")
			}
			var body struct {
				RepositoryIDs []int64 `json:"repository_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 505 {
				t.Errorf("token repository scope = %+v, err=%v", body, err)
			}
			json.NewEncoder(w).Encode(map[string]string{
				"token":      "inst-tok",
				"expires_at": "2099-01-01T00:00:00Z",
			})
		case r.URL.Path == "/repos/o/r/pulls/7":
			if r.Header.Get("Authorization") != "Bearer inst-tok" {
				t.Error("installation token not used")
			}
			fmt.Fprint(w, `{"number":7,"head":{"sha":"abc"},"base":{"sha":"def","repo":{"id":505,"full_name":"o/r"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))

	tok, err := a.InstallationToken(context.Background(), 42, 505)
	if err != nil || tok != "inst-tok" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	pr, err := a.GetPR(context.Background(), tok, "o/r", 7)
	if err != nil || pr.Head.SHA != "abc" {
		t.Fatalf("pr = %+v, %v", pr, err)
	}
	a.InstallationToken(context.Background(), 42, 505)
	if calls != 1 {
		t.Fatalf("token endpoint called %d times, want 1 (cached)", calls)
	}
}

func TestFindCommentMarkerRequiresBotAuthorAndExactMarker(t *testing.T) {
	const marker = "<!-- oc-review-bot:v2:opaque-marker -->"
	paths := []string{
		"/repos/o/r/issues/7/comments",
		"/repos/o/r/pulls/7/comments",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			a := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != path {
					http.NotFound(w, r)
					return
				}
				// The attacker comment is newer and has the same public marker;
				// reconciliation must skip it and continue to the bot comment.
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"id": 900, "body": marker, "user": map[string]any{"id": 999}},
					{"id": 901, "body": marker, "user": map[string]any{"id": 707}},
				})
			}))
			var (
				id    int64
				found bool
				err   error
			)
			if strings.HasSuffix(path, "/comments") && strings.Contains(path, "/issues/") {
				id, found, err = a.FindIssueCommentMarker(context.Background(), "token", "o/r", 7, marker)
			} else {
				id, found, err = a.FindReviewCommentMarker(context.Background(), "token", "o/r", 7, marker)
			}
			if err != nil || !found || id != 901 {
				t.Fatalf("marker result = %d, %t, %v; want bot comment 901", id, found, err)
			}
		})
	}
}

func TestFindCommentMarkerRejectsAttackerOnlyAndDuplicateMarkers(t *testing.T) {
	const marker = "<!-- oc-review-bot:v2:opaque-marker -->"
	a := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 900, "body": marker, "user": map[string]any{"id": 999}},
			{"id": 901, "body": marker + "\n" + marker, "user": map[string]any{"id": 707}},
		})
	}))
	if id, found, err := a.FindIssueCommentMarker(context.Background(), "token", "o/r", 7, marker); err != nil || found || id != 0 {
		t.Fatalf("attacker/duplicate marker result = %d, %t, %v; want no match", id, found, err)
	}
}

func TestListFilesPaginatesPastThreePages(t *testing.T) {
	a := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls/7/files" {
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		count := 100
		if page == "3" {
			count = 50
		}
		files := make([]File, count)
		for i := range files {
			files[i].Filename = fmt.Sprintf("file-%s-%d.go", page, i)
		}
		json.NewEncoder(w).Encode(files)
	}))

	files, err := a.ListFiles(context.Background(), "token", "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 250 {
		t.Fatalf("got %d files, want 250", len(files))
	}
}

func TestGitHubErrorsDoNotExposeResponseBodies(t *testing.T) {
	a := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "provider reflected secret: sk-never-log-this")
	}))
	_, err := a.GetPR(context.Background(), "token", "o/r", 7)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "sk-never-log-this") {
		t.Fatalf("response body leaked in error: %v", err)
	}
}

func TestOnlyActualRateLimitResponsesAreMarkedRateLimited(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		headers map[string]string
		want    bool
	}{
		{"forbidden permission", http.StatusForbidden, nil, false},
		{"forbidden exhausted", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, true},
		{"too many requests", http.StatusTooManyRequests, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tt.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
			}))
			_, err := a.GetPR(context.Background(), "token", "o/r", 7)
			if errors.Is(err, ErrRateLimit) != tt.want {
				t.Fatalf("errors.Is(%v, ErrRateLimit) = %v, want %v", err, errors.Is(err, ErrRateLimit), tt.want)
			}
		})
	}
}

func TestParseKeyFromPathAndLiteral(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, []byte(pemText), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := parseRSAKey(path); err != nil || got.N.Cmp(key.N) != 0 {
		t.Fatalf("parse from path failed: %v", err)
	}
	if got, err := parseRSAKey(pemText); err != nil || got.N.Cmp(key.N) != 0 {
		t.Fatalf("parse from literal failed: %v", err)
	}
}
