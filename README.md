# Email Agent

A self-hosted email digest bot for Mattermost. It pulls unread emails from Gmail and IMAP accounts (iCloud, Outlook, etc.) on a schedule, posts them as interactive cards to a DM channel, and lets you archive, delete, mark read, move, and filter emails directly from chat.

## Features

- **Multi-account** — connect any combination of Gmail and IMAP accounts
- **Daily digest** — posts unread emails on a cron schedule (default: 7am)
- **Interactive cards** — Archive, Mark Read, Move, Delete, and Create Filter buttons on each email
- **Filters** — create rules to auto-archive, delete, mute, or label emails from a sender
- **AI summaries** — optional Ollama integration for one-line email previews and NLP commands
- **Natural language** — type "archive emails 1 3 5" or "add my iCloud account" and it figures it out
- **Account management** — add, rename, and remove accounts without restarting

## Requirements

- Docker and Docker Compose
- A Mattermost instance with a bot account
- Gmail: a Google Cloud project with the Gmail API enabled and OAuth credentials
- IMAP: an app-specific password (iCloud, Outlook, or any standard IMAP server)

## Setup

### 1. Clone and create the data directory

```bash
git clone https://github.com/kamden-rasmussen/unneeded-email-client.git
cd unneeded-email-client
mkdir data
```

### 2. Create `data/config.yaml`

Copy the example and fill in your values:

```bash
cp config.yaml.example data/config.yaml
```

Key fields:

| Field | Description |
|---|---|
| `mattermost.bot_token` | Bot token from Mattermost System Console |
| `mattermost.server_url` | Your Mattermost URL (no trailing slash) |
| `mattermost.dm_user` | Mattermost username to send digests to |
| `mattermost.callback_url` | Public HTTPS URL where Mattermost can reach this service |
| `mattermost.webhook_secret` | A random string to authenticate button callbacks |
| `schedule` | Cron expression for the digest (default: `0 7 * * *`) |

### 3. Gmail setup

1. Create a Google Cloud project and enable the Gmail API
2. Create OAuth 2.0 credentials (Desktop app type)
3. Download `credentials.json` and place it in `data/credentials.json`
4. Add your account to `data/config.yaml` and the bot will send an authorization link on first run

### 4. IMAP / iCloud setup

Add an account to `data/config.yaml`:

```yaml
accounts:
  - name: icloud
    type: imap
    email: you@icloud.com
    password: xxxx-xxxx-xxxx-xxxx  # app-specific password
    host: imap.mail.me.com
    port: 993
```

Or just tell the bot `add icloud myaccount` and it will walk you through it via a browser form.

### 5. Optional: Ollama (AI summaries and NLP)

```yaml
ollama:
  enabled: true
  model: llama3.2
```

Set `OLLAMA_HOST` in `data/.env` if Ollama is running on another machine.

### 6. Run

```bash
docker compose up -d
```

## Commands

Send these as direct messages to the bot:

| Command | Description |
|---|---|
| `check` | Run a digest now |
| `archive 1 3` | Archive emails by number |
| `done all` | Archive and mark all as read |
| `delete 2` | Delete an email |
| `add gmail <name>` | Add a Gmail account (sends OAuth link) |
| `add icloud <name>` | Add an iCloud account (opens credential form) |
| `add imap <name>` | Add a generic IMAP account |
| `remove <name>` | Remove an account |
| `rename <old> <new>` | Rename an account |
| `help` | List all commands |

Natural language works too — "archive the newsletter emails" or "remove my work account".

## Data

Everything is stored in `./data/`:

```
data/
  config.yaml          # your configuration (never commit this)
  credentials.json     # Gmail OAuth credentials
  *_token.json         # Gmail access tokens
  email_agent.db       # SQLite database (digest history, filters)
  .env                 # optional env vars (OLLAMA_HOST, OLLAMA_TOKEN)
```

The entire `data/` directory is gitignored.
