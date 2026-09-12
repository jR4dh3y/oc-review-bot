package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jR4dh3y/samik-bot/internal/orca"
)

// partnerHarness builds a server with the partner routes registered exactly
// as New registers them, plus an admin and a non-admin session.
func partnerHarness(t *testing.T) (*Server, *http.ServeMux, string, string) {
	t.Helper()
	s, _, _ := setup(t, nil)
	s.orca = orca.NewService(s.cfg.BotUsername, s.cfg.PublicURL+"/auth/orca/callback", s.cfg.OrcaReferralCode)
	admin, err := s.st.UpsertUser(testAdminID, "root", "", true)
	if err != nil {
		t.Fatal(err)
	}
	pleb, err := s.st.UpsertUser(testRequesterID, "pleb", "", false)
	if err != nil {
		t.Fatal(err)
	}
	adminToken := "tok-partner-admin"
	if err := s.st.CreateSession(admin.ID, adminToken, time.Hour); err != nil {
		t.Fatal(err)
	}
	plebToken := "tok-partner-pleb"
	if err := s.st.CreateSession(pleb.ID, plebToken, time.Hour); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/admin/partner", s.withAdmin(s.handlePartnerInfo))
	mux.HandleFunc("GET /orca/connect-url", s.withAdmin(s.handleOrcaConnectURL))
	mux.HandleFunc("GET /auth/orca/callback", s.handleOrcaCallback)
	return s, mux, adminToken, plebToken
}

func partnerGet(t *testing.T, mux *http.ServeMux, token, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestPartnerInfoRequiresAdminAndReportsTheReferralIdentity checks that the
// partner facts are admin-only and carry the referral code, connect
// endpoints, and pinned script location the dashboard section renders.
func TestPartnerInfoRequiresAdminAndReportsTheReferralIdentity(t *testing.T) {
	s, mux, adminToken, plebToken := partnerHarness(t)

	if rec := partnerGet(t, mux, plebToken, "/api/admin/partner"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin code = %d", rec.Code)
	}
	rec := partnerGet(t, mux, adminToken, "/api/admin/partner")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin code = %d, %s", rec.Code, rec.Body.String())
	}
	var info struct {
		ReferralCode         string `json:"referral_code"`
		ReferralURL          string `json:"referral_url"`
		CallbackURL          string `json:"callback_url"`
		ConnectScript        string `json:"connect_script"`
		ConnectScriptIntegri string `json:"connect_script_integrity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.ReferralCode != orca.DefaultReferralCode {
		t.Fatalf("referral_code = %q", info.ReferralCode)
	}
	if want := orca.WebBaseURL + "/ref/" + orca.DefaultReferralCode; info.ReferralURL != want {
		t.Fatalf("referral_url = %q, want %q", info.ReferralURL, want)
	}
	if info.CallbackURL != s.cfg.PublicURL+"/auth/orca/callback" {
		t.Fatalf("callback_url = %q", info.CallbackURL)
	}
	if info.ConnectScript != orcaConnectScriptURL || info.ConnectScriptIntegri != orcaConnectSRI {
		t.Fatalf("connect script = %q (%q)", info.ConnectScript, info.ConnectScriptIntegri)
	}
}

// TestOrcaConnectURLMintsPerAdminRequest verifies the drop-in endpoint's
// contract: an admin-gated GET answering {auth_url} with the callback, PKCE
// challenge parameters, and the referral code baked in.
func TestOrcaConnectURLMintsPerAdminRequest(t *testing.T) {
	s, mux, adminToken, plebToken := partnerHarness(t)

	if rec := partnerGet(t, mux, plebToken, "/orca/connect-url"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin code = %d", rec.Code)
	}
	rec := partnerGet(t, mux, adminToken, "/orca/connect-url")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin code = %d, %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		AuthURL string `json:"auth_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(payload.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "https" || parsed.Host != "www.orcarouter.ai" {
		t.Fatalf("auth_url = %q", payload.AuthURL)
	}
	query := parsed.Query()
	if query.Get("callback_url") != s.cfg.PublicURL+"/auth/orca/callback" ||
		query.Get("code_challenge_method") != "S256" ||
		query.Get("ref") != orca.DefaultReferralCode ||
		query.Get("state") == "" || query.Get("code_challenge") == "" {
		t.Fatalf("auth_url parameters = %v", query)
	}
	second := partnerGet(t, mux, adminToken, "/orca/connect-url")
	var secondPayload struct {
		AuthURL string `json:"auth_url"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondPayload); err != nil {
		t.Fatal(err)
	}
	if secondPayload.AuthURL == payload.AuthURL {
		t.Fatal("each connect request must mint a fresh state")
	}
}

// TestOrcaCallbackExchangesAndPoolsTheKey walks the full callback: a state
// minted by ConnectURL is exchanged against a stub OrcaRouter, and the
// returned key lands in the encrypted pool as an orcarouter-connect key.
func TestOrcaCallbackExchangesAndPoolsTheKey(t *testing.T) {
	s, mux, adminToken, _ := partnerHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"key":"sk-orca-1234"}`))
	}))
	defer upstream.Close()
	s.orca.APIBaseURL = upstream.URL

	authURL, err := s.orca.ConnectURL()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	callback := "/auth/orca/callback?code=the-code&state=" + url.QueryEscape(parsed.Query().Get("state"))
	req := httptest.NewRequest(http.MethodGet, callback, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: adminToken})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback code = %d, %s", rec.Code, rec.Body.String())
	}
	if location := rec.Header().Get("Location"); location != partnerRedirectPath+"?connected=1" {
		t.Fatalf("redirect = %q", location)
	}
	keys, err := s.st.ListKeys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("pooled keys = %+v, %v", keys, err)
	}
	if keys[0].Label != "orcarouter-connect" || keys[0].Last4 != "1234" {
		t.Fatalf("pooled key = %+v", keys[0])
	}
}

// TestOrcaCallbackRejectsUnknownStateWithoutPooling proves the state check
// rejects foreign callbacks before any exchange or key storage.
func TestOrcaCallbackRejectsUnknownStateWithoutPooling(t *testing.T) {
	s, mux, _, _ := partnerHarness(t)
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"key":"sk-orca-attacker"}`))
	}))
	defer upstream.Close()
	s.orca.APIBaseURL = upstream.URL

	rec := partnerGet(t, mux, "", "/auth/orca/callback?code=evil&state=forged")
	if rec.Code != http.StatusFound {
		t.Fatalf("callback code = %d", rec.Code)
	}
	if location := rec.Header().Get("Location"); !strings.HasPrefix(location, partnerRedirectPath+"?error=state") {
		t.Fatalf("redirect = %q", location)
	}
	if called {
		t.Fatal("unknown state must not reach the exchange endpoint")
	}
	keys, err := s.st.ListKeys()
	if err != nil || len(keys) != 0 {
		t.Fatalf("forged callback pooled keys: %+v, %v", keys, err)
	}
}
