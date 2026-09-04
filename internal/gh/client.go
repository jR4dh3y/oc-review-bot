package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// InstallationToken returns a valid installation access token, refreshing
// the cache shortly before expiry.
func (a *App) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	a.mu.Lock()
	if t, ok := a.tokens[installationID]; ok && time.Now().Before(t.expiresAt) {
		tok := t.token
		a.mu.Unlock()
		return tok, nil
	}
	a.mu.Unlock()

	jwt, err := a.SignAppJWT()
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/app/installations/%d/access_tokens", a.base, installationID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b := make([]byte, 1024)
		n, _ := resp.Body.Read(b)
		return "", fmt.Errorf("installation token: %d: %s", resp.StatusCode, b[:n])
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	exp, _ := time.Parse(time.RFC3339, out.ExpiresAt)
	a.mu.Lock()
	a.tokens[installationID] = installationToken{token: out.Token, expiresAt: exp.Add(-5 * time.Minute)}
	a.mu.Unlock()
	return out.Token, nil
}

// PR is the subset of a pull request the bot needs.
type PR struct {
	Number  int64  `json:"number"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
	Draft bool `json:"draft"`
	User  struct {
		Login string `json:"login"`
	} `json:"user"`
}

// GetPR fetches a pull request.
func (a *App) GetPR(ctx context.Context, token, repo string, number int64) (*PR, error) {
	var pr PR
	if err := a.doJSON(ctx, token, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// File is one changed file with its unified diff patch.
type File struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Patch     string `json:"patch"`
}

// ListFiles returns the changed files of a PR (up to 300).
func (a *App) ListFiles(ctx context.Context, token, repo string, number int64) ([]File, error) {
	var out []File
	for page := 1; page <= 3; page++ {
		var batch []File
		path := fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100&page=%d", repo, number, page)
		if err := a.doJSON(ctx, token, http.MethodGet, path, nil, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return out, nil
}

// GetDiff returns the PR's full unified diff.
func (a *App) GetDiff(ctx context.Context, token, repo string, number int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/repos/%s/pulls/%d", a.base, repo, number), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github.diff")
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("get diff: %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 10<<20))
}

// CreateIssueComment posts a general PR comment and returns its id.
func (a *App) CreateIssueComment(ctx context.Context, token, repo string, issue int64, body string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	bodyObj := map[string]string{"body": body}
	if err := a.doJSON(ctx, token, http.MethodPost,
		fmt.Sprintf("/repos/%s/issues/%d/comments", repo, issue), bodyObj, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// ReviewComment is a comment pinned to a diff line. StartLine 0 means single-line.
type ReviewComment struct {
	CommitID  string
	Path      string
	Side      string // "RIGHT" (new code) or "LEFT" (old code)
	Line      int64
	StartLine int64
	Body      string
}

// CreateReviewComment posts an inline PR comment on a diff line.
func (a *App) CreateReviewComment(ctx context.Context, token, repo string, pr int64, c ReviewComment) (int64, error) {
	reqBody := map[string]any{
		"body":      c.Body,
		"commit_id": c.CommitID,
		"path":      c.Path,
		"side":      c.Side,
		"line":      c.Line,
	}
	if c.StartLine > 0 {
		reqBody["start_line"] = c.StartLine
		reqBody["start_side"] = c.Side
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := a.doJSON(ctx, token, http.MethodPost,
		fmt.Sprintf("/repos/%s/pulls/%d/comments", repo, pr), reqBody, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// ReactToIssueComment adds a reaction to a PR comment.
func (a *App) ReactToIssueComment(ctx context.Context, token, repo string, commentID int64, content string) error {
	return a.doJSON(ctx, token, http.MethodPost,
		fmt.Sprintf("/repos/%s/issues/comments/%d/reactions", repo, commentID),
		map[string]string{"content": content}, nil)
}

// OAuthUser is the GitHub account behind an OAuth login.
type OAuthUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

// ExchangeOAuthCode swaps an OAuth code for a user access token.
func (a *App) ExchangeOAuthCode(ctx context.Context, clientID, clientSecret, code, redirectURI string) (string, error) {
	form := map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
		"code":          code,
	}
	if redirectURI != "" {
		form["redirect_uri"] = redirectURI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://github.com/login/oauth/access_token", strings.NewReader(encodeForm(form)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.AccessToken == "" {
		return "", errors.New("oauth token exchange failed")
	}
	return out.AccessToken, nil
}

// GetOAuthUser fetches the authenticated user for an OAuth token.
func (a *App) GetOAuthUser(ctx context.Context, accessToken string) (*OAuthUser, error) {
	var u OAuthUser
	if err := a.doJSON(ctx, accessToken, http.MethodGet, "/user", nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}
