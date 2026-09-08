# oc-review-bot

Greptile/CodeRabbit-style PR review bot. Mention `@oc-review-bot` in a PR comment and it runs
[OpenCode 2 beta](https://opencode.ai/v2/docs) (`opencode2`) against the PR diff using a pool of
organization-authorized OpenCode Zen API keys, then posts one summary comment plus inline findings
pinned to diff lines. The summary includes a Mermaid sequence diagram and precedes the inline
findings. A React dashboard handles registration and administrator key management. One Go binary
serves everything.

> **OpenCode and Zen status.** OpenCode 2 is beta software and its CLI/configuration can change.
> This service invokes an externally installed `opencode2` binary; it is not bundled with the Go or
> Bun dependencies. [OpenCode Zen](https://opencode.ai/docs/zen/) is pay-as-you-go, not a free tier.
> Add only keys your organization is authorized to operate. The pool must not be used to evade
> provider credits, rate limits, spend limits, or terms of service.

## How it works

1. GitHub webhook `issue_comment.created` → verify HMAC → body mentions `@<BOT_USERNAME>` →
   commenter must have registered in the dashboard → their immutable numeric GitHub user ID must
   be in `ADMIN_GITHUB_IDS` or `REVIEWER_GITHUB_IDS`, and the target installation and repository
   IDs must both be allowlisted → enqueue (one active review per PR; reacts 👀). Unregistered
   commenters receive a registration nudge; registered but unauthorized commenters receive an
   access nudge.
2. Worker: pick the eligible Zen key with the fewest recorded requests today (skipping
   cooling-down/disabled keys)
   → shallow-clone the PR head → fetch the diff via the GitHub API → write an isolated
   `opencode.json` and `auth.json` (the key exists only in the isolated auth store) in a temp XDG
   dir → run
   `opencode2 run` in standalone mode with the review prompt (the agent reads repository files
   itself), selected model, JSON output, and a hard timeout.
3. Parse the agent's final message: last fenced JSON block `{summary, sequence_diagram, findings:[{path,
   line, side, severity, body}]}` (tolerant — plain text gets a safe fallback diagram) → map findings
   to diff lines via the PR file list (skip findings outside the diff) → post the summary with its
   Mermaid diagram first, then inline review comments → react 🚀 on success, or react 👎 and record a
   failure cause on terminal failure → record per-key usage. A
   402/429/quota failure cools that key for `ZEN_COOLDOWN_MINUTES` and may retry once with another
   eligible, authorized key.

Terminal failures mark the review `failed` in the dashboard with an operator-safe cause (for example
`opencode_execution`, `sandbox_unavailable`, `github_http_404`) so the PR's review slot is released
and the requester can mention the bot again. The service log gains the cause plus, for agent and
sandbox failures, a bounded sanitized excerpt of the reviewer's stderr; set `LOG_LEVEL=debug` for
engine-internal detail. Shutdown interruptions are not failures: those rows are requeued durably by
the next startup.

## Local setup

Install [Mise](https://mise.jdx.dev/) first. The repository pins Go and Bun in `mise.toml`; do not
rely on whichever versions happen to be globally installed. Install the current supported OpenCode
2 beta CLI separately, package it with its dependencies in a trusted runtime directory, and install
Bubblewrap on the Linux host before attempting a live review. The default `OPENCODE_BIN=opencode2`
is resolved as `$OPENCODE_RUNTIME_DIR/bin/opencode2`, not from an arbitrary application `PATH`.
Follow the [OpenCode 2 beta installation guide](https://opencode.ai/v2/docs) instead of assuming a
V1 `opencode` installation or an unsupported beta packaging method.

`make setup` installs the repository-pinned Go/Bun toolchain and application dependencies only; it
intentionally does not install the external OpenCode beta CLI, Bubblewrap, or any credentials. After
staging `opencode2`, confirm the non-interactive automation entry point before configuring the
service:

```bash
make setup
OPENCODE_BIN=/opt/opencode-runtime/bin/opencode2 mise exec -- make opencode-check
cp .env.example .env
# Edit .env with the required GitHub, ID allowlist, sandbox, session, and model values.
mise exec -- make run-local
```

`opencode-check` runs `opencode2 --version` and `opencode2 run --help`; it does not call a model,
validate the Bubblewrap sandbox, or need a Zen API key. `make run-local` loads `.env` only for a
local POSIX-shell run. The binary itself reads process environment variables and never parses `.env`;
production must inject values through its secret manager. The web UI is at `$PUBLIC_URL` (`/`
landing, `/dashboard` reviews, `/admin/keys`, `/admin/settings`). Health check: `GET /healthz` →
`{"status":"ok"}`.

Build and verify the frontend plus single binary:

```bash
mise exec -- make check
mise exec -- make build
```

The binary is written to `bin/oc-review-bot`. `web/dist` is the Vite production build embedded into
the binary (`web/embed.go`, SPA fallback in `internal/server`). Go builds omit local source paths and
VCS checkout state. For frontend development, keep `mise exec -- make run-local` running in one
terminal, then run `mise exec -- make web-dev` in a second terminal; Vite listens on `:5173` and
proxies `/api`, `/auth`, `/healthz`, and `/webhooks` to the local Go server on `:8080`.

## GitHub App setup (click-by-click)

You need **two** GitHub-side registrations: a GitHub App (webhooks and review comments) and a
GitHub OAuth App (dashboard login). They are separate registrations.

### 1. Create the GitHub App

1. GitHub → Settings (your org/user) → Developer settings → GitHub Apps → New GitHub App.
2. Name: `oc-review-bot` (must be globally unique; add a suffix if taken).
3. Homepage URL: your `PUBLIC_URL` (e.g. `https://bot.example.com`).
4. Webhook URL: `https://<host>/webhooks/github`. Webhook secret: generate
   (`openssl rand -hex 32`) and save as `GITHUB_WEBHOOK_SECRET`.
5. Repository permissions: `Contents: read`, `Pull requests: read & write`, and `Issues: read &
   write`. The Issues permission covers issue/PR summary comments and comment reactions; Metadata
   is provided automatically.
6. Subscribe to the `Issue comment` event. The current implementation is mention-triggered and
   ignores other webhook event types.
7. Create → note the **App ID** → `GITHUB_APP_ID`.
8. Generate a private key → save the `.pem` → `GITHUB_APP_PRIVATE_KEY_PATH`.
9. Install the App on the repos you want reviewed (App page → Install App). Record the immutable
   numeric installation and repository IDs from GitHub's API or delivery metadata, then set
   `ALLOWED_GITHUB_INSTALLATION_IDS` and `ALLOWED_GITHUB_REPOSITORY_IDS` before enabling reviews.

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

Fill `.env` (all names in `.env.example`), then run `mise exec -- make run-local`. Set
`ADMIN_GITHUB_IDS` to one or more trusted numeric GitHub user IDs **before** starting the service;
there is no first-user administrator bootstrap. Add optional review-only IDs with
`REVIEWER_GITHUB_IDS`. Every requester, including an administrator, must log into the dashboard once
with GitHub before the bot will review their PR, and every request must match both target allowlists.
Do not set `ADMIN_GITHUB_LOGINS`: a non-empty legacy login allowlist is rejected at startup. Add
organization-authorized Zen keys at `/admin/keys`. If the GitHub App login differs from the default,
set `BOT_USERNAME` to its mentionable login without a leading `@`.

### 4. Try it

Open a PR on an installed repo, comment `@oc-review-bot review please`, expect a 👀 reaction,
then a summary comment with a Mermaid sequence diagram followed by inline findings. Unregistered
commenters get a register-here reply.

## Configuration

| Var | Required | Default | Notes |
|---|---|---|---|
| `PORT` | no | `8080` | HTTP listen port |
| `PUBLIC_URL` | no | `http://localhost:$PORT` | Absolute HTTP(S) URL without credentials, query, or fragment; HTTPS is required outside loopback development |
| `DB_PATH` | no | `oc-review-bot.db` | SQLite database path; production must place its directory on persistent storage |
| `GITHUB_APP_ID` | yes | — | GitHub App ID |
| `GITHUB_APP_PRIVATE_KEY` / `GITHUB_APP_PRIVATE_KEY_PATH` | yes (at least one) | — | Inline PEM or path; a non-empty file path takes precedence |
| `GITHUB_WEBHOOK_SECRET` | yes | — | HMAC secret for `/webhooks/github` |
| `GITHUB_OAUTH_CLIENT_ID` / `GITHUB_OAUTH_CLIENT_SECRET` | for a usable website | — | OAuth app; login routes return 503 without both values |
| `SESSION_SECRET` | yes | — | Use 32+ random bytes to derive Zen-key encryption; retain it while the DB exists |
| `ADMIN_GITHUB_IDS` | yes | — | Comma-separated immutable numeric GitHub user IDs; administrators can manage the dashboard and request reviews |
| `REVIEWER_GITHUB_IDS` | no | empty | Additional comma-separated numeric GitHub user IDs that can request, but not administer, reviews |
| `ALLOWED_GITHUB_INSTALLATION_IDS` | yes | — | Comma-separated numeric GitHub App installation IDs; each review target must match |
| `ALLOWED_GITHUB_REPOSITORY_IDS` | yes | — | Comma-separated numeric GitHub repository IDs; each review target must match |
| `BOT_USERNAME` | no | `oc-review-bot` | Valid GitHub login without `@`; mention trigger is case-insensitive |
| `ZEN_DEFAULT_MODEL` | yes | — | Current enabled `provider/model` identifier; initial value, overridable at `/admin/settings` |
| `REVIEW_CONCURRENCY` | no | `2` | Worker pool size; must be at least 1 |
| `REVIEW_TIMEOUT_MINUTES` | no | `20` | Per-review hard timeout; must be at least 1 |
| `ZEN_COOLDOWN_MINUTES` | no | `60` | Cooldown after a Zen quota/rate-limit error; must be at least 1 |
| `USER_REVIEWS_PER_HOUR` | no | `6` | Per-requester admission limit; must be at least 1 |
| `REPO_REVIEWS_PER_HOUR` | no | `30` | Per-repository admission limit; must be at least 1 |
| `MAX_ACTIVE_REVIEWS` | no | `50` | Maximum queued or running reviews; must be at least 1 |
| `OPENCODE_BIN` | no | `opencode2` | Must name an OpenCode 2 `opencode2` executable; an absolute path must be inside the runtime directory |
| `OPENCODE_RUNTIME_DIR` | yes | — | Absolute trusted runtime root; the default binary is `$OPENCODE_RUNTIME_DIR/bin/opencode2` |
| `BUBBLEWRAP_BIN` | yes | — | Bubblewrap executable path or command resolving to a trusted executable |
| `LOG_LEVEL` | no | `info` | Service log level: `debug`, `info`, `warn`, or `error` |

GitHub ID lists accept positive decimal IDs separated by commas (whitespace is allowed); duplicates
and login names are rejected. Obtain the numeric `id` values from GitHub API responses or webhook
delivery metadata, not from a mutable login or GraphQL node ID. A non-empty `ADMIN_GITHUB_LOGINS`
value is rejected as unsafe.

Every review runs under a mandatory Bubblewrap boundary. At review time, the runner requires a
trusted runtime directory with no symlinked, group-writable, or world-writable entries, and a trusted
non-symlink Bubblewrap executable. It bind-mounts the runtime and PR checkout read-only; it never
falls back to executing OpenCode directly when that boundary cannot be established.

Zen API keys are intentionally entered by an authenticated administrator at `/admin/keys`, not via
an environment variable. The database stores them encrypted, but the database and `SESSION_SECRET`
must be retained together; replacing the secret makes existing stored keys unreadable.

## Deployment and operations

Read [deployment and operations](docs/operations.md) before exposing the webhook. It covers HTTPS,
secret handling, persistent SQLite storage, supported OpenCode 2 beta runtime expectations, key-pool
governance, backups, and the single-replica deployment constraint.

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
- `internal/pool` — least-used eligible-key selection + configurable cooldown; `internal/runner` —
  shallow clone + isolated `opencode2` exec + `--format json` text extraction
- `internal/review` — prompt contract, tolerant findings parser, diff→line mapping
- `internal/bot` — engine: fetch → runWithPool (one quota retry) → prepare and post the summary
  publication before inline findings → finish/fail
- `web/` — Vite + React + TS + TanStack Router/Query + shadcn-style UI (see above)

## Verification

- `mise exec -- make check` runs Go formatting verification, `go vet`, Go tests, the frontend
  build, and a production binary build.
- A live review can incur OpenCode Zen charges. Use an authorized funded test key, add it at
  `/admin/keys`, comment `@oc-review-bot` on a test PR, and watch `/dashboard` go queued →
  running → done (or failed, with the cause on the review detail page).
