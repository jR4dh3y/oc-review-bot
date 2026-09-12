# Deployment and operations

This document describes the runtime contract for `samik-bot`. It is intentionally provider
neutral: deploy it on a host that can run the repository-pinned Go binary and the current supported
OpenCode 2 beta CLI, rather than assuming a Docker image or a particular PaaS integration.

## Before first deploy

1. Build the release artifact with `mise exec -- make build`. The resulting
   `bin/samik-bot` contains the compiled dashboard from `web/dist`.
2. Provision a Linux review sandbox before enabling webhooks. Install the current OpenCode 2 beta
   client and Bubblewrap on the **runtime** host. The integration invokes `opencode2`, not the V1
   `opencode` executable. Package the OpenCode executable and its package files under the absolute
   `OPENCODE_RUNTIME_DIR`; with the default `OPENCODE_BIN=opencode2`, the runner expects
   `$OPENCODE_RUNTIME_DIR/bin/opencode2`. An absolute custom `OPENCODE_BIN` must still resolve
   inside that runtime directory.

   The runner bind-mounts the runtime directory read-only, so deploy a trusted tree: it must contain
   no symlink, group-writable, or world-writable entry, and each entry must be owned by root or the
   service user. Set `BUBBLEWRAP_BIN` to a direct, non-symlink executable path that is likewise not
   group- or world-writable and is owned by root or the service user. Reviews fail closed when either
   trust check or the Bubblewrap launch fails; the service never falls back to direct OpenCode
   execution.

   Run `OPENCODE_BIN=/opt/opencode-runtime/bin/opencode2 mise exec -- make opencode-check` as the
   service user to check the non-interactive CLI entry point. It does not validate the Bubblewrap
   boundary, so test a disposable, authorized PR in staging before production. Use the current
   [OpenCode 2 beta installation guidance](https://opencode.ai/v2/docs); its beta docs explicitly
   warn that supported packaging options can change. `make setup` intentionally installs only the
   repository-pinned Go/Bun toolchain and application dependencies. Pin a known-good OpenCode beta
   package release in the host image or deployment configuration, record `opencode2 --version` with
   the application release, and promote that same version through staging before production. Do not
   make a moving beta tag an unattended production update.
3. Create the GitHub App and separate OAuth App as described in the README. Install the App only on
   repositories it is allowed to review. The webhook must reach
   `https://<public-host>/webhooks/github` over HTTPS.
4. Put every secret in the deployment platform's secret manager: the GitHub App private key,
   webhook secret, OAuth secret, and `SESSION_SECRET`. Never put them in an image, a repository,
   a command line, or application logs. A private-key file supplied through
   `GITHUB_APP_PRIVATE_KEY_PATH` should be readable only by the service account.
5. Set every required variable in the [configuration table](../README.md#configuration): GitHub App
   credentials, `SESSION_SECRET`, `ADMIN_GITHUB_IDS`, both GitHub target allowlists,
   `ZEN_DEFAULT_MODEL`, the active engine's runtime dir (`OPENCODE_RUNTIME_DIR` for the default
   `opencode2` engine, `PI_RUNTIME_DIR` when `REVIEW_ENGINE=pi`), and `BUBBLEWRAP_BIN`. Configure
   both OAuth values as well: startup permits them to be absent, but users cannot register or log
   in without them. Select a model that is currently enabled for the deployed gateway
   configuration (`opencode/…` models use OpenCode Zen, `orcarouter/…` models use OrcaRouter);
   model catalogs change, so do not rely on a historical README example. Do not set
   `ADMIN_GITHUB_LOGINS`; a non-empty legacy login allowlist is explicitly rejected.

## Runtime configuration

Run one service process with environment variables injected by the platform. Start the released
`bin/samik-bot` directly in production; `make run` is a source-checkout convenience that also
rebuilds the dashboard and therefore requires Go and Bun. `make run-local` is deliberately a
local-development convenience that sources `.env`. The binary itself does not read dotenv files.

The application always runs OpenCode 2 in standalone mode. `OPENCODE_BIN` must have the basename
`opencode2`; a relative name is resolved below `OPENCODE_RUNTIME_DIR/bin`, and an absolute path must
be inside that directory. `OPENCODE_RUNTIME_DIR` and `BUBBLEWRAP_BIN` are required configuration
values, not optional tuning knobs. Configuration loading detects missing paths; individual reviews
also verify the runtime tree and Bubblewrap capability before executing untrusted PR content.

### Reviewer engine selection

`REVIEW_ENGINE` selects the reviewer agent and its separate configuration; the default remains
`opencode2`. Setting `REVIEW_ENGINE=pi` activates the pi coding agent:

- `PI_BIN` (default `pi`) must have the basename `pi` and resolve inside `PI_RUNTIME_DIR`, exactly
  like the OpenCode contract. `PI_RUNTIME_DIR` is required for this engine; `OPENCODE_RUNTIME_DIR`
  is then not required and is never consulted, so both trees can stay staged side by side and the
  engine can be switched with one variable.
- The pi runtime tree satisfies the same read-only trust rules (no symlinked, group-writable, or
  world-writable entries). Because pi is a Node CLI, stage the `node` executable inside the tree
  and give `bin/pi` an absolute shebang such as `#!/opt/pi-runtime/bin/node`; the sandbox has no
  host `/usr/bin/env`. See the README's pi staging recipe.
- Startup preflight runs `pi --version` and `pi --list-models <gateway>` inside the same Bubblewrap
  profile used for reviews, where `<gateway>` is the provider prefix of the configured
  `ZEN_DEFAULT_MODEL` (`opencode` for Zen, `orcarouter` for OrcaRouter). The catalog probe proves
  the staged agent resolves that provider's credentials from the isolated credential store without
  calling a model; a model later selected in the dashboard for the other gateway is exercised at
  review time.
- Each pi review runs `pi --print` with a fixed, non-configurable flag set: model from the shared
  `provider/model` setting, a read-only tool allowlist (`read`, `grep`, `find`, `ls`), all
  project-local discovery disabled (extensions, skills, prompt templates, themes, context files,
  project trust), no session persistence, and a safety system-prompt appendix. The pooled key is
  written to an isolated `auth.json` under a per-run `PI_CODING_AGENT_DIR` tmpfs, keyed by the
  model's gateway provider (`{"opencode":{"type":"api_key", ...}}` for Zen; `orcarouter/…` models
  additionally receive a per-run `models.json` declaring the custom OrcaRouter provider), and is
  never passed through the child environment. Startup
  network noise (update checks, telemetry) is disabled with `PI_OFFLINE`, `PI_SKIP_VERSION_CHECK`,
  and `PI_TELEMETRY=0`; only model traffic to the configured gateway remains.
- pi terminal execution failures surface in the dashboard and logs with the `pi_execution` cause
  instead of `opencode_execution`; quota/rate-limit handling and key cooldown are identical.

Set the immutable numeric access boundary before accepting webhook traffic. `ADMIN_GITHUB_IDS` is
the authoritative dashboard-admin and review-requester allowlist; `REVIEWER_GITHUB_IDS` grants
additional users review access only. Each requester must still register through GitHub OAuth, and
every review target must match both `ALLOWED_GITHUB_INSTALLATION_IDS` and
`ALLOWED_GITHUB_REPOSITORY_IDS`. Use positive comma-separated GitHub numeric IDs, never mutable
logins; duplicate IDs and a non-empty `ADMIN_GITHUB_LOGINS` setting cause startup to fail. Set
`USER_REVIEWS_PER_HOUR`, `REPO_REVIEWS_PER_HOUR`, and `MAX_ACTIVE_REVIEWS` deliberately for the
organization's paid-review capacity (all must be at least one).

Use a canonical HTTPS `PUBLIC_URL` in production. The HTTP server does not terminate TLS, and its
OAuth/session cookies become `Secure` only when `PUBLIC_URL` begins with `https://`. Put it behind
a TLS-terminating ingress or reverse proxy, and prevent direct public access to the backend's plain
HTTP port. HTTP `localhost` is suitable only for local development.

The host process needs outbound access to GitHub for App/OAuth/API operations. The review sandbox
shares network access so OpenCode can reach its configured Zen/provider endpoint; Bubblewrap cannot
express hostname allowlists, so enforce provider-only sandbox egress with host or network policy.
The review runner makes a temporary checkout per job and invokes `opencode2` with isolated
configuration; allow enough temporary disk and process capacity for the largest PR that the service
policy accepts. Current defensive ceilings are a 2 MiB webhook body, a 10 MiB PR diff, rejection at
3,000 changed files, a 5 MiB checkout blob, 100 MiB total checkout content, 10,000 checkout files,
and 1 MiB each for agent stdout and stderr. Reviews beyond those limits fail rather than consuming
unbounded service resources.

## Persistent data and replicas

Set `DB_PATH` to an absolute path inside a mounted, durable directory, for example a platform volume
mounted at `/var/lib/samik-bot` with `DB_PATH=/var/lib/samik-bot/samik-bot.db`. Create
the parent directory before the service starts. Persist the whole directory, not only the main `.db`
file: SQLite runs in WAL mode and can create `-wal` and `-shm` sidecars.

Deploy a single application replica against a database volume. The built-in work queue and SQLite
locking are designed for one service process; a network filesystem or multiple pods sharing the
same SQLite file is not a supported high-availability topology. On startup, interrupted queued and
running reviews are requeued from the persistent database.

Pooled API keys are stored encrypted with AES-GCM using key material derived from `SESSION_SECRET`.
Keep the database and `SESSION_SECRET` together across restarts and restores. Rotating or losing
that secret without a key re-encryption migration makes the stored keys unrecoverable; retain a
controlled backup before making a planned secret change. The database is not otherwise encrypted
by the application, so use the platform's encrypted volume controls and narrow access to it.

For backups, use SQLite's backup mechanism or stop the service before copying data. If a filesystem
snapshot/copy is taken while the service is running, it must preserve the database, WAL, and shared
memory files consistently. Test restoration into an isolated environment before relying on a backup.

## Publication reconciliation runbook

The review and publication outboxes are durable, but a process can still lose its local
acknowledgement after GitHub accepts a comment. In that case the review is shown as
`reconciliation_required` and remains the active review for that immutable repository/PR pair. This
is an intentional quarantine: a new mention must not create a duplicate comment while the remote
result is uncertain.

1. Open the review in the dashboard and record the review ID, PR URL, status, error, and affected
   publication. For read-only inspection, query `reviews` and `review_publications` from a consistent
   SQLite backup or maintenance copy; do not edit the live database while the service is running.
2. Search the target PR's issue comments for the exact opaque marker, requiring both the service bot
   author and the marker value. The service performs the same bounded search automatically: at most
   50 pages of 100 comments for each issue or inline-comment endpoint. Do not search by summary text
   alone and do not create a replacement comment.
3. If the marker is found, leave the row quarantined. The worker retries marker reconciliation every
   five minutes and will record the verified GitHub comment ID, finish the publication, and release
   the review lock. Restarting the service is safe: startup recovery preserves uncertain sends until
   their bounded handoff window expires, then switches them to marker-only reconciliation.
4. If the marker is not found, treat delivery as unknown rather than as failed. Check GitHub audit
   logs, API responses, ingress/proxy logs, and the service log for the original request. Keep the
   review quarantined until the incident owner can account for the remote effect. Never delete the
   publication row, clear `reconciliation_required`, or manually post a replacement from an
   unverified copy of the result; any of those actions can create duplicate or stale review output.
   If the PR must proceed before the incident is resolved, quarantine the bot operationally for that
   repository and use a separate PR/revision according to the organization's incident policy.

There is deliberately no admin endpoint that marks an absent marker as safe: only an exact
bot-authored marker plus a GitHub comment ID can close the uncertainty. The dashboard status and
logs provide the operator handoff, while the durable worker supplies the only safe automatic repair.
Webhook delivery markers are retained for seven days and purged at most every six hours; that is
longer than GitHub's normal redelivery window. After retention expires, the delivery ID alone may be
accepted again, but immutable trigger and active-review uniqueness constraints still prevent the
same review event from creating an unsafe duplicate.

## Key-pool governance

OpenCode Zen is a usage-based provider that also lists rate-limited free models (IDs ending in
`-free`); free models still consume the pooled keys' request quota and rate limits. Add API keys
only from accounts that are authorized to share the same workload, budget, and data-access
boundary. The dashboard's per-key request counter is an operational scheduling metric; it is not a
provider billing record or a way to bypass provider controls.

The pool serves one gateway at a time: the configured review model's provider prefix selects it
(`opencode/…` for OpenCode Zen, `orcarouter/…` for OrcaRouter), and every key in the pool must be
issued by that gateway. Key selection is least-used across the whole pool, so mixing gateways in
one pool sends reviews with credentials the configured model cannot use; switch the model and the
pool together.

On a recognized quota, credit, or rate-limit response from either gateway, the service puts that
key on cooldown
for `ZEN_COOLDOWN_MINUTES` and can make one retry with another eligible key. Do not use multiple
accounts or keys to work around quotas, credits, rate limits, spend controls, or provider terms.
Configure the provider's own spend limits and monitor its billing dashboard independently.

Only configured administrators can add, disable, or delete pooled keys. Promptly disable a key in
the dashboard and rotate it at the provider if it is suspected of exposure. Do not put pooled
gateway keys in
`.env`, CI secrets, repository configuration, or review prompts.

## Monitoring and rollout

Probe `GET /healthz` for basic process liveness. It returns a static success response and does not
verify GitHub connectivity, the OpenCode binary, the Zen provider, or SQLite health, so also monitor
review completion/failure rates and the dashboard queue. Alert on repeated quota failures, failed
reviews, unavailable eligible keys, and unexpected restart loops. Terminal review failures log the
operator-safe cause plus a bounded, sanitized reviewer-stderr excerpt for agent and sandbox
failures; run with `LOG_LEVEL=debug` while diagnosing, then return to `info`.

Roll out OpenCode 2 beta upgrades first in a non-production environment with a funded,
organization-authorized test key and a disposable test PR. The beta CLI/configuration contract may
change independently of this repository. Keep the previous binary, OpenCode installation, database
backup, and `SESSION_SECRET` available until the smoke test completes.
