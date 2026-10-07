# Crypto Trading Bot — Architecture & Processing Flow

## 1. System Overview

```
┌─────────────────────────────────────────────────────────────────────────┐
│                         INFRASTRUCTURE                                   │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  ┌────────────┐ │
│  │ TimescaleDB  │  │   Ollama     │  │   Backend    │  │  Frontend  │ │
│  │  (Postgres)  │  │  (Local LLM) │  │   (Go/Gin)  │  │ (Next.js)  │ │
│  │  port: 5432  │  │  port: 11434 │  │  port: 8081  │  │ port: 3002 │ │
│  └──────────────┘  └──────────────┘  └──────────────┘  └────────────┘ │
└─────────────────────────────────────────────────────────────────────────┘
```

**Goal**: Automated crypto trading bot using a local AI (LLM) to analyze market conditions and generate BUY/SELL/HOLD signals.

**Tech Stack**:
- Backend: Go + Gin framework
- Frontend: Next.js + React Query + Tailwind CSS
- Database: PostgreSQL (TimescaleDB hypertable on local docker; plain table on Neon — capability-gated in `database.HasTimescale`)
- AI: Ollama (local LLM, no external API keys required)
- Exchange: Binance (WebSocket + REST API)

---

## 2. Main Data Flow

```
Binance WebSocket ──► Candle Data ──► TimescaleDB ──► Strategy Analyzer
                                                            │
                                                    ┌───────▼───────┐
                                                    │ Compute RSI(14)│
                                                    │ Compute MACD   │
                                                    │  (12, 26, 9)   │
                                                    └───────┬───────┘
                                                            │
                                                    ┌───────▼───────┐
                                                    │  Ollama LLM   │
                                                    │  Analyze &    │
                                                    │  Generate     │
                                                    │  Signal       │
                                                    └───────┬───────┘
                                                            │
                                                    ┌───────▼───────┐
                                                    │ Risk Manager  │
                                                    │ (Validate     │
                                                    │  risk limits) │
                                                    └───────┬───────┘
                                                            │
                                                    ┌───────▼───────┐
                                                    │   Executor    │
                                                    │ (Execute      │
                                                    │  trade order) │
                                                    └───────┬───────┘
                                                            │
                              ┌──────────────────────────────┼──────────────────┐
                              │                              │                  │
                      ┌───────▼───────┐              ┌──────▼──────┐   ┌───────▼───────┐
                      │  Save Trade   │              │ Save Audit  │   │  WebSocket    │
                      │  to Database  │              │    Log      │   │  → Frontend   │
                      └───────────────┘              └─────────────┘   └───────────────┘
```

---

## 3. Step-by-Step Processing Detail

### Step 1: System Startup (`backend/cmd/server/main.go`)

```
1. Load config from environment variables
2. Connect to PostgreSQL/TimescaleDB
3. Run auto-migration (create tables: candles, trades, positions, audit_logs)
4. Initialize Ollama client
5. Initialize WebSocket Hub (to broadcast data to frontend)
6. Initialize Binance Collector
7. Initialize Risk Manager with config:
   - Max position size: 2% of equity
   - Max daily drawdown: 5%
   - Max open positions: 3
8. Initialize Strategy Analyzer
9. Initialize Executor (dry-run mode by default)
```

### Step 2: Backfill Historical Data (`backend/internal/exchange/binance.go`)

```
For each coin in watchlist [BTC, ETH, SOL, BNB, ADA, DOGE, XRP, AVAX]:
  1. Call Binance REST API: GET /api/v3/klines
  2. Fetch latest 1000 candles (interval: 1h)
  3. Parse response → Candle struct {Open, High, Low, Close, Volume, OpenTime}
  4. Upsert into TimescaleDB (ON CONFLICT DO NOTHING)
```

### Step 3: Stream Real-time Data (`backend/internal/exchange/binance.go`)

```
1. Connect WebSocket: wss://stream.binance.com/ws
2. Subscribe to kline streams: btcusdt@kline_1m, btcusdt@kline_1h, ...
3. On message received:
   a. Parse JSON → Kline event
   b. If candle is closed (is_final = true):
      - Persist to TimescaleDB
   c. Broadcast tick data via WebSocket Hub → Frontend
4. On disconnect: auto-reconnect after 3-5 seconds
```

### Step 4: Market Analysis (`backend/internal/strategy/analyzer.go`)

```
Every 5 minutes, Executor triggers Analyzer:
  1. Query latest 100 candles from DB (by symbol + interval)
  2. Compute technical indicators:
     - RSI(14): measures momentum (>70 = overbought, <30 = oversold)
     - MACD(12,26,9): measures trend (signal line crossover)
  3. Build MarketContext:
     {
       symbol, interval, current_price,
       rsi_value, macd_line, signal_line, histogram,
       volume_24h, price_change_24h
     }
  4. Send MarketContext → Ollama for AI analysis
```

### Step 5: AI Decision Making (`backend/internal/ai/ollama.go`)

```
1. Construct prompt with:
   - Current market data (price, RSI, MACD, volume)
   - Analysis rules:
     + RSI > 70 → bearish signal
     + RSI < 30 → bullish signal
     + MACD histogram positive + crossover → bullish
     + MACD histogram negative + crossover → bearish
   - Required output format: JSON

2. Call Ollama API: POST /api/generate
   - Model: from config (default: gemma or llama)
   - Format: JSON mode
   - Timeout: 120 seconds

3. Parse response JSON:
   {
     "action": "BUY" | "SELL" | "HOLD",
     "confidence": 0.0 - 1.0,
     "reasoning": "Explanation for the decision",
     "stop_loss": stop loss price,
     "take_profit": take profit price
   }

4. Save audit trail:
   - Full prompt sent
   - Raw response received
   - Inference duration
```

### Step 6: Risk Validation (`backend/internal/riskmanager/riskmanager.go`)

```
Before executing any trade:
  1. Check Circuit Breaker:
     - If TRIPPED → BLOCK (no trading allowed)
     - Circuit breaker auto-resets on new calendar day

  2. Check Daily Drawdown:
     - Total daily loss > 5% of equity → BLOCK + TRIP circuit breaker

  3. Check Max Open Positions:
     - Open positions count >= 3 → BLOCK

  4. Calculate Position Size:
     - size = (equity * 2%) / current_price
     - Ensures position does not exceed risk limits

Result: ALLOW (trade permitted) or BLOCK (rejected + reason logged)
```

### Step 7: Trade Execution (`backend/internal/executor/executor.go`)

```
Executor Loop (every 5 minutes):
  1. Call Analyzer.Analyze(symbol, interval)
  2. Receive signal from AI

  3. Filter signal:
     - confidence < 0.7 → SKIP
     - action = HOLD → SKIP

  4. If action = BUY:
     a. Verify no existing position for this symbol
     b. Call RiskManager.PreTradeCheck()
     c. Calculate position size
     d. Create Trade record (status: OPEN)
     e. Create Position record
     f. Log audit: "Trade executed"

  5. If action = SELL:
     a. Find open position for this symbol
     b. If none exists → SKIP
     c. Close position
     d. Calculate PnL = (exit_price - entry_price) * quantity
     e. Update Trade record (status: CLOSED, pnl)
     f. Call RiskManager.PostTradeCheck(pnl)
     g. Log audit: "Trade closed"

  6. Save AuditLog for EVERY evaluation (whether executed or not)

  ⚠️  DRY-RUN MODE: By default, no real orders are placed on Binance
```

### Step 8: Broadcast Results to Frontend

```
1. WebSocket Hub sends events to all connected clients:
   - Candle ticks (real-time price updates)
   - AI signals (when new signal is generated)
   - Trade notifications

2. Frontend receives via WebSocket hook:
   - Throttled query invalidation (3 seconds) to prevent UI jank
   - requestAnimationFrame for chart updates
   - Connection health tracking (latency, reconnect count)
```

---

## 4. API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/ws` | WebSocket connection for live data |
| `GET` | `/api/v1/stats` | Aggregate trade statistics (win rate, PnL, etc.) |
| `GET` | `/api/v1/candles` | OHLCV data for charts |
| `GET` | `/api/v1/trades` | Recent trade history |
| `GET` | `/api/v1/trades/:id/audit` | Audit detail for a specific trade |
| `GET` | `/api/v1/positions` | Currently open positions |
| `GET` | `/api/v1/analytics/history` | Multi-symbol data by time range |
| `GET` | `/api/v1/audit-logs` | Paginated AI decision logs |
| `GET` | `/api/v1/risk/status` | Circuit breaker state + risk limits |
| `POST` | `/api/v1/risk/reset` | Manually reset circuit breaker (bearer `APPROVAL_TOKEN` when set) |
| `POST` | `/api/v1/backtest` | Trigger historical backtest (bearer `APPROVAL_TOKEN` when set) |
| `GET` | `/api/v1/backtest` | Poll backtest results |
| `GET` | `/api/v1/signals/pending` · `POST /api/v1/signals/:id/{approve,reject}` | Live approval gate (bearer `APPROVAL_TOKEN`) |

---

## 5. Database Schema

### candles (TimescaleDB Hypertable)
```sql
- symbol      VARCHAR    -- "BTCUSDT"
- interval    VARCHAR    -- "1m", "1h"
- open_time   TIMESTAMP  -- Candle open time (partition key)
- open        FLOAT      -- Open price
- high        FLOAT      -- Highest price
- low         FLOAT      -- Lowest price
- close       FLOAT      -- Close price
- volume      FLOAT      -- Trading volume
```

### trades
```sql
- id          UUID
- symbol      VARCHAR
- side        VARCHAR    -- "BUY" / "SELL"
- entry_price FLOAT
- exit_price  FLOAT
- quantity    FLOAT
- pnl         FLOAT
- confidence  FLOAT     -- AI confidence score
- status      VARCHAR   -- "OPEN" / "CLOSED"
- created_at  TIMESTAMP
- closed_at   TIMESTAMP
```

### positions
```sql
- id          UUID
- symbol      VARCHAR
- side        VARCHAR
- entry_price FLOAT
- quantity    FLOAT
- status      VARCHAR   -- "OPEN" / "CLOSED"
- created_at  TIMESTAMP
```

### audit_logs
```sql
- id             UUID
- trade_id       UUID (nullable)
- symbol         VARCHAR
- action         VARCHAR     -- "BUY" / "SELL" / "HOLD"
- confidence     FLOAT
- reasoning      TEXT        -- AI explanation
- prompt         TEXT        -- Full prompt sent to LLM
- response       TEXT        -- Raw LLM response
- executed       BOOLEAN     -- Whether trade was executed
- blocked_reason VARCHAR     -- Reason for blocking (if any)
- latency_ms     INT         -- LLM inference time
- created_at     TIMESTAMP
```

---

## 6. Frontend Architecture

```
app/
├── page.tsx          ← Main dashboard (overview)
├── ai/page.tsx       ← AI signals & audit logs
├── chart/page.tsx    ← Detailed charts
├── trades/page.tsx   ← Trade history
└── settings/page.tsx ← Settings

components/
├── dashboard/        ← Dashboard widgets
│   ├── coin-picker   ← Select coins to monitor
│   ├── stats-cards   ← Quick statistics
│   ├── ai-signal     ← Current AI signal display
│   ├── risk-gauge    ← Visual risk level gauge
│   ├── kill-switch   ← Emergency stop button
│   └── recent-trades ← Recent trade list
├── charts/           ← Price chart + multi-line chart
└── layout/           ← Header + Sidebar

hooks/
├── use-websocket.ts  ← WS connection management + auto-reconnect
├── use-price-chart.ts ← Chart data processing
└── use-url-state.ts  ← Sync state with URL params
```

**Data Fetching Strategy**:
- React Query polling every 5-10 seconds for: stats, trades, positions, risk
- WebSocket for real-time: candle ticks, AI signals
- URL state sync for: selected coins, time range filters

---

## 7. Backtest Flow

```
1. User sends POST /api/v1/backtest:
   {symbol, interval, start_date, end_date}

2. Backend loads candles from DB for the specified time range

3. Sliding window (100 candles) across entire dataset:
   - Each window → call Analyzer (compute RSI, MACD, query AI)
   - Simulate trade with 2% equity per order
   - Track: entries, exits, PnL

4. Compute metrics:
   - Win Rate (% of profitable trades)
   - Total PnL (cumulative profit/loss)
   - Max Drawdown (largest peak-to-trough decline)
   - Sharpe Ratio (risk-adjusted return)

5. User polls GET /api/v1/backtest to retrieve results
```

---

## 8. Risk Management Rules

| Rule | Value | Action on Violation |
|------|-------|---------------------|
| Max Position Size | 2% of equity | Limit order size |
| Max Daily Drawdown | 5% of equity | Trip circuit breaker |
| Max Open Positions | 3 | Block new orders |
| Min Confidence | 0.7 (70%) | Skip signal |
| Circuit Breaker | Auto-trip on violation | Halt all trading |

---

## 9. Watchlist Coins

```
BTCUSDT, ETHUSDT, SOLUSDT, BNBUSDT, ADAUSDT, DOGEUSDT, XRPUSDT, AVAXUSDT
```

---

## 10. Deployment (Docker Compose)

```yaml
Services:
  postgres:   TimescaleDB (port 5432) — data storage
  ollama:     Local LLM (port 11434) — AI inference
  backend:    Go server (port 8081) — trading engine
  frontend:   Next.js (port 3002) — dashboard UI
```

**Startup order**: postgres → ollama → backend → frontend

---

## 11. Key Design Decisions

1. **Dry-run by default** — No real orders placed until explicitly enabled
2. **Full audit trail** — Every AI evaluation is logged regardless of execution
3. **Circuit breaker pattern** — Automatic halt on excessive daily losses
4. **Local LLM (Ollama)** — No external API costs, full control over inference
5. **TimescaleDB hypertables** — Optimized time-series queries for candle data
6. **Hybrid data loading** — REST backfill + WebSocket live streaming
7. **Confidence threshold** — Only trades when AI is >= 70% confident
