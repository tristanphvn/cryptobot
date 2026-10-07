# CryptoBot

An autonomous crypto trading bot that asks a **local LLM** (Ollama) for BUY / SELL / HOLD decisions on Binance market data, with a **Go** backend for ingestion, risk and execution and a **Next.js** dashboard for monitoring.

Inference, keys and data stay on your machine. The bot starts in **dry-run** mode and never places a real order unless you explicitly switch to `live`.

> **Not financial advice.** This is an experimental project. Backtests and paper trading so far show a negative edge; do not trade money you cannot afford to lose.

## Quick start

Requirements: [Docker](https://docs.docker.com/get-docker/) with Compose v2 (running), ~8 GB free disk. Go 1.26+ and Node 22+ are optional (only for running services outside Docker).

```bash
git clone <this repo> cryptobot && cd cryptobot

# macOS / Linux / Git Bash
./scripts/setup.sh

# Windows PowerShell
.\scripts\setup.ps1
```

The script:

1. checks Docker (and Go / Node if installed),
2. creates `.env` from `.env.example` with a random database password and operator token (an existing `.env` is never overwritten),
3. runs `go mod download` + `go build` and `npm ci` when Go / Node are present,
4. checks the host ports are free, then `docker compose up -d --build`,
5. pulls the Ollama model (`llama3`, ~4.7 GB) and waits until the API and dashboard answer.

Then open **http://localhost:3002**. Re-running the script is safe.

| Option | Effect |
|---|---|
| `--skip-model` / `-SkipModel` | don't pull the LLM now (`docker compose exec ollama ollama pull llama3` later) |
| `--no-start` / `-NoStart` | only prepare `.env` and dependencies |

If a port is taken (a native Ollama often owns 11434), the script stops and names the variable to change, e.g. `OLLAMA_PORT=11435` in `.env`.

Stop with `docker compose down`; follow logs with `docker compose logs -f backend`.

## Architecture

```
Binance WS ──► eventbus ──► persister ──► TimescaleDB
                  │                          │
                  └──► WS hub ──► dashboard  ▼
                                   strategy loop (every 5 min)
                                   RSI / MACD ─► Ollama (JSON signal)
                                   ─► risk gates ─► executor ─► position monitor (SL / TP / trailing)
```

| Service | Port (127.0.0.1) | |
|---|---|---|
| Dashboard (Next.js 15) | 3002 | `FRONTEND_PORT` |
| API + WebSocket (Go / Gin) | 8081 | `BACKEND_PORT`, `/health` |
| TimescaleDB | 5432 | `POSTGRES_PORT` |
| Ollama | 11434 | `OLLAMA_PORT` |

Deeper walkthrough: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

```
backend/
  cmd/server        main service          cmd/backtest   historical replay + go/no-go report
  cmd/prune         run data retention    cmd/migrate-schema, cmd/backfill
  internal/         ai, api, database, exchange, executor, retention, riskmanager, strategy, ...
frontend/           Next.js App Router dashboard
scripts/            setup.sh, setup.ps1
```

## Execution modes

| `EXECUTION_MODE` | What happens |
|---|---|
| `dry-run` (default) | signals are evaluated and logged, nothing is opened |
| `paper` | simulated fills on live prices, 0.1% fee per leg (`PAPER_FEE_RATE`) |
| `live` | **real Binance orders**, every entry needs operator approval in the dashboard |

`live` refuses to start without `BINANCE_API_KEY`, `BINANCE_API_SECRET` and `APPROVAL_TOKEN`. Also set `NEXT_PUBLIC_EXECUTION_MODE` so the dashboard shows the mode, and rebuild the frontend image (`docker compose up -d --build frontend`).

Built-in safeguards: confidence gate (0.6), wrong-side SL/TP rejection, max stop-loss distance (`MAX_STOP_LOSS_PCT`, 5%), min expected return (`MIN_EXPECTED_RETURN_BPS`), position sizing (`RISK_MAX_POSITION_PCT`), max open positions, daily net-PnL drawdown circuit breaker, stop-loss / take-profit / trailing-stop monitor, slippage cap on live fills.

## Configuration

All settings are environment variables; [.env.example](.env.example) lists every one with its default and a comment. The ones you are most likely to touch:

| Variable | Default | |
|---|---|---|
| `EXECUTION_MODE` | `dry-run` | see above |
| `OLLAMA_MODEL` | `llama3` | any Ollama model that returns JSON reliably |
| `EXECUTOR_SYMBOLS` | 8 USDT majors | comma-separated |
| `APPROVAL_TOKEN` | random (setup) | operator bearer token, paste it in the dashboard Settings |
| `BIND_ADDR` | `127.0.0.1` | interface the docker ports bind to |
| `ALLOWED_ORIGINS` | `http://localhost:3002,…` | browser origins for CORS + WebSocket |
| `CANDLE_1M_RETENTION_DAYS` | `30` | 5m / 15m keep 90, 1h+ forever, audit text 30 |

## Security

- Every port binds to `127.0.0.1`. Exposing the stack (`BIND_ADDR=0.0.0.0`) is your call — put it behind your own TLS reverse proxy and set `TRUSTED_PROXIES` / `ALLOWED_ORIGINS`.
- State-changing operator actions (signal approval, circuit-breaker reset, backtests) require the `APPROVAL_TOKEN` bearer when it is set; the setup script always sets one.
- `.env` holds your secrets and is git-ignored, as is every other `.env.*` file except the template.
- Binance key for live mode: **Spot trading only, withdrawals disabled, IP-restricted**. A leaked key without an IP whitelist can drain the account.
- Orders are HMAC-signed locally (`internal/exchange`); transport errors never echo the signed query.

Found a vulnerability? Please open a private security advisory on the repository rather than a public issue.

## Development

```bash
# backend (needs Postgres + Ollama: docker compose up -d postgres ollama, POSTGRES_HOST=localhost)
cd backend
go build ./... && go vet ./... && go test ./...
go run ./cmd/server

# frontend
cd frontend
npm ci
npm run lint && npm run build
npm run dev          # http://localhost:3002

# backtest a symbol over the last 12 months (needs candles + Ollama)
cd backend && go run ./cmd/backtest -symbols BTCUSDT -interval 1h -slippage-bps 5
```

Data retention runs inside the server every 24h; `go run ./cmd/prune` applies it once.

## License

[MIT](LICENSE)
