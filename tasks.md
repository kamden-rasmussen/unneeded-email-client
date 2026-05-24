# Email Agent — Task Tracker

_Last updated: 2026-05-24_

---

## Immediate Actions Required

These are blocking or security-critical — do these before anything else.

- [ ] **Rotate the Gmail OAuth client secret** — `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` was hardcoded in `email/gmail.go` inside commit `def3754` on the pushed `feature/multiple-accounts` branch. It is in GitHub's history. Go to Google Cloud Console → APIs & Services → Credentials and rotate it now.
- [ ] **Rotate the second client secret** — `credentials.json` on disk (not committed) contains a different secret: `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`. Same key; rotate it above and this is covered. But verify both `credentials.json` and the env var are using the new secret after rotation.
- [ ] **Create a new Google Cloud OAuth app** — needs to be **Web application** type (not Desktop) so the server-side redirect URI `https://email-agent.nocandstar.cc/setup/gmail/callback` is accepted. Add that URI as an authorized redirect URI.
- [ ] **Update `data/.env`** with new `GMAIL_CLIENT_ID` and `GMAIL_CLIENT_SECRET` after rotation.
- [ ] **Re-authorize Gmail** — token in `data/gmail_token.json` is expired (`invalid_grant`). After new credentials are in `data/.env`, trigger re-auth: the bot will post a DM link automatically on next digest attempt, or restart the container and let it detect the invalid_grant.
- [ ] **Merge `feature/multiple-accounts` → `main`** (open a PR). The branch is ahead of main by 2 commits and all the real work lives there. Consider a history rewrite (`git filter-repo` or `BFG`) to scrub the hardcoded secret from `def3754` before merging, or at minimum accept that the old commit hash will remain and the rotated credential mitigates it.

---

## Done

### Core email fetching
- [x] Gmail client — fetches inbox via Gmail API, metadata-only (From/Subject/Date/Snippet)
- [x] IMAP client — connects via TLS, searches by date with `UidSearch`, fetches envelope
- [x] Multi-account support — `config.yaml` accounts list, each account fetched independently
- [x] Account-agnostic message IDs — `MsgID` field holds Gmail hex ID or IMAP UID string; `ID` field carries the `gmail-<id>` / `imap-<name>-<uid>` composite for dedup

### Email actions (IMAP + Gmail)
- [x] `Archive` — Gmail removes INBOX label; IMAP copies to archive mailbox then sets `\Deleted`
- [x] `MarkRead` — Gmail removes UNREAD label; IMAP sets `\Seen` flag via `UidStore`
- [x] `MoveToLabel` — Gmail adds label + removes INBOX; IMAP copies to named mailbox
- [x] `ListLabels` — Gmail returns user-type labels only; IMAP lists mailboxes excluding system folders
- [x] `ArchiveMailbox` config field for IMAP accounts (defaults to `"Archive"`)

### Mattermost bot
- [x] Digest post with per-email attachment cards and action buttons (Archive / Mark Read / Move...)
- [x] Plain-text fallback digest when no `callback_url` is configured
- [x] Number-based alphanumeric action IDs (`ar1`, `rd1`, `mo1`, `sg1`) — works for any account type and satisfies Mattermost's router requirements
- [x] `attachmentOwnsNumber` exact match — checks `id[2:] == numStr` to avoid false matches at 10+ emails
- [x] Move dialog — "Move..." button fetches labels, opens Mattermost interactive dialog with select dropdown
- [x] Cancelled dialog fix — `handleMoveDialog` returns `200 OK` on `cancelled: true` instead of error
- [x] Webhook secret — all action endpoints validate `webhook_secret` from context using constant-time compare
- [x] Body size limit — `http.MaxBytesReader` at 1 MB on `/actions/email` and `/actions/move_dialog`
- [x] Post patch after action — digest attachment updated in place to remove buttons and show completion label

### Poll loop / commands
- [x] Background poll loop watching DM channel every `poll_interval` seconds (default 30s)
- [x] Poll cursor initialized on startup (no missed commands after first boot)
- [x] `check` / `digest` command triggers an on-demand digest run
- [x] `archive <n> [n...]` / `archive all` text command
- [x] `read <n> [n...]` / `read all` text command
- [x] `done <n>` — archives and marks read in one command
- [x] Digest email number-to-MsgID mapping persisted in SQLite `digest_emails` table

### Gmail OAuth
- [x] `email-agent auth [--port N] gmail <name>` CLI subcommand — browser OAuth flow, saves token to `<name>_token.json`
- [x] `--port N` flag for fixed callback port (SSH tunnel / Docker port mapping)
- [x] Gmail client secret moved out of binary to `GMAIL_CLIENT_ID` / `GMAIL_CLIENT_SECRET` env vars (read from `data/.env` via `docker-compose.yml`)
- [x] `invalid_grant` detection in digest loop — calls `reauthFn` which posts DM with re-auth link
- [x] `StartGmailReauth` — generates random state, stores pending OAuth in-memory with 10-minute TTL
- [x] `/setup/gmail/callback` HTTP handler — exchanges code, saves new token, posts confirmation DM

### Label suggestions
- [x] `InferLabel` on `GmailClient` — queries Gmail for recently organized messages from sender domain, picks most common user label
- [x] `sender_suggestions` SQLite table — caches domain → label mappings; user-confirmed entries never expire, inferred entries expire after 24h
- [x] Suggested label shown as "→ LabelName" primary button on digest attachment
- [x] User-confirmed moves update the suggestions cache

### AI / categorization
- [x] Ollama integration — optional; categorizes and summarizes emails when `ollama.enabled: true`
- [x] Keyword/sender fallback categorizer — uses `preferences.yaml` categories config
- [x] VIP sender detection — marks email `VIP: true`, shown with gold attachment color
- [x] Mute filter — drops emails by sender or subject pattern before digest
- [x] `<think>` tag stripper for qwen3-style LLM output

### Infrastructure
- [x] Docker — multi-stage `Dockerfile` (golang:1.26-alpine builder → alpine runtime), binary at `/usr/local/bin/email-agent`
- [x] `docker-compose.yml` — mounts `./data:/data`, loads `./data/.env`, exposes port 8090, `restart: unless-stopped`
- [x] `data/` volume — all runtime secrets and config live here, gitignored
- [x] `data/` added to `.gitignore` (also ignores legacy `credentials.json`, `*_token.json`, `*.db`, `config.yaml` at root)
- [x] `Makefile` — `build`, `run`, `now`, `down`, `logs` targets
- [x] Cloudflare Tunnel live at `https://email-agent.nocandstar.cc` → `localhost:8090`
- [x] `callback_url` in `data/config.yaml` points to Cloudflare tunnel URL

### Storage
- [x] SQLite DB via `modernc.org/sqlite` (pure Go, no CGO) — tables: `seen_emails`, `last_run`, `digest_emails`, `poll_state`, `sender_suggestions`
- [x] `last_run` per-account timestamp — used as `since` for IMAP `UidSearch`; defaults to 24h ago on first run
- [x] `IsSeen` / `MarkSeen` implemented but currently commented out in `digest()` (intentional — re-enables on next loop otherwise no new mail shows)

---

## In Progress

- [ ] **Unstaged working-tree changes not yet committed** — `git status` shows modifications to `.gitignore`, `actions/handler.go`, `docker-compose.yml`, `email/gmail.go`, `main.go`, and new file `actions/setup.go`. These represent the env-var migration and Gmail re-auth flow work. Commit once new credentials are confirmed working.
- [ ] **`preferences.yaml` deleted from repo root** — file was deleted in working tree (was committed in `3016404`). It now lives at `data/preferences.yaml`. The deletion should be staged and committed.

---

## Backlog

### Add account via Mattermost bot conversation
- [ ] `add gmail <name>` command in poll loop → generate OAuth link → post DM → callback saves token to `data/<name>_token.json` → append account to `data/config.yaml` → reload or restart
- [ ] `add imap <name>` command → post a button to DM → button triggers `trigger_id` → open Mattermost dialog with fields: host, port, email, password, archive mailbox → validate connection → append to `data/config.yaml`
- [ ] Config hot-reload or graceful restart after adding an account (currently requires container restart to pick up config changes)

### Google Cloud / OAuth hardening
- [ ] Publish the Google Cloud app (unverified) so OAuth tokens don't expire every 7 days — go to OAuth consent screen, set to "External", submit for verification or accept the unverified warning for personal use
- [ ] Consider switching to a service account + domain-wide delegation if this becomes a multi-user tool

### Git hygiene
- [ ] Scrub `def3754` from branch history using `git filter-repo --path email/gmail.go --invert-paths` or rebase to rewrite the commit — eliminates the committed secret from GitHub history after credential rotation
- [ ] PR: merge `feature/multiple-accounts` into `main`

### Reliability / polish
- [ ] Re-enable `IsSeen` / `MarkSeen` dedup (currently commented out in `digest()`) — or decide on a different dedup strategy (e.g. only show emails newer than `last_run`)
- [ ] Gmail `FetchNew` ignores the `since time.Time` argument — currently fetches all inbox messages on every run. Should add a `after:<date>` Gmail query filter or rely on `last_run` timestamp.
- [ ] IMAP `moveUID` marks messages `\Deleted` but never calls `EXPUNGE` — deleted messages linger until the server or client expunges. Add `c.Expunge(nil)` after the store.
- [ ] Action client map is built once at startup — if a Gmail token is re-authorized, the in-memory `GmailClient` still holds the old (expired) HTTP client. Rebuild or refresh the action client map after a successful re-auth callback.
- [ ] `preferences.yaml` changes require container restart — no live reload
- [ ] Ollama `generate` makes a blocking HTTP call in the digest loop; slow models will delay the whole digest. Consider a timeout or goroutine per email.
