# autoclawpi

> Unofficial AutoClaw proxy — OpenAI-compatible API + web management panel

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/license-masantoid-blue)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-Linux-333?logo=linux)]()

**autoclawpi** is a self-hosted proxy for AutoClaw that provides an OpenAI-compatible API endpoint with multi-account round-robin, auto token refresh, daily check-in, 100M token claim, and a full-featured web management panel.

---

## Features

### 🤖 OpenAI-Compatible API
- Drop-in replacement for OpenAI client — use any OpenAI library, CLI, or coding agent
- Verified models: `glm-5.3`, `glm-5.3-flash`, `deepseek-v4.1-flash`, `deepseek-v4-pro`, `auto`, `auto-fast`
- Full tool calling / function calling support (streaming SSE and non-streaming)
- Compatible with coding agents (pi, Cline, Cursor, Roo Code, Continue, Aider)
- Upstream WAF bypass (OpenClaw banner injection & harness preamble sanitization)
- Automatic reasoning budget headroom & empty-content recovery
- Round-robin load balancing across accounts
- Auto token refresh on 401
- Token usage tracking & cost estimation
- API key authentication
- WAF prefix stripping — handles multiple `{"message":"forbidden"}` prefixes
- WAF auto-retry — switch to next account on hard block + 500ms delay
- Token bucket rate limiter — prevents WAF rate-limit (configurable)

### 🌐 Web Management Panel
- **Dashboard** — account overview, points balance, API usage stats, cost tracking
- **Accounts** — OAuth login, import tokens, view details, JWT decode, health check
- **Claim 100M** — claim newbie token reward for fresh accounts
- **Check-In** — daily reward claiming for all accounts
- **Logs** — real-time API request log with terminal-style view
- **Settings** — password, multiple API keys, round-robin strategy
- **Health** — test & refresh all account tokens
- **API Docs** — endpoint reference with curl examples
- **Responsive** — collapsible sidebar, adaptive layout

### 📚 Documentation
- [CPA gateway integration](docs/CPA-INTEGRATION.md) — using autoclawpi as a channel behind a CPA-style gateway
- [Egress IP pool design](docs/EGRESS-POOL-DESIGN.md) — why each account gets its own outbound IP, and how the pool is built

### 🔐 Security
- Web panel password protection
- API key authentication for OpenAI endpoint
- JWT token auto-decode (user info extracted from token)
- Session-based panel login with 30-day expiry

---

## Quick Start

### Prerequisites
- Go 1.26+ (for building from source)
- Windows 10/11 or Linux

### Install & Build

**Windows (PowerShell):**
```powershell
git clone https://github.com/IngKevinDavid/autoclawpi.git
cd autoclawpi

# Automated build script:
.\install.ps1

# Or manual compilation:
go build -o autoclawpi.exe ./cmd/autoclawpi
```

**Linux:**
```bash
git clone https://github.com/IngKevinDavid/autoclawpi.git
cd autoclawpi
go build -o ~/.local/bin/autoclawpi ./cmd/autoclawpi
```

### Run

**Windows (PowerShell):**
```powershell
# Quick start with default settings (port 8787, password: admin):
.\start-server.ps1

# Or with custom options:
.\start-server.ps1 -Port 8787 -WebPassword "yourpassword" -ApiKey "sk-your-key"
```

**Linux / CLI:**
```bash
# Start server with password
autoclawpi serve --port 8787 --web-password yourpassword

# With API key protection
autoclawpi serve --port 8787 --web-password yourpassword --api-key sk-your-key
```

Open **http://localhost:8787** in your browser.

---

## Usage

### CLI Commands

```bash
# Login (OAuth via browser with captcha)
autoclawpi login

# Account management
autoclawpi account list
autoclawpi account show 1
autoclawpi account add --access "Bearer eyJ..." --refresh "Bearer eyJ..."
autoclawpi account remove 1

# Check-in all accounts
autoclawpi checkin

# Start server
autoclawpi serve --port 8787 --web-password yourpassword --api-key sk-your-key
```

### API

```bash
# List models
curl http://localhost:8787/v1/models

# Chat completion
curl -X POST http://localhost:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "auto", "messages": [{"role": "user", "content": "Hello!"}]}'

# With API key
curl -X POST http://localhost:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-your-key" \
  -d '{"model": "deepseek-v4-pro", "messages": [{"role": "user", "content": "Hello!"}]}'

# Python SDK
python3 -c "
from openai import OpenAI
client = OpenAI(base_url='http://localhost:8787/v1', api_key='sk-your-key')
r = client.chat.completions.create(model='auto', messages=[{'role':'user','content':'halo'}])
print(r.choices[0].message.content)
"
```

### Web Panel Routes

| Route | Description |
|-------|-------------|
| `/` | Dashboard with stats |
| `/accounts` | Account management |
| `/accounts/{id}` | Account detail + claim 100M |
| `/accounts/login` | OAuth login |
| `/accounts/import` | Import token manually |
| `/checkin` | Daily check-in |
| `/logs` | API request logs |
| `/settings` | Configuration, API keys |
| `/health` | Account health check |
| `/docs` | API documentation |
| `/login` | Panel login |

---

## Architecture

```
autoclawpi
├── cmd/autoclawpi/       # CLI entry point
│   ├── main.go           # Command routing, server startup
│   ├── login.go          # OAuth login (captcha widget)
│   ├── account.go        # Account management commands
│   └── checkin.go        # Check-in command
├── internal/
│   ├── client/           # AutoClaw HTTP API client (inference, claim, check-in, OAuth)
│   ├── config/           # Configuration loading (rate limit, ports, etc.)
│   ├── db/               # SQLite database layer (accounts, logs, config)
│   ├── server/           # OpenAI-compatible proxy + token bucket rate limiter
│   ├── sign/             # X-Auth signature generation (md5-based)
│   ├── store/            # Credential storage (AES-GCM encrypted)
│   └── web/              # Web management panel
│       └── templates/    # HTML templates (glassmorphism dark theme)
├── DISCLAIMER.md         # Legal disclaimer (EN + 中文)
├── LICENSE               # masantoid license
├── docs/
│   ├── CPA-INTEGRATION.md # Using autoclawpi behind a CPA-style gateway
│   └── EGRESS-POOL-DESIGN.md # Per-account egress isolation design notes
└── go.mod
```

### Data Flow

```
Client (OpenAI SDK) → autoclawpi (port 8787)
  ├── Rate Limiter (token bucket: 1 req/1.5s, burst 3)
  ├── /v1/chat/completions → Round-robin account selection
  │   ├── Account #1 → AutoClaw API → Response → Token usage logged
  │   ├── 401 → Auto refresh token → Retry
  │   ├── 403 (WAF block) → Next account + 500ms delay
  │   └── ... (failover across all accounts)
  └── Web Panel → SQLite → Account management, logs, config
```

---

## Configuration

### Server Options

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `8787` | Listen port |
| `--host` | `127.0.0.1` | Listen host |
| `--web-password` | `""` | Panel password (required) |
| `--api-key` | `""` | API key for OpenAI endpoint |

### Config File (`~/.autoclawpi/config.json`)

```json
{
  "rate_limit_per_sec": 0.667,
  "rate_limit_burst": 3
}
```

| Key | Default | Description |
|-----|---------|-------------|
| `rate_limit_per_sec` | `0.667` (1/1.5s) | Token refill rate per second |
| `rate_limit_burst` | `3` | Burst capacity for parallel requests |
| `host` | `127.0.0.1` | Listen host |
| `port` | `8787` | Listen port |
| `api_key` | `""` | API key protection |

### Rate Limiter Guide

| Config | Rate | Burst | Use Case |
|--------|------|-------|----------|
| Default | 1 req/1.5s | 3 | Coding (Claude Code/Cursor) |
| Safe | 1 req/2s | 2 | Conservative |
| Relaxed | 1 req/s | 5 | Heavy parallel tool calls |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `AUTOCLAWPI_DIR` | Data directory (default: `~/.autoclawpi/`) |

---

## Storage

All data is stored in SQLite at `~/.autoclawpi/autoclawpi.db`:

| Table | Description |
|-------|-------------|
| `accounts` | OAuth tokens, user info, points balance |
| `checkin_log` | Daily check-in history |
| `logs` | API request logs (tokens, cost, timestamps) |
| `config` | Key-value settings |

---

## Models

All models verified live via upstream inference (September 2026):

| Model | Route ID | Description | Input | Status |
|-------|----------|-------------|-------|--------|
| `glm-5.3` | `zaicoding_glm-5.3` | Flagship coding & multi-step complex reasoning | Text | ✅ Active |
| `glm-5.3-flash` | `zai_glm-5.3-flash` | Fast multimodal reasoning | Text, Image | ✅ Active |
| `deepseek-v4.1-flash` *(or `deepseek-v4-flash`)* | `tdpsk_deepseek-v4-flash-202605` | Fast & cost-efficient for tools and agent workflows | Text | ✅ Active |
| `deepseek-v4-pro` *(or `deepseek-v4`)* | `tdpsk_deepseek-v4-pro-202606` | DeepSeek flagship reasoning for complex tasks | Text | ✅ Active |
| `auto` | `zai_auto` | Intelligent task-based automatic routing | Text | ✅ Active |
| `auto-fast` | `zai_auto-fast` | Speed-prioritized automatic multimodal routing | Text, Image | ✅ Active |

> **Note on Deprecated Models:** Legacy models `glm-5-turbo` and `glm-5.2` have been discontinued by the upstream service and return HTTP 400 (`非法模型`). They have been removed from `/v1/models`.

## Screenshots

```
┌─────────────────────────────────────────────┐
│  Dashboard                                   │
│  ┌──────┐ ┌──────┐ ┌──────┐                 │
│  │Accts │ │Active│ │Points│                 │
│  │  4   │ │  4   │ │ 105K │                 │
│  └──────┘ └──────┘ └──────┘                 │
│  ┌──────┐ ┌──────┐ ┌──────┐                 │
│  │ Req  │ │Tokens│ │ Cost │                 │
│  │  25  │ │ 1.5K │ │ 0.00 │                 │
│  └──────┘ └──────┘ └──────┘                 │
│  ─── Accounts ─────────────────────────      │
│  ✓ Account #5           27,181 pts           │
│  ✓ Account #6           36,200 pts           │
│  ✓ Account #7           36,200 pts           │
│  ✓ Account #9           11,940 pts           │
└─────────────────────────────────────────────┘
```

---

## Security

- **Panel access**: Password-protected (`--web-password`)
- **API access**: Optional API key (`--api-key`)
- **Token storage**: Stored in SQLite (local only)
- **Session**: Cookie-based with 30-day expiry
- **JWT decode**: Auto-extract user info from tokens

---

## Why autoclawpi?

- **Self-hosted** — full control over your data
- **OpenAI-compatible** — use any existing OpenAI tooling
- **Multi-account** — round-robin with auto failover + WAF bypass
- **Rate-limited** — token bucket prevents WAF blocks
- **Token management** — auto-refresh, never lose access
- **Claim 100M** — one-click newbie token reward
- **Monitoring** — real-time logs, cost tracking, health checks
- **Responsive** — collapsible sidebar, adaptive layout

---

## License

masantoid — see [LICENSE](LICENSE) file for details.

---

## Disclaimer

This is an **unofficial, independent** project. It is not created by, endorsed by, or
affiliated with the upstream service provider in any way.

**What it does:** runs a local proxy on your own machine that automates an OAuth login
flow for an account **you own**, stores the resulting token locally, and forwards
**your own** API requests.

**What it does NOT do:** it does not bypass or circumvent any authentication mechanism,
does not grant access to any account other than your own, and does not crack or exploit
the service. Using it with credentials you do not own is strictly prohibited.

**Your responsibility:** use it only with accounts you own; comply with the upstream
service's Terms of Service; your account may be rate-limited or suspended as a result of
automated access, and you accept that risk knowingly.

The software is provided **"AS IS"**, without warranty of any kind. The authors accept
**no liability** for any loss of account access, data, tokens, credits, or any other
damages arising from its use.

See [DISCLAIMER.md](DISCLAIMER.md) for the full terms (English + 中文).

*Built by masanto*