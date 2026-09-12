// Package orca implements the OrcaRouter partner connect flow: minting PKCE
// authorization URLs that carry the partner referral code, and exchanging the
// returned authorization code for an API key. The verifier never leaves this
// package's server-side state; the browser only ever sees the challenge.
package orca

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Fixed OrcaRouter endpoints. The model-gateway base URL that the reviewer
// engines use lives in the runner; these URLs are for the browser consent and
// key exchange endpoints only.
const (
	WebBaseURL   = "https://www.orcarouter.ai"
	APIBaseURL   = "https://api.orcarouter.ai"
	AuthPath     = "/auth"
	ExchangePath = "/api/v1/auth/keys"

	// DefaultReferralCode is the deployment's partner referral code baked into
	// every connect URL and surfaced on the partner dashboard.
	DefaultReferralCode = "ref_b7dd35655fa712a6b8b0"

	maxKeyBytes        = 512
	maxExchangeBodyKey = 64 << 10
)

// pendingConnectTTL bounds how long a minted authorization attempt stays
// redeemable. It only has to cover the user's round trip through the consent
// screen.
const pendingConnectTTL = 10 * time.Minute

// ErrUnknownState marks an exchange whose state was never minted here,
// already redeemed, or expired. The callback must reject it without a network
// call; it is the parameter that makes the callback ours.
var ErrUnknownState = errors.New("unknown or expired connect state")

// Service mints connect URLs and exchanges authorization codes for keys.
// Pending verifiers live in memory: the service is single-replica by design,
// and a lost attempt only costs the user one retry.
type Service struct {
	AuthBaseURL  string
	APIBaseURL   string
	ReferralCode string
	AppName      string
	CallbackURL  string
	HTTPClient   *http.Client
	Now          func() time.Time

	mu      sync.Mutex
	pending map[string]pendingConnect
}

type pendingConnect struct {
	verifier string
	expires  time.Time
}

// NewService builds the connect service for one deployment. callbackURL must
// be the exact absolute URL OrcaRouter redirects back to.
func NewService(appName, callbackURL, referralCode string) *Service {
	if referralCode == "" {
		referralCode = DefaultReferralCode
	}
	return &Service{
		AuthBaseURL:  WebBaseURL,
		APIBaseURL:   APIBaseURL,
		ReferralCode: referralCode,
		AppName:      appName,
		CallbackURL:  callbackURL,
		HTTPClient:   &http.Client{Timeout: 30 * time.Second},
		Now:          time.Now,
		pending:      map[string]pendingConnect{},
	}
}

// ConnectURL mints a single-use PKCE authorization URL carrying the referral
// code. Each call produces a fresh state and verifier.
func (s *Service) ConnectURL() (string, error) {
	verifier, err := randomToken(64)
	if err != nil {
		return "", err
	}
	state, err := randomToken(32)
	if err != nil {
		return "", err
	}
	now := s.now()
	s.mu.Lock()
	s.sweepExpiredLocked(now)
	s.pending[state] = pendingConnect{verifier: verifier, expires: now.Add(pendingConnectTTL)}
	s.mu.Unlock()

	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	u := s.AuthBaseURL + AuthPath +
		"?callback_url=" + url.QueryEscape(s.CallbackURL) +
		"&code_challenge=" + challenge +
		"&code_challenge_method=S256" +
		"&state=" + state +
		"&app_name=" + url.QueryEscape(s.AppName) +
		"&ref=" + url.QueryEscape(s.ReferralCode)
	return u, nil
}

// Exchange redeems an authorization code with the verifier held for state.
// The state is consumed on every path: an attempt can complete exactly once.
func (s *Service) Exchange(ctx context.Context, code, state string) (string, error) {
	verifier, err := s.takeVerifier(state)
	if err != nil {
		return "", err
	}
	payload := fmt.Sprintf(`{"code":%q,"code_verifier":%q}`, code, verifier)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.APIBaseURL+ExchangePath, strings.NewReader(payload))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.httpClient().Do(request)
	if err != nil {
		return "", fmt.Errorf("orcarouter key exchange: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxExchangeBodyKey))
	if err != nil {
		return "", fmt.Errorf("orcarouter key exchange: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("orcarouter key exchange returned status %d", response.StatusCode)
	}
	key := extractKey(body)
	if key == "" {
		return "", fmt.Errorf("orcarouter key exchange returned no API key")
	}
	return key, nil
}

// takeVerifier consumes the pending attempt for state.
func (s *Service) takeVerifier(state string) (string, error) {
	if state == "" {
		return "", ErrUnknownState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.pending[state]
	if !ok {
		return "", ErrUnknownState
	}
	delete(s.pending, state)
	if s.now().After(pending.expires) {
		return "", ErrUnknownState
	}
	return pending.verifier, nil
}

func (s *Service) sweepExpiredLocked(now time.Time) {
	for state, pending := range s.pending {
		if now.After(pending.expires) {
			delete(s.pending, state)
		}
	}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) httpClient() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return http.DefaultClient
}

// extractKey reads the API key out of the exchange response. The documented
// shape is a JSON object with the key under "key" or "api_key"; a bare text
// body is accepted as the key itself so a wire change cannot strand the flow.
func extractKey(body []byte) string {
	for _, field := range []string{`"key":"`, `"api_key":"`, `"apikey":"`, `"key": "`} {
		if start := strings.Index(string(body), field); start >= 0 {
			rest := body[start+len(field):]
			if end := strings.IndexByte(string(rest), '"'); end >= 0 {
				return strings.TrimSpace(string(rest[:end]))
			}
		}
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || strings.HasPrefix(trimmed, "{") {
		return ""
	}
	return trimmed
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
