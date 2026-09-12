package orca

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testService(t *testing.T) *Service {
	t.Helper()
	s := NewService("samik-bot", "https://bot.example.com/auth/orca/callback", "ref_test123")
	t.Cleanup(func() { s.HTTPClient.CloseIdleConnections() })
	return s
}

func TestConnectURLMintsPKCEAndReferral(t *testing.T) {
	s := testService(t)
	first, err := s.ConnectURL()
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ConnectURL()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(first)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "https" || parsed.Host != "www.orcarouter.ai" || parsed.Path != AuthPath {
		t.Fatalf("connect URL = %q", first)
	}
	query := parsed.Query()
	if query.Get("callback_url") != "https://bot.example.com/auth/orca/callback" {
		t.Fatalf("callback_url = %q", query.Get("callback_url"))
	}
	if query.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q", query.Get("code_challenge_method"))
	}
	if query.Get("app_name") != "samik-bot" || query.Get("ref") != "ref_test123" {
		t.Fatalf("app_name/ref = %q / %q", query.Get("app_name"), query.Get("ref"))
	}
	state := query.Get("state")
	// 43 base64url characters encode 256 bits, which is the entropy that
	// makes the state unguessable.
	if len(state) != 43 {
		t.Fatalf("state %q has length %d, want 43 base64url characters", state, len(state))
	}
	// The challenge must be the S256 digest of the verifier held server-side.
	pending := s.pending[state]
	if pending.verifier == "" {
		t.Fatal("state was not stored as a pending connect")
	}
	digest := sha256.Sum256([]byte(pending.verifier))
	want := base64.RawURLEncoding.EncodeToString(digest[:])
	if query.Get("code_challenge") != want {
		t.Fatalf("code_challenge = %q, want S256 of the stored verifier", query.Get("code_challenge"))
	}
	// The verifier itself must never travel in the URL.
	if strings.Contains(first, pending.verifier) {
		t.Fatal("connect URL leaks the PKCE verifier")
	}
	if first == second {
		t.Fatal("each connect attempt must mint a fresh state")
	}
}

func TestExchangeConsumesStateExactlyOnce(t *testing.T) {
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ExchangePath || r.Method != http.MethodPost {
			t.Errorf("exchange hit %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Errorf("content type = %q", got)
		}
		var payload struct {
			Code         string `json:"code"`
			CodeVerifier string `json:"code_verifier"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		bodies = append(bodies, payload.CodeVerifier)
		w.Write([]byte(`{"key":"sk-orca-test-key"}`))
	}))
	defer server.Close()

	s := testService(t)
	s.APIBaseURL = server.URL
	authURL, err := s.ConnectURL()
	if err != nil {
		t.Fatal(err)
	}
	state, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	stateValue := state.Query().Get("state")

	key, err := s.Exchange(context.Background(), "auth-code", stateValue)
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-orca-test-key" {
		t.Fatalf("key = %q", key)
	}
	if len(bodies) != 1 || bodies[0] == "" {
		t.Fatalf("sent verifiers = %q", bodies)
	}
	// The state is single-use: a replay must be rejected without a network
	// call, which the still-empty second body list proves.
	if _, err := s.Exchange(context.Background(), "auth-code", stateValue); err == nil {
		t.Fatal("replayed state must be rejected")
	}
	if _, err := s.Exchange(context.Background(), "auth-code", "never-minted"); err == nil {
		t.Fatal("unknown state must be rejected")
	}
	if len(bodies) != 1 {
		t.Fatalf("rejected exchanges must not reach the API, sent = %q", bodies)
	}
}

func TestExchangeRejectsExpiredState(t *testing.T) {
	s := testService(t)
	authURL, err := s.ConnectURL()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return time.Now().Add(2 * pendingConnectTTL) }
	if _, err := s.Exchange(context.Background(), "code", parsed.Query().Get("state")); err == nil {
		t.Fatal("expired state must be rejected")
	}
}

func TestExchangeSurfacesAPIErrorsAndMalformedBodies(t *testing.T) {
	var mode string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "status":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.Write([]byte(`{"error":"nope"}`))
		}
	}))
	defer server.Close()

	s := testService(t)
	s.APIBaseURL = server.URL

	mode = "status"
	if _, err := s.Exchange(context.Background(), "code", mintState(t, s)); err == nil {
		t.Fatal("a non-200 exchange must fail")
	}
	mode = "body"
	if _, err := s.Exchange(context.Background(), "code", mintState(t, s)); err == nil {
		t.Fatal("a body without an API key must fail")
	}
	if _, err := s.Exchange(context.Background(), "code", "never-minted"); err == nil {
		t.Fatal("unknown state must fail")
	}
}

// mintState mints a connect URL and returns its fresh state.
func mintState(t *testing.T, s *Service) string {
	t.Helper()
	authURL, err := s.ConnectURL()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Query().Get("state")
}
