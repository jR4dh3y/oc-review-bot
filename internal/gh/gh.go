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
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const defaultBase = "https://api.github.com"

// ErrRateLimit marks quota/rate-limit failures that should cool a key down.
var ErrRateLimit = errors.New("github rate limited")

// ErrResponseTooLarge marks an API response that exceeds the reviewer's safe
// processing bounds.
var ErrResponseTooLarge = errors.New("github response too large")

// HTTPError is a sanitized GitHub API failure. It intentionally never stores
// a response body because GitHub can reflect hostile pull-request content.
type HTTPError struct {
	Method     string
	Path       string
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("github %s %s returned status %d", e.Method, e.Path, e.StatusCode)
}

func (e *HTTPError) Unwrap() error {
	if e.StatusCode == http.StatusTooManyRequests {
		return ErrRateLimit
	}
	return nil
}

// IsRetryable reports whether a GitHub transport/API failure is safe to retry.
func IsRetryable(err error) bool {
	if errors.Is(err, ErrRateLimit) {
		return true
	}
	var response *HTTPError
	if errors.As(err, &response) {
		return response.StatusCode == http.StatusRequestTimeout || response.StatusCode >= 500
	}
	var networkErr net.Error
	return errors.As(err, &networkErr) && (networkErr.Timeout() || networkErr.Temporary())
}

// IsPermanent reports known GitHub API responses that will not succeed by
// retrying the exact same request.
func IsPermanent(err error) bool {
	var response *HTTPError
	return errors.As(err, &response) && !IsRetryable(response)
}

const userAgent = "oc-review-bot"

const maxJSONResponseBytes = 10 << 20

// App authenticates as a GitHub App and caches installation tokens.
type App struct {
	appID  string
	key    *rsa.PrivateKey
	secret string
	botID  int64
	base   string
	http   *http.Client

	mu     sync.Mutex
	tokens map[installationTokenKey]installationToken
}

type installationTokenKey struct {
	installationID int64
	repositoryID   int64
}

type installationToken struct {
	token     string
	expiresAt time.Time
}

// NewApp parses the private key PEM and returns an App.
func NewApp(appID, privateKeyPEM, webhookSecret string, botGitHubID int64) (*App, error) {
	key, err := parseRSAKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	return &App{
		appID:  appID,
		key:    key,
		secret: webhookSecret,
		botID:  botGitHubID,
		base:   apiBase(),
		http:   &http.Client{Timeout: 30 * time.Second},
		tokens: map[installationTokenKey]installationToken{},
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
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := responseError(method, path, resp); err != nil {
		return err
	}
	if out != nil {
		return decodeJSONLimited(resp.Body, out)
	}
	return nil
}

func decodeJSONLimited(r io.Reader, out any) error {
	b, err := io.ReadAll(io.LimitReader(r, maxJSONResponseBytes+1))
	if err != nil {
		return err
	}
	if len(b) > maxJSONResponseBytes {
		return ErrResponseTooLarge
	}
	return json.Unmarshal(b, out)
}

// responseError never returns the response body: GitHub may reflect untrusted
// request data there, and callers must not post or log it as a review failure.
func responseError(method, path string, resp *http.Response) error {
	if resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden &&
			(resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "")) {
		return &HTTPError{Method: method, Path: path, StatusCode: http.StatusTooManyRequests}
	}
	return &HTTPError{Method: method, Path: path, StatusCode: resp.StatusCode}
}

// ValidRepoFullName accepts the canonical GitHub owner/name form used in API
// paths and credential-bearing clone URLs. It deliberately excludes URL
// delimiters and whitespace so hostile metadata cannot escape a path.
func ValidRepoFullName(repo string) bool {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[0]) > 39 || len(parts[1]) == 0 || len(parts[1]) > 100 {
		return false
	}
	for _, part := range parts {
		for i := range part {
			c := part[i]
			if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '-' && c != '_' && c != '.' {
				return false
			}
		}
	}
	return true
}
