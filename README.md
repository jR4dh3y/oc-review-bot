# oc-review-bot

Greptile/CodeRabbit-style PR review bot. Mention `@oc-review-bot` in a PR comment and it runs
[OpenCode 2.0](https://opencode.ai) (`opencode2`) against the PR diff using a pooled set of
OpenCode Zen API keys, then posts one summary comment plus inline findings pinned to diff lines.
A React dashboard handles registration and admin key management. One Go binary serves everything.

## How it works

1. GitHub webhook `issue_comment.created` → verify HMAC → body mentions `@<BOT_USERNAME>` →
   commenter's GitHub login must exist in `users` (else the bot replies once per PR with a
   register-here nudge) → enqueue (one active review per PR; reacts 👀).
2. Worker: pick the Zen key with the fewest requests today (skipping cooling-down/disabled keys)
   → shallow-clone the PR head → fetch the diff via the GitHub API → write an isolated
   `opencode.json` (key injected via `{env:...}` + `auth.json`) in a temp XDG dir → run
   `opencode2 run --model <model> --format json` with the review prompt (the agent reads repo
   files itself), `--standalone`, hard timeout.
3. Parse the agent's final message: last fenced JSON block `{summary, findings:[{path, line,
   side, severity, body}]}` (tolerant — plain text becomes summary-only) → map findings to diff
   lines via the PR file list (skip findings outside the diff) → post inline review comments +
   summary comment → react 🚀/❌ → record per-key usage. Quota failures (429/402/quota markers)
   cool that key down until UTC midnight and retry once with the next key.

## Quick start

```bash
cp .env.example .env   # fill in the GitHub App + OAuth + SESSION_SECRET values below
go run ./cmd/oc-review-bot
```

Web UI at `$PUBLIC_URL` (`/` landing, `/dashboard` reviews, `/admin/keys`, `/admin/settings`).
Health check: `GET /healthz` → `{"status":"ok"}`.

Build the frontend + single binary:

```bash
cd web && bun install && bun run build && cd ..
go build -o oc-review-bot ./cmd/oc-review-bot
```

`web/dist` is the Vite production build, embedded into the binary (`web/embed.go`, SPA fallback
in `internal/server`). Dev mode proxies `/api`, `/auth`, `/healthz`, `/webhooks` to `:8080`
(see `web/vite.config.ts`), so `bun run dev` works against a local Go server.

## GitHub App setup (click-by-click)

You need **two** GitHub-side registrations: a GitHub App (webhooks + review comments) and an
OAuth app (dashboard login). They can share one OAuth client only if you create a separate
OAuth App — GitHub Apps have OAuth too, but a standalone OAuth App keeps callback URLs simple.

### 1. Create the GitHub App

1. GitHub → Settings (your org/user) → Developer settings → GitHub Apps → New GitHub App.
2. Name: `oc-review-bot` (must be globally unique; add a suffix if taken).
3. Homepage URL: your `PUBLIC_URL` (e.g. `https://bot.example.com`).
4. Webhook URL: `https://<host>/webhooks/github`. Webhook secret: generate
   (`openssl rand -hex 32`) and save as `GITHUB_WEBHOOK_SECRET`.
5. Permissions: `Contents: read`, `Pull requests: read & write`, `Issues: read & write`
   (issue comments arrive as issue events), `Reactions: write`.
6. Subscribe to events: `Issue comment`, `Pull request` (used for future auto-review; the
   trigger today is the comment mention).
7. Create → note the **App ID** → `GITHUB_APP_ID`.
8. Generate a private key → save the `.pem` → `GITHUB_APP_PRIVATE_KEY_PATH`.
9. Install the App on the repos you want reviewed (App page → Install App). Note: each install
   has its own installation ID, resolved per-webhook automatically.

### 2. Create the OAuth App (dashboard login)

1. Developer settings → OAuth Apps → New OAuth App.
2. Homepage URL: `PUBLIC_URL`. Authorization callback URL:
   `https://<host>/auth/github/callback`.
3. Note **Client ID** → `GITHUB_OAUTH_CLIENT_ID`, generate **Client secret** →
   `GITHUB_OAUTH_CLIENT_SECRET`.

### 3. Configure + run

```bash
SESSION_SECRET=$(openssl rand -hex 32)   # 32+ random bytes
```

Fill `.env` (all names in `.env.example`), then `go run ./cmd/oc-review-bot`.
Register by logging into the dashboard once with GitHub, then set `ADMIN_GITHUB_LOGINS`
(or leave unset — the very first user becomes admin). Add Zen keys at `/admin/keys`.

### 4. Try it

Open a PR on an installed repo, comment `@oc-review-bot review please`, expect a 👀 reaction,
then a summary comment + inline findings. Unregistered commenters get a register-here reply.

## Configuration

| Var | Required | Default | Notes |
|---|---|---|---|
| `PORT` | no | `8080` | HTTP listen port |
| `PUBLIC_URL` | no | `http://localhost:$PORT` | Used for OAuth redirects + cookie `Secure` flag |
| `DB_PATH` | no | `oc-review-bot.db` | SQLite (WAL, CGO-free via `modernc.org/sqlite`) |
| `GITHUB_APP_ID` | yes | — | GitHub App ID |
| `GITHUB_APP_PRIVATE_KEY` / `GITHUB_APP_PRIVATE_KEY_PATH` | yes (one) | — | PEM literal or file path |
| `GITHUB_WEBHOOK_SECRET` | yes | — | HMAC secret for `/webhooks/github` |
| `GITHUB_OAUTH_CLIENT_ID` / `SECRET` | for login | — | OAuth app; login routes 503 without them |
| `SESSION_SECRET` | yes | — | Key encryption at rest + session signing |
| `ADMIN_GITHUB_LOGINS` | no | first user | Comma-separated logins forced to admin |
| `BOT_USERNAME` | no | `oc-review-bot` | Mention trigger `@<name>` (case-insensitive) |
| `ZEN_DEFAULT_MODEL` | no | `opencode/big-pickle` | Overridable at `/admin/settings` (stored in DB) |
| `REVIEW_CONCURRENCY` | no | `2` | Worker pool size |
| `REVIEW_TIMEOUT_MINUTES` | no | `20` | Per-review hard timeout |
| `OPENCODE_BIN` | no | `opencode2` | Agent binary; `--standalone` auto-added for v2 |

## API (dashboard)

All JSON, session cookie `oc_review_session`:

- `GET /api/me` → `{id, login, avatar_url, is_admin}`
- `GET /api/reviews` → last 50 `[{id, repo_full, pr_number, head_sha, requester_login, status, model, summary_md, error, summary_comment_id, created_at, findings_count}]`
- `GET /api/reviews/{id}` → `{review, findings: [{path, line, side, severity, body, posted_comment_id}]}`
- Admin: `GET/POST /api/admin/keys`, `PATCH/DELETE /api/admin/keys/{id}` (`{disabled}`),
  `GET/POST /api/admin/settings` (`{model}`). Keys are never returned unmasked (only `last4`).

## Repo layout

- `cmd/oc-review-bot/main.go` — wiring: config → App auth → store → key pool → engine → server
- `internal/config` — env parsing; `internal/store` — SQLite + migrations
  (`users`, `sessions`, `zen_keys`, `reviews`, `findings`, `nudges`, `settings`)
- `internal/gh` — App JWT → installation token, REST (PR/files/diff/comments/reactions),
  webhook HMAC, OAuth exchange; `internal/server` — webhook handler, OAuth, admin JSON API, SPA
- `internal/pool` — least-used-today rotation + UTC-midnight cooldown; `internal/runner` —
  shallow clone + isolated `opencode2` exec + `--format json` text extraction
- `internal/review` — prompt contract, tolerant findings parser, diff→line mapping
- `internal/bot` — engine: fetch → runWithPool (one quota retry) → post → finish/fail
- `web/` — Vite + React + TS + TanStack Router/Query + shadcn-style UI (see above)

## Verification

- `go vet ./...` and `go test ./...` cover pool rotation/cooldown, findings parser, diff-line
  mapping, runner JSONL extraction, webhook HMAC + nudge/enqueue flows, admin key round-trip.
- `cd web && bun run build` must pass (`tsc -b && vite build`); `go build ./...` then embeds it.
- Live check costs one free-tier Zen request: add a key at `/admin/keys`, comment
  `@oc-review-bot` on a test PR, watch `/dashboard` go queued → running → done.

Built with opencode-zen/muse-spark via the Oh My Pi coding harness.
