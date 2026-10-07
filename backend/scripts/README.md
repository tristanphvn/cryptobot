# backend/scripts

Operator-triggered SQL utilities. None of these run automatically — by
design, so the operator owns when and against which DSN they execute.

| Script | Purpose | Run with |
|---|---|---|
| `cry22-observe.sql` | Read-only daily observation bundle for CRY-22 / CRY-23: 6 result sets (lifecycle, exit distribution, winner protection, trail effectiveness, AI exit value, alerts). No DML. | `psql "$POSTGRES_DSN" -f backend/scripts/cry22-observe.sql -v ON_ERROR_STOP=1` |

### Alert tiering — `cry22-observe.sql` section 6

Section 6 emits 5 checks. Treat by tier:

- **Tier 1 (HALT observation, notify immediately)** — `6a` profitable→loss after trail armed; `6b` peak stuck >1h on open position; `6e` AI signal silently dropped. These are production-grade defects in the trailing-stop or executor pipeline.
- **Tier 2 (note in daily report, do not halt)** — `6c` not armed despite max_unrealized ≥ arm threshold; `6d` AI audit row missing `position_*` despite open position. Instrumentation defects — recoverable, but worth fixing in a follow-up ticket.

Override the observation window via `-v since="'YYYY-MM-DD HH:MM:SS'"`. Default is the trailing 24 hours.

## Data retention

Pruning moved out of SQL into the backend (CRY-38): `internal/retention`
runs every `RETENTION_INTERVAL` (default 24h) inside the server, and
`go run ./cmd/prune` runs the same rules once (`-clear-backtest-cache`
also drops the backtest AI cache, which is never cleared automatically).

| Data | Kept | Env |
|---|---|---|
| 1m candles | 30 days (+ up to one 7-day chunk) | `CANDLE_1M_RETENTION_DAYS` |
| 5m / 15m candles | 90 days | `CANDLE_INTRADAY_RETENTION_DAYS` |
| 1h / 4h / 1d candles | forever | — |
| `audit_logs` rows | forever; `prompt` / `raw_response` cleared after 30 days | `AUDIT_TEXT_RETENTION_DAYS` |
| `pending_signals` expired / rejected / failed | 14 days | — |
| `trades` FAILED | 30 days | — |

On TimescaleDB, candle chunks older than 7 days are compressed; the job
prunes compressed chunks by decompress → delete → compress so they stay
compact. `0` disables a rule.
