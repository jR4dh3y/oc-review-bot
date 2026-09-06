package gh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	filesPerPage    = 100
	maxPRFiles      = 3000
	maxDiffSize     = 10 << 20
	maxCommentPages = 50
)

// InstallationToken returns a valid repository-scoped installation access
// token, refreshing the cache shortly before expiry.
func (a *App) InstallationToken(ctx context.Context, installationID, repositoryID int64) (string, error) {
	if installationID < 1 || repositoryID < 1 {
		return "", errors.New("installation and repository IDs are required")
	}
	cacheKey := installationTokenKey{installationID: installationID, repositoryID: repositoryID}
	a.mu.Lock()
	if t, ok := a.tokens[cacheKey]; ok && time.Now().Before(t.expiresAt) {
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
	body, err := json.Marshal(map[string][]int64{"repository_ids": []int64{repositoryID}})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/app/installations/%d/access_tokens", a.base, installationID), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := responseError(http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installationID), resp); err != nil {
		return "", err
	}
	if err := decodeJSONLimited(resp.Body, &out); err != nil {
		return "", err
	}
	exp, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil || out.Token == "" {
		return "", errors.New("invalid installation token response")
	}
	a.mu.Lock()
	a.tokens[cacheKey] = installationToken{token: out.Token, expiresAt: exp.Add(-5 * time.Minute)}
	a.mu.Unlock()
	return out.Token, nil
}

// PR is the subset of a pull request the bot needs.
type PR struct {
	Number    int64  `json:"number"`
	Title     string `json:"title"`
	HTMLURL   string `json:"html_url"`
	UpdatedAt string `json:"updated_at"`
	Head      struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		SHA          string `json:"sha"`
		RepositoryID int64  `json:"id"`
		FullName     string `json:"full_name"`
		Repo         struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	Draft bool `json:"draft"`
	User  struct {
		Login string `json:"login"`
	} `json:"user"`
}

// RevisionToken is a conservative provider revision token. GitHub's PR head
// SHA is not sufficient for ABA protection: a force-push can return to the
// same SHA while the PR's mutable revision metadata changes. UpdatedAt is
// included when available; fixtures and compatible providers without it still
// receive a stable SHA-based token.
func (p *PR) RevisionToken() string {
	if p == nil {
		return ""
	}
	material := p.Head.SHA + "\x00" + p.Base.SHA + "\x00" + p.Head.Ref
	if strings.TrimSpace(p.UpdatedAt) != "" {
		material = p.UpdatedAt + "\x00" + material
	}
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

// TargetRepositoryID returns the immutable repository ID from the PR base
// repository. GitHub nests this data under base.repo; the direct fields keep
// fixtures and alternate API-compatible responses easy to decode.
func (p *PR) TargetRepositoryID() int64 {
	if p == nil {
		return 0
	}
	if p.Base.RepositoryID != 0 {
		return p.Base.RepositoryID
	}
	return p.Base.Repo.ID
}

// TargetRepositoryFullName returns the current canonical owner/name for the
// PR base repository after its immutable ID has been checked.
func (p *PR) TargetRepositoryFullName() string {
	if p == nil {
		return ""
	}
	if p.Base.FullName != "" {
		return p.Base.FullName
	}
	return p.Base.Repo.FullName
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

// ListFiles returns the changed files of a PR, up to GitHub's 3,000-file cap.
func (a *App) ListFiles(ctx context.Context, token, repo string, number int64) ([]File, error) {
	var out []File
	for page := 1; page <= maxPRFiles/filesPerPage+1; page++ {
		var batch []File
		path := fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=%d&page=%d", repo, number, filesPerPage, page)
		if err := a.doJSON(ctx, token, http.MethodGet, path, nil, &batch); err != nil {
			return nil, err
		}
		if len(out)+len(batch) > maxPRFiles {
			return nil, ErrResponseTooLarge
		}
		out = append(out, batch...)
		if len(batch) < filesPerPage {
			break
		}
	}
	if len(out) == maxPRFiles {
		// A PR at the API cap cannot be reviewed completely and safely.
		return nil, ErrResponseTooLarge
	}
	return out, nil
}

// GetDiff returns the PR's unified diff within the configured safety limit.
func (a *App) GetDiff(ctx context.Context, token, repo string, number int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/repos/%s/pulls/%d", a.base, repo, number), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github.diff")
	req.Header.Set("User-Agent", userAgent)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := responseError(http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), resp); err != nil {
		return nil, err
	}
	if resp.ContentLength > maxDiffSize {
		return nil, ErrResponseTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDiffSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxDiffSize {
		return nil, ErrResponseTooLarge
	}
	return b, nil
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

type remoteComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		ID int64 `json:"id"`
	} `json:"user"`
}

// FindIssueCommentMarker returns the service-owned issue comment carrying an
// opaque marker. It pages with a hard bound so hostile repositories cannot
// turn reconciliation into unbounded memory or API work.
func (a *App) FindIssueCommentMarker(ctx context.Context, token, repo string, issue int64, marker string) (int64, bool, error) {
	return a.findCommentMarker(ctx, token,
		func(page int) string {
			return fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=%d&page=%d&sort=created&direction=desc", repo, issue, filesPerPage, page)
		}, marker)
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

// FindReviewCommentMarker returns the service-owned changed-line review
// comment carrying an opaque marker, using the same bounded reconciliation.
func (a *App) FindReviewCommentMarker(ctx context.Context, token, repo string, pr int64, marker string) (int64, bool, error) {
	return a.findCommentMarker(ctx, token,
		func(page int) string {
			return fmt.Sprintf("/repos/%s/pulls/%d/comments?per_page=%d&page=%d&sort=created&direction=desc", repo, pr, filesPerPage, page)
		}, marker)
}

func (a *App) findCommentMarker(ctx context.Context, token string, pagePath func(int) string, marker string) (int64, bool, error) {
	if marker == "" {
		return 0, false, errors.New("comment marker is required")
	}
	for page := 1; page <= maxCommentPages; page++ {
		var comments []remoteComment
		if err := a.doJSON(ctx, token, http.MethodGet, pagePath(page), nil, &comments); err != nil {
			return 0, false, err
		}
		for _, comment := range comments {
			if comment.ID > 0 && comment.User.ID == a.botID && strings.Count(comment.Body, marker) == 1 && strings.Contains(comment.Body, marker) {
				return comment.ID, true, nil
			}
		}
		if len(comments) < filesPerPage {
			return 0, false, nil
		}
	}
	return 0, false, ErrResponseTooLarge
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
	req.Header.Set("User-Agent", userAgent)
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := responseError(http.MethodPost, "/login/oauth/access_token", resp); err != nil {
		return "", errors.New("oauth token exchange failed")
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := decodeJSONLimited(resp.Body, &out); err != nil || out.AccessToken == "" {
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
