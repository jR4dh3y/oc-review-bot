// Package gh talks to GitHub as a GitHub App: webhook signature
// verification, installation tokens, and the REST calls the bot needs.
package gh

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const defaultBase = "https://api.github.com"

// ErrRateLimit marks quota/rate-limit failures that should cool a key down.
var ErrRateLimit = errors.New("github rate limited")

// App authenticates as a GitHub App and caches installation tokens.
type App struct {
	appID  string
	key    *rsa.PrivateKey
	secret string
	base   string
	http   *http.Client

	mu     sync.Mutex
	tokens map[int64]installationToken
}

type installationToken struct {
	token     string
	expiresAt time.Time
}

// NewApp parses the private key PEM and returns an App.
func NewApp(appID, privateKeyPEM, webhookSecret string) (*App, error) {
	key, err := parseRSAKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	return &App{
		appID:  appID,
		key:    key,
		secret: webhookSecret,
		base:   apiBase(),
		http:   &http.Client{Timeout: 30 * time.Second},
		tokens: map[int64]installationToken{},
	}, nil
}

// apiBase returns the GitHub API base URL, overridable via GITHUB_API_BASE
// (used by tests and GitHub Enterprise).
func apiBase() string {
	if v := os.Getenv("GITHUB_API_BASE"); v != "" {
		return v
	}
	return defaultBase
}

// VerifySignature checks the X-Hub-Signature-256 header against the payload.
func (a *App) VerifySignature(payload []byte, signatureHeader string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(signatureHeader, prefix) {
		return false
	}
	sig, err := decodeHex(signatureHeader[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(a.secret))
	mac.Write(payload)
	return hmac.Equal(sig, mac.Sum(nil))
}

// doJSON performs an authenticated request and decodes the response.
func (a *App) doJSON(ctx context.Context, token, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("%s %s: %d: %w", method, path, resp.StatusCode, ErrRateLimit)
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %d: %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
