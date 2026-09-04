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
	return &App{appID: "12345", key: key, secret: "whsec", base: srv.URL, http: srv.Client(), tokens: map[int64]installationToken{}}
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
			json.NewEncoder(w).Encode(map[string]string{
				"token":      "inst-tok",
				"expires_at": "2099-01-01T00:00:00Z",
			})
		case r.URL.Path == "/repos/o/r/pulls/7":
			if r.Header.Get("Authorization") != "Bearer inst-tok" {
				t.Error("installation token not used")
			}
			fmt.Fprint(w, `{"number":7,"head":{"sha":"abc"},"base":{"sha":"def"}}`)
		default:
			http.NotFound(w, r)
		}
	}))

	tok, err := a.InstallationToken(context.Background(), 42)
	if err != nil || tok != "inst-tok" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	pr, err := a.GetPR(context.Background(), tok, "o/r", 7)
	if err != nil || pr.Head.SHA != "abc" {
		t.Fatalf("pr = %+v, %v", pr, err)
	}
	a.InstallationToken(context.Background(), 42)
	if calls != 1 {
		t.Fatalf("token endpoint called %d times, want 1 (cached)", calls)
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
