const API_URL = process.env.NEXT_PUBLIC_API_URL || "http://localhost:8081";
const WS_URL = process.env.NEXT_PUBLIC_WS_URL || "ws://localhost:8081/ws";

/** localStorage key for the bearer token that authenticates approval calls. */
export const APPROVAL_TOKEN_KEY = "cry5_approval_token";

/** Read the operator's bearer token from localStorage (SSR-safe). */
export function getApprovalToken(): string {
  if (typeof window === "undefined") return "";
  return window.localStorage.getItem(APPROVAL_TOKEN_KEY) || "";
}

/** Persist the operator's bearer token. Pass "" to clear. */
export function setApprovalToken(token: string): void {
  if (typeof window === "undefined") return;
  if (token) {
    window.localStorage.setItem(APPROVAL_TOKEN_KEY, token);
  } else {
    window.localStorage.removeItem(APPROVAL_TOKEN_KEY);
  }
}

/** ApprovalError carries the HTTP status so callers can render different
 *  states for 401 (no/invalid token), 409 (already decided), 410 (expired). */
export class ApprovalError extends Error {
  constructor(public status: number, message: string) {
    super(message);
    this.name = "ApprovalError";
  }
}

async function fetchAPI<T>(endpoint: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${API_URL}/api/v1${endpoint}`, {
    cache: "no-store",
    ...init,
  });
  if (!res.ok) {
    if (endpoint.startsWith("/signals")) {
      throw new ApprovalError(res.status, `${res.status} ${res.statusText}`);
    }
    throw new Error(`API error: ${res.status} ${res.statusText}`);
  }
  return res.json();
}

async function postAPI<T>(endpoint: string, body?: unknown, init?: RequestInit): Promise<T> {
  // init is spread first so its headers are merged below, not replace
  // the Content-Type.
  const res = await fetch(`${API_URL}/api/v1${endpoint}`, {
    ...init,
    method: "POST",
    headers: { "Content-Type": "application/json", ...(init?.headers || {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    if (endpoint.startsWith("/signals")) {
      throw new ApprovalError(res.status, `${res.status} ${res.statusText}`);
    }
    throw new Error(`API error: ${res.status} ${res.statusText}`);
  }
  return res.json();
}

/** authHeader returns the bearer-auth header object when a token is set,
 *  else an empty object. Standard, not bandaid: production endpoints
 *  reject empty/invalid tokens with 401 — there is no anonymous path. */
function authHeader(): Record<string, string> {
  const token = getApprovalToken();
  return token ? { Authorization: `Bearer ${token}` } : {};
}

export interface DashboardStats {
  total_trades: number;
  winning_trades: number;
  total_pnl: string;
  win_rate: number;
  open_positions: number;
  // CRY-11 — fee-aware figures alongside gross.
  net_pnl: string;
  total_fees: string;
  gross_vs_net_diff: string;
}

export interface Candle {
  id: number;
  symbol: string;
  interval: string;
  open_time: string;
  close_time: string;
  open: string;
  high: string;
  low: string;
  close: string;
  volume: string;
}

export interface Trade {
  id: number;
  symbol: string;
  side: "BUY" | "SELL";
  status: "OPEN" | "CLOSED" | "CANCELLED" | "FAILED";
  entry_price: string;
  exit_price: string;
  quantity: string;
  // CRY-6 / CRY-9 — cost & fee accounting. Backend serialises decimal as string.
  entry_cost: string;
  exit_cost: string;
  entry_fee: string;
  exit_fee: string;
  fee_currency: string;
  pnl: string;
  pnl_percent: string;
  net_pnl: string;
  net_pnl_percent: string;
  ai_confidence: number;
  ai_reasoning: string;
  entry_at: string;
  exit_at: string | null;
  created_at: string;
  // is_paper distinguishes a paper-mode simulated trade from a live fill.
  // Falsy (or missing on legacy rows pre-PR #21) is treated as live by
  // the UI, matching the backend default. Backfill in `database.Migrate`
  // flips legacy rows without a `client_order_id` to paper.
  is_paper: boolean;
}

export interface PortfolioBySymbol {
  symbol: string;
  total_invested: string;
  realized_pnl: string;
  unrealized_pnl: string;
  total_fees: string;
  open_qty: string;
  current_price: string;
  closed_trades: number;
}

export interface PortfolioAggregate {
  total_net_pnl: string;
  total_fees: string;
  total_invested: string;
  open_positions: number;
}

export interface PortfolioDailyPoint {
  date: string; // YYYY-MM-DD UTC
  net_pnl: string;
}

export interface PortfolioResponse {
  by_symbol: PortfolioBySymbol[];
  aggregate: PortfolioAggregate;
  daily_net_pnl: PortfolioDailyPoint[];
}

export interface Position {
  id: number;
  symbol: string;
  side: "BUY" | "SELL";
  quantity: string;
  avg_entry_price: string;
  current_price: string;
  unrealized_pnl: string;
  // Entry fee already paid on the open trade (USDT) — live PnL is
  // computed net of it.
  open_entry_fee?: string;
  is_open: boolean;
  // CRY-16 immutable exit plan captured at OPEN. Decimal strings on the
  // wire; null/missing when the AI declined to set them or for pre-CRY-16
  // legacy positions.
  stop_loss?: string | null;
  take_profit?: string | null;
  // CRY-34: set while a live SL / TP / trailing exit keeps failing to
  // sell on the venue — the coins are still held. Omitted otherwise.
  exit_failure?: ExitFailure | null;
}

export interface ExitFailure {
  exit_reason: string;
  reason: string;
  detail: string;
  attempt: number;
  last_failed_at: string;
  next_retry_at: string;
}

// ── Analytics types ──────────────────────────────────────

export interface HistoryPoint {
  time: string;
  symbol: string;
  close: string;
  volume: string;
  high: string;
  low: string;
}

export interface SymbolSummary {
  symbol: string;
  current_price: string;
  start_price: string;
  pnl_percent: string;
  pnl_absolute: string;
  high: string;
  low: string;
  avg_volume: string;
}

export interface HistoryResponse {
  points: HistoryPoint[];
  summaries: SymbolSummary[];
}

export type TimeRange = "1H" | "4H" | "24H" | "7D" | "30D" | "90D" | "1Y" | "ALL";

// ── Risk Management types ──────────────────────────────

export interface RiskStatus {
  halted: boolean;
  daily_pnl: string;
  daily_drawdown_pct: number;
  open_positions: number;
  max_open_positions: number;
  max_position_pct: number;
  daily_limit_pct: number;
  // Priority-symbol slot reservation (default ETHUSDT). Empty string =
  // gate disabled. `priority_reserved` flips true when the last open
  // slot is being held for the priority symbol — useful for surfacing
  // why a non-priority BUY signal was rejected.
  priority_symbol: string;
  priority_reserved: boolean;
}

// ── Audit Log types ────────────────────────────────────

export interface AuditLog {
  id: number;
  trade_id: number | null;
  symbol: string;
  interval: string;
  // Only on the single-entry endpoints; the list omits it (CRY-38) and it
  // is cleared after AUDIT_TEXT_RETENTION_DAYS.
  prompt?: string;
  market_price: string;
  market_rsi: number;
  market_macd: number;
  market_signal_line: number;
  market_volume: number;
  raw_response?: string;
  action: string;
  confidence: number;
  reasoning: string;
  executed: boolean;
  blocked_by: string;
  prompt_sent_at: string;
  response_at: string;
  inference_time_ms: number;
  created_at: string;
}

export interface AuditLogResponse {
  total: number;
  logs: AuditLog[];
}

// ── Approval gate (CRY-5) ──────────────────────────────

export type PendingSignalStatus =
  | "pending"
  | "approved"
  | "executing"
  | "rejected"
  | "expired"
  | "submitted"
  | "failed";

export interface PendingSignal {
  id: number;
  symbol: string;
  interval: string;
  audit_log_id: number;
  action: "BUY" | "SELL";
  confidence: number;
  reasoning: string;
  proposed_sl: string;
  proposed_tp: string;
  market_price: string;
  status: PendingSignalStatus;
  rejected_reason?: string;
  decided_at?: string;
  decided_by?: string;
  approval_source?: string;
  approval_ip?: string;
  approval_user_agent?: string;
  expires_at: string; // RFC3339
  executing_at?: string;
  trade_id?: number;
  created_at: string;
}

export interface PendingListResponse {
  pending: PendingSignal[];
}

export interface ApproveResult {
  status: "submitted";
  pending_signal_id: number;
  trade_id: number;
}

export interface RejectResult {
  status: "rejected";
  pending_signal_id: number;
  reason: string;
}

// ── AI provider health (CRY-17) ────────────────────────

/**
 * Mirrors backend ai.HealthSnapshot. `healthy=false` is normal during
 * Ollama warmup or short outages — the panel shows the state, no 5xx.
 */
export interface AIHealthSnapshot {
  provider: string;
  healthy: boolean;
  last_check_at: string;
  last_error?: string;
  breaker_state?: string;
}

// ── Realtime infra health ──────────────────────────────

/**
 * Snapshot of the backend realtime pipeline health, served by the
 * top-level `/health` endpoint. Each sub-object mirrors the Health()
 * map returned by the matching backend component.
 */
export interface BackendHealth {
  status: string;
  uptime_s: number;
  timestamp: string;
  collector?: {
    messages_total: number;
    reconnects_total: number;
    current_uptime_s: number;
    last_error: string;
  };
  persister?: {
    candles_written: number;
    write_failures: number;
    last_write_at: string | null;
  };
  hub?: {
    clients: number;
    messages_sent: number;
    messages_queued: number;
    clients_dropped: number;
  };
}

async function fetchHealth(): Promise<BackendHealth> {
  const res = await fetch(`${API_URL}/health`, { cache: "no-store" });
  if (!res.ok) throw new Error(`health: ${res.status}`);
  return res.json();
}

// ── Backtest types ─────────────────────────────────────

export interface BacktestConfig {
  symbol: string;
  interval: string;
  start?: string;
  end?: string;
  capital?: number;
  window?: number;
  step_size?: number;
}

export interface BacktestTrade {
  entry_time: string;
  exit_time: string;
  side: string;
  entry_price: string;
  exit_price: string;
  quantity: string;
  pnl: string;
  pnl_pct: string;
  confidence: number;
  reasoning: string;
}

export interface BacktestResult {
  config: BacktestConfig;
  trades: BacktestTrade[];
  total_trades: number;
  winning_trades: number;
  losing_trades: number;
  win_rate: number;
  total_pnl: string;
  total_pnl_pct: string;
  max_drawdown: string;
  max_drawdown_pct: string;
  sharpe_ratio: number;
  final_equity: string;
  started_at: string;
  completed_at: string;
  duration_sec: number;
  error?: string;
}

export interface BacktestResponse {
  status: "running" | "completed" | "no backtest has been run yet";
  result?: BacktestResult;
}

// ── Available coins ─────────────────────────────────────

export interface CoinOption {
  symbol: string;
  label: string;
  color: string;
}

export const AVAILABLE_COINS: CoinOption[] = [
  { symbol: "BTCUSDT", label: "BTC", color: "#F7931A" },
  { symbol: "ETHUSDT", label: "ETH", color: "#627EEA" },
  { symbol: "SOLUSDT", label: "SOL", color: "#00FFA3" },
  { symbol: "BNBUSDT", label: "BNB", color: "#F3BA2F" },
  { symbol: "XRPUSDT", label: "XRP", color: "#00AAE4" },
  { symbol: "ADAUSDT", label: "ADA", color: "#0033AD" },
  { symbol: "DOGEUSDT", label: "DOGE", color: "#C3A634" },
  { symbol: "AVAXUSDT", label: "AVAX", color: "#E84142" },
];

// ── API client ──────────────────────────────────────────

export const api = {
  getStats: () => fetchAPI<DashboardStats>("/stats"),
  getPortfolio: () => fetchAPI<PortfolioResponse>("/portfolio"),
  getCandles: (symbol = "BTCUSDT", interval = "1h", limit = 300) =>
    fetchAPI<Candle[]>(
      `/candles?symbol=${symbol}&interval=${interval}&limit=${limit}`
    ),
  getTrades: () => fetchAPI<Trade[]>("/trades"),
  getPositions: () => fetchAPI<Position[]>("/positions"),
  getHistory: (symbols: string[], range: TimeRange) =>
    fetchAPI<HistoryResponse>(
      `/analytics/history?symbols=${symbols.join(",")}&range=${range}`
    ),

  // Risk management
  getRiskStatus: () => fetchAPI<RiskStatus>("/risk/status"),
  // Operator actions carry the bearer token when one is set (CRY-75).
  resetCircuitBreaker: () => postAPI<{ status: string }>("/risk/reset", undefined, { headers: authHeader() }),

  // Audit logs
  getAuditLogs: (symbol?: string, limit = 50, offset = 0) =>
    fetchAPI<AuditLogResponse>(
      `/audit-logs?${symbol ? `symbol=${symbol}&` : ""}limit=${limit}&offset=${offset}`
    ),
  getAuditLog: (id: number) => fetchAPI<AuditLog>(`/audit-logs/${id}`),
  getTradeAudit: (tradeId: number) => fetchAPI<AuditLog>(`/trades/${tradeId}/audit`),

  // AI provider health (CRY-17)
  getAIHealth: () => fetchAPI<AIHealthSnapshot>("/ai/health"),

  // Backtesting
  runBacktest: (config: BacktestConfig) =>
    postAPI<{ status: string; message: string }>("/backtest", config, { headers: authHeader() }),
  getBacktestResult: () => fetchAPI<BacktestResponse>("/backtest"),

  // Infra health (not under /api/v1)
  getBackendHealth: () => fetchHealth(),

  // Approval gate (CRY-5) — all three require bearer auth in live mode.
  getPendingSignals: () =>
    fetchAPI<PendingListResponse>("/signals/pending", { headers: authHeader() }),
  approveSignal: (id: number) =>
    postAPI<ApproveResult>(`/signals/${id}/approve`, undefined, { headers: authHeader() }),
  rejectSignal: (id: number, reason?: string) =>
    postAPI<RejectResult>(`/signals/${id}/reject`, { reason }, { headers: authHeader() }),
};

export function createWebSocket(onMessage: (data: unknown) => void): { close: () => void } {
  let ws: WebSocket | null = null;
  let closed = false;

  function connect() {
    if (closed) return;
    ws = new WebSocket(WS_URL);
    ws.onopen = () => console.log("[ws] connected");
    ws.onmessage = (event) => {
      try {
        const data = JSON.parse(event.data);
        onMessage(data);
      } catch (e) {
        console.error("[ws] parse error:", e);
      }
    };
    ws.onerror = (e) => console.error("[ws] error:", e);
    ws.onclose = () => {
      if (!closed) {
        console.log("[ws] disconnected, reconnecting in 3s...");
        setTimeout(connect, 3000);
      }
    };
  }

  connect();
  return {
    close: () => {
      closed = true;
      ws?.close();
    },
  };
}
