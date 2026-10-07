-- ============================================================================
-- CryptoBot · cry22-observe.sql
-- ============================================================================
-- Daily observation bundle for CRY-22 (trailing stop + audit forensics +
-- position-aware prompt) and the deferred CRY-23 decision (asymmetric SELL
-- confidence gate). Read-only — no DML.
--
-- Produces 6 result sets in order. Operator runs:
--
--   psql "$POSTGRES_DSN" -f backend/scripts/cry22-observe.sql -v ON_ERROR_STOP=1
--
-- Override the observation window (default last 24h). Pass the timestamp
-- WITHOUT inner single quotes — psql's `:'since'` reference wraps it for
-- you, and double-wrapping yields invalid SQL:
--
--   psql "$POSTGRES_DSN" -f backend/scripts/cry22-observe.sql \
--        -v since='2026-06-09 00:00:00'
--
-- Each section is a single SELECT plus a \echo separator so the output
-- reads top-to-bottom in a terminal. Sections can also be copy-pasted
-- into a SQL client one at a time.
--
-- Cumulative tallies are anchored to the CRY-22 merge commit landed on
-- main at 2026-06-09 (commit c872418 — first PR-1 trailing-stop tick).
-- Anything earlier is pre-CRY-22 and excluded from "trail effectiveness".
--
-- ── Alert tiering for section 6 ─────────────────────────────────────────────
-- Tier 1 — production-grade defects (HALT observation, notify immediately):
--   6a  profitable→loss after trail was armed
--   6b  peak tracking stuck >1h on an open position
--   6e  AI signal silently dropped (no trade, no blocked_by)
--
-- Tier 2 — instrumentation defects (note in daily report, do not halt):
--   6c  position never armed despite max_unrealized_pnl_pct ≥ arm threshold
--   6d  AI audit row missing position_* columns despite open position
--
-- ── Caveat: monitor-rows do not populate position_* columns ────────────────
-- writeMonitorAuditLog (synthetic audit_log for SL/TP/TRAILING_STOP closes)
-- leaves the 5 position_* columns NULL by design — only the AI eval site
-- (saveAuditLog) populates them. So for monitor-driven closes, retention
-- math joins trades → positions via timing (most recent closed position on
-- the same symbol whose updated_at brackets the trade.exit_at ±5 min).
-- Proxy join, not referential. If it shows row-mismatch noise in
-- production, the fix is to extend writeMonitorAuditLog to populate the 5
-- columns (separate ticket, out of scope here).
-- ============================================================================

\set QUIET on
\pset format aligned

-- Observation window. Default: last 24h. Override with
-- `-v since='YYYY-MM-DD HH:MM:SS'` on the psql command line (no inner
-- single quotes — the `:'since'` reference below wraps it for you).
--
-- The default branch uses \gset (not \set) because \set stores its arg
-- as literal text — `\set since (now() - interval '24 hours')` would
-- save the string "(now() - interval '24 hours')" instead of evaluating.
-- \gset runs the SELECT and stores the result.
\if :{?since}
\else
  SELECT (now() - interval '24 hours')::text AS since \gset
\endif

-- Trailing-stop tunables for the AI exit value heuristic in section 5.
-- Edit these if you've overridden TRAILING_STOP_PCT / TRAILING_STOP_ARM_PCT
-- in the backend's .env so the classification matches runtime behaviour.
\set trail_pct 2.0
\set arm_pct 1.0
\set neutral_band_pct 0.5

\echo
\echo '═══ CRY-22 observation report ═══'
\echo

SELECT
  'window since'                       AS k, (:'since')::timestamp AS v
UNION ALL SELECT
  'window until',                        now()::timestamp
UNION ALL SELECT
  'cumulative anchor (CRY-22 merge)',    '2026-06-09'::timestamp;

\echo
\echo '── 1. Position lifecycle (CLOSED trades in window) ──'

WITH window_trades AS (
  SELECT *
  FROM trades
  WHERE status = 'CLOSED'
    AND exit_at >= (:'since')::timestamp
),
resolved AS (
  SELECT
    t.id        AS trade_id,
    t.symbol,
    t.entry_price,
    t.exit_price,
    t.net_pnl_percent,
    t.exit_reason,
    EXTRACT(EPOCH FROM (t.exit_at - t.entry_at)) / 3600.0 AS hold_hours,
    (
      SELECT p.id FROM positions p
      WHERE p.symbol = t.symbol
        AND p.is_open = false
        AND p.updated_at BETWEEN t.exit_at - interval '5 minutes'
                              AND t.exit_at + interval '5 minutes'
      ORDER BY p.id DESC LIMIT 1
    ) AS position_id
  FROM window_trades t
)
SELECT
  r.symbol,
  r.entry_price::numeric(20, 8)            AS entry,
  p.peak_price::numeric(20, 8)             AS peak,
  p.max_unrealized_pnl_pct::numeric(10, 4) AS max_unrealized_pct,
  r.exit_price::numeric(20, 8)             AS exit,
  r.net_pnl_percent::numeric(10, 4)        AS realized_pct,
  r.exit_reason,
  ROUND(r.hold_hours::numeric, 2)          AS hold_h
FROM resolved r
LEFT JOIN positions p ON p.id = r.position_id
ORDER BY r.trade_id DESC;

\echo
\echo '── 2. Exit distribution (in-window vs cumulative since CRY-22 merge) ──'

WITH window_counts AS (
  SELECT exit_reason, COUNT(*) AS n
  FROM trades
  WHERE status = 'CLOSED'
    AND exit_at >= (:'since')::timestamp
  GROUP BY exit_reason
),
cumulative_counts AS (
  SELECT exit_reason, COUNT(*) AS n
  FROM trades
  WHERE status = 'CLOSED'
    AND exit_at >= '2026-06-09'::timestamp
  GROUP BY exit_reason
),
buckets(exit_reason) AS (
  VALUES ('STOP_LOSS'),('TRAILING_STOP'),('TAKE_PROFIT'),('AI_SELL'),('MANUAL_CLOSE')
)
SELECT
  b.exit_reason,
  COALESCE(w.n, 0) AS in_window,
  COALESCE(c.n, 0) AS cumulative_since_cry22
FROM buckets b
LEFT JOIN window_counts     w ON w.exit_reason = b.exit_reason
LEFT JOIN cumulative_counts c ON c.exit_reason = b.exit_reason
ORDER BY b.exit_reason;

\echo
\echo '── 3. Winner protection summary (profitable CLOSED trades in window) ──'

WITH profit_trades AS (
  SELECT
    t.id, t.symbol, t.exit_at, t.net_pnl_percent,
    (
      SELECT p.max_unrealized_pnl_pct FROM positions p
      WHERE p.symbol = t.symbol
        AND p.is_open = false
        AND p.updated_at BETWEEN t.exit_at - interval '5 minutes'
                              AND t.exit_at + interval '5 minutes'
      ORDER BY p.id DESC LIMIT 1
    ) AS max_unrealized
  FROM trades t
  WHERE t.status = 'CLOSED'
    AND t.exit_at >= (:'since')::timestamp
    AND t.net_pnl_percent > 0
),
retention AS (
  SELECT
    id, symbol, net_pnl_percent, max_unrealized,
    CASE WHEN max_unrealized IS NULL OR max_unrealized = 0 THEN NULL
         ELSE (net_pnl_percent / max_unrealized * 100)
    END AS retention_pct
  FROM profit_trades
)
SELECT
  COUNT(*) FILTER (WHERE retention_pct IS NOT NULL)                                    AS profitable_trades,
  ROUND(percentile_cont(0.5) WITHIN GROUP (ORDER BY retention_pct)::numeric, 2)        AS median_retention_pct,
  ROUND(percentile_cont(0.9) WITHIN GROUP (ORDER BY retention_pct)::numeric, 2)        AS p90_retention_pct,
  ROUND(MIN(retention_pct)::numeric, 2)                                                AS worst_retention_pct,
  (SELECT symbol FROM retention WHERE retention_pct = (SELECT MIN(retention_pct) FROM retention) LIMIT 1) AS worst_symbol
FROM retention;

\echo
\echo '── 3b. Per-trade retention detail (profitable only, in window) ──'

WITH profit_trades AS (
  SELECT
    t.id, t.symbol, t.exit_at, t.net_pnl_percent,
    (
      SELECT p.max_unrealized_pnl_pct FROM positions p
      WHERE p.symbol = t.symbol
        AND p.is_open = false
        AND p.updated_at BETWEEN t.exit_at - interval '5 minutes'
                              AND t.exit_at + interval '5 minutes'
      ORDER BY p.id DESC LIMIT 1
    ) AS max_unrealized
  FROM trades t
  WHERE t.status = 'CLOSED'
    AND t.exit_at >= (:'since')::timestamp
    AND t.net_pnl_percent > 0
)
SELECT
  symbol,
  net_pnl_percent::numeric(10, 4)                AS realized_pct,
  max_unrealized::numeric(10, 4)                 AS max_unrealized_pct,
  CASE WHEN max_unrealized IS NULL OR max_unrealized = 0 THEN NULL
       ELSE ROUND((net_pnl_percent / max_unrealized * 100)::numeric, 2)
  END AS retention_pct
FROM profit_trades
ORDER BY retention_pct ASC NULLS LAST;

\echo
\echo '── 4a. Trailing-stop effectiveness — snapshot of currently OPEN positions ──'

SELECT
  COUNT(*) FILTER (WHERE is_open = true AND trailing_stop_armed = true) AS open_armed_now,
  COUNT(*) FILTER (WHERE is_open = true)                                AS open_total_now
FROM positions;

\echo
\echo '── 4b. Trailing-stop effectiveness — trail-fired closes in window ──'

SELECT
  COUNT(*)                                                                                       AS trail_closes_in_window,
  ROUND(AVG(t.net_pnl_percent)::numeric, 4)                                                      AS avg_locked_net_pct,
  ROUND(percentile_cont(0.5) WITHIN GROUP (ORDER BY t.net_pnl_percent)::numeric, 4)              AS median_locked_net_pct,
  ROUND(AVG(
    CASE WHEN p.peak_price IS NULL OR p.peak_price = 0 THEN NULL
         ELSE (p.peak_price - t.exit_price) / p.peak_price * 100
    END
  )::numeric, 4) AS avg_drawdown_from_peak_at_exit_pct
FROM trades t
LEFT JOIN positions p
  ON p.symbol = t.symbol
 AND p.is_open = false
 AND p.updated_at BETWEEN t.exit_at - interval '5 minutes' AND t.exit_at + interval '5 minutes'
WHERE t.status = 'CLOSED'
  AND t.exit_reason = 'TRAILING_STOP'
  AND t.exit_at >= (:'since')::timestamp;

\echo
\echo '── 5. AI exit value (every AI_SELL in window, classified) ──'

-- Heuristic classification:
--   beneficial — AI exit price > peak * (1 - trail_pct/100); AI sold at-or-
--                better than where trailing would have fired.
--   harmful    — AI exit price below the trail trigger AND trailing was
--                already armed; AI front-ran a working safety net.
--   neutral    — anything else (incl. positions not yet armed at AI exit).
--   unknown    — position_peak_price NULL (pre-CRY-22 row or backfill gap).

WITH ai_exits AS (
  SELECT
    t.id            AS trade_id,
    t.symbol,
    t.exit_at,
    t.exit_price,
    t.net_pnl_percent,
    t.ai_confidence,
    t.ai_reasoning,
    al.position_unrealized_pnl_pct,
    al.position_peak_price,
    al.position_drawdown_from_peak_pct,
    p.trailing_stop_armed
  FROM trades t
  LEFT JOIN audit_logs al ON al.trade_id = t.id
  LEFT JOIN positions  p
    ON p.symbol = t.symbol
   AND p.is_open = false
   AND p.updated_at BETWEEN t.exit_at - interval '5 minutes' AND t.exit_at + interval '5 minutes'
  WHERE t.status = 'CLOSED'
    AND t.exit_reason = 'AI_SELL'
    AND t.exit_at >= (:'since')::timestamp
),
classified AS (
  SELECT
    *,
    CASE
      WHEN position_peak_price IS NULL THEN 'unknown'
      WHEN exit_price > position_peak_price * (1 - :trail_pct / 100.0)
        THEN 'beneficial'
      WHEN trailing_stop_armed = true
       AND exit_price < position_peak_price * (1 - :trail_pct / 100.0)
                       - position_peak_price * (:neutral_band_pct / 100.0)
        THEN 'harmful'
      ELSE 'neutral'
    END AS classification
  FROM ai_exits
)
SELECT
  symbol,
  ROUND(net_pnl_percent::numeric, 4)                  AS realized_pct,
  ROUND(position_unrealized_pnl_pct::numeric, 4)      AS current_pct_at_exit,
  ROUND(position_drawdown_from_peak_pct::numeric, 4)  AS drawdown_from_peak_pct,
  ROUND(ai_confidence::numeric, 4)                    AS conf,
  classification,
  LEFT(ai_reasoning, 80)                              AS reasoning_head
FROM classified
ORDER BY trade_id DESC;

\echo
\echo '── 5b. AI exit value summary (window) ──'

WITH ai_exits AS (
  SELECT
    t.id, t.symbol, t.exit_price, t.net_pnl_percent,
    al.position_peak_price, p.trailing_stop_armed
  FROM trades t
  LEFT JOIN audit_logs al ON al.trade_id = t.id
  LEFT JOIN positions  p
    ON p.symbol = t.symbol
   AND p.is_open = false
   AND p.updated_at BETWEEN t.exit_at - interval '5 minutes' AND t.exit_at + interval '5 minutes'
  WHERE t.status = 'CLOSED'
    AND t.exit_reason = 'AI_SELL'
    AND t.exit_at >= (:'since')::timestamp
)
SELECT
  COUNT(*) FILTER (WHERE position_peak_price IS NULL) AS unknown,
  COUNT(*) FILTER (
    WHERE position_peak_price IS NOT NULL
      AND exit_price > position_peak_price * (1 - :trail_pct / 100.0)
  ) AS beneficial,
  COUNT(*) FILTER (
    WHERE position_peak_price IS NOT NULL
      AND trailing_stop_armed = true
      AND exit_price < position_peak_price * (1 - :trail_pct / 100.0)
                      - position_peak_price * (:neutral_band_pct / 100.0)
  ) AS harmful,
  COUNT(*) AS total
FROM ai_exits;

\echo
\echo '── 6. Stability alerts ──'
\echo '       Tier 1 (HALT): 6a, 6b, 6e — production-grade defects'
\echo '       Tier 2 (NOTE): 6c, 6d     — instrumentation defects'
\echo

-- 6a. Tier 1 — profitable position closed below entry AFTER trail was armed.
SELECT
  '6a [TIER 1] profitable→loss after trail armed' AS check_name,
  COUNT(*) AS n
FROM trades t
JOIN positions p
  ON p.symbol = t.symbol
 AND p.is_open = false
 AND p.updated_at BETWEEN t.exit_at - interval '5 minutes' AND t.exit_at + interval '5 minutes'
WHERE t.status = 'CLOSED'
  AND t.exit_at >= (:'since')::timestamp
  AND p.trailing_stop_armed = true
  AND p.max_unrealized_pnl_pct > 0
  AND t.net_pnl_percent < 0

UNION ALL

-- 6b. Tier 1 — peak tracking stuck >1h on an open position.
SELECT
  '6b [TIER 1] peak stuck >1h on open position',
  COUNT(*)
FROM positions
WHERE is_open = true
  AND updated_at < now() - interval '1 hour'

UNION ALL

-- 6c. Tier 2 — trail never armed despite max_unrealized_pnl_pct >= arm threshold.
SELECT
  '6c [TIER 2] not armed despite max_unrealized >= ' || :arm_pct || '%',
  COUNT(*)
FROM positions
WHERE is_open = true
  AND max_unrealized_pnl_pct IS NOT NULL
  AND max_unrealized_pnl_pct >= :arm_pct
  AND trailing_stop_armed = false

UNION ALL

-- 6d. Tier 2 — AI eval audit row had a position open but position_* columns NULL.
SELECT
  '6d [TIER 2] AI audit row missing position_* despite open position',
  COUNT(*)
FROM audit_logs al
WHERE al.created_at >= (:'since')::timestamp
  AND al.interval != 'monitor'
  AND al.position_entry_price IS NULL
  AND EXISTS (
    SELECT 1 FROM positions p
    WHERE p.symbol = al.symbol
      AND p.is_open = true
      AND p.created_at <= al.created_at
  )

UNION ALL

-- 6e. Tier 1 — AI signal silently dropped (no trade, no blocked_by).
SELECT
  '6e [TIER 1] AI signal silently dropped (no trade, no blocked_by)',
  COUNT(*)
FROM audit_logs
WHERE created_at >= (:'since')::timestamp
  AND interval != 'monitor'
  AND action IN ('BUY', 'SELL')
  AND trade_id IS NULL
  AND (blocked_by IS NULL OR blocked_by = '')

ORDER BY 1;

\echo
\echo '═══ end of report ═══'
