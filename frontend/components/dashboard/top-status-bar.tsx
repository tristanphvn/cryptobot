"use client";

import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { cn, formatCurrency } from "@/lib/utils";
import { HealthDot, type HealthStatus } from "@/components/ui/health-dot";
import { ModeChip, type ExecutionMode } from "@/components/ui/mode-chip";
import { Metric } from "@/components/ui/metric";
import { KillSwitch } from "@/components/dashboard/kill-switch";
import { ExitFailureBanner } from "@/components/dashboard/exit-failure-banner";
import { api, type DashboardStats, type RiskStatus } from "@/lib/api";
import type { WSHealth } from "@/hooks/use-websocket";

interface TopStatusBarProps {
  /** Page label rendered on the far left next to the mode chip. */
  pageLabel?: string;
  /** Override mode; default reads NEXT_PUBLIC_EXECUTION_MODE. */
  mode?: ExecutionMode;
  /**
   * WS health from Mission Control's `useWebSocket`. Pages without
   * an active WS connection omit this and the WS/DB dots render
   * "idle" — accurate, not misleading.
   */
  wsHealth?: WSHealth;
  /**
   * Latest Ollama inference latency (ms). Defaults to internally
   * polling the most recent audit log so every page can show the
   * LLM health dot without wiring its own query.
   */
  ollamaInferenceMs?: number | null;
  /** Overrides for the auto-fetched stats / risk (Mission Control passes its existing queries). */
  stats?: DashboardStats | null;
  risk?: RiskStatus | null;
}

const DEFAULT_MODE: ExecutionMode =
  ((process.env.NEXT_PUBLIC_EXECUTION_MODE as ExecutionMode) || "dry-run");

/**
 * TopStatusBar — sticky 44px bar that answers the three operator
 * questions at a glance:
 *   1. Capital at risk         (Net P&L, Daily P&L vs cap, Open n/m, Exposure)
 *   2. System action posture   (mode chip, halted state)
 *   3. Runtime integrity       (WS / LLM / DB health dots, Kill/Pause)
 *
 * Replaces TopStrip + sidebar's RiskGauge + sidebar's ConnectionHealth +
 * the page-level Header/KillSwitch row. Every route mounts this so the
 * operator never loses status when navigating.
 *
 * Mode wiring: reads NEXT_PUBLIC_EXECUTION_MODE for now. A future backend
 * change should add `execution_mode` to /api/v1/risk/status so the chip
 * tracks reality instead of build-time config.
 */
export function TopStatusBar({
  pageLabel,
  mode = DEFAULT_MODE,
  wsHealth,
  ollamaInferenceMs,
  stats: statsProp,
  risk: riskProp,
}: TopStatusBarProps) {
  // Shared queries — React Query dedupes by key so Mission Control's
  // existing queries and this bar use the same cached data.
  const { data: statsAuto } = useQuery({
    queryKey: ["stats"],
    queryFn: api.getStats,
    refetchInterval: 5_000,
    enabled: statsProp === undefined,
  });
  const { data: riskAuto } = useQuery({
    queryKey: ["risk-status"],
    queryFn: api.getRiskStatus,
    refetchInterval: 5_000,
    enabled: riskProp === undefined,
  });
  const { data: latestAudit } = useQuery({
    queryKey: ["audit-logs-latest"],
    queryFn: () => api.getAuditLogs(undefined, 1, 0),
    refetchInterval: 10_000,
    enabled: ollamaInferenceMs === undefined,
  });

  const stats = statsProp ?? statsAuto ?? null;
  const risk = riskProp ?? riskAuto ?? null;
  const ollamaMs =
    ollamaInferenceMs !== undefined
      ? ollamaInferenceMs
      : latestAudit?.logs?.[0]?.inference_time_ms ?? null;

  const netPnl = parseFloat(stats?.total_pnl || "0");
  const dailyPnl = parseFloat(risk?.daily_pnl || "0");
  const dailyLimitPct = risk?.daily_limit_pct || 5;
  const dailyDrawdownPct = Math.abs(risk?.daily_drawdown_pct ?? 0);
  const isDailyLoss = dailyPnl < 0;

  const openPositions = risk?.open_positions ?? stats?.open_positions ?? 0;
  const maxOpen = risk?.max_open_positions ?? 3;
  const halted = risk?.halted ?? false;

  // ── Health dots ─────────────────────────────────────────
  const wsStatus: HealthStatus = useMemo(() => {
    if (!wsHealth) return "idle";
    if (!wsHealth.connected) return "down";
    if (wsHealth.reconnectCount > 0) return "warn";
    return "ok";
  }, [wsHealth]);

  const ollamaStatus: HealthStatus = useMemo(() => {
    if (ollamaMs == null) return "idle";
    if (ollamaMs > 30_000) return "warn";
    return "ok";
  }, [ollamaMs]);

  const dbStatus: HealthStatus = useMemo(() => {
    if (!wsHealth?.lastMessageAt) return "idle";
    const ageSec = (Date.now() - wsHealth.lastMessageAt) / 1000;
    if (ageSec > 30) return "warn";
    return "ok";
  }, [wsHealth?.lastMessageAt]);

  return (
    <div className="sticky top-0 z-40">
      <div
        className={cn(
          "flex h-11 items-center gap-3 border-b border-border bg-card/95 px-4 backdrop-blur-sm",
          // LIVE-mode shell ring is painted at the <body> level in
          // app/layout.tsx — bar keeps only the standard divider here.
          mode === "live" && "border-b-loss/40"
        )}
      >
        {/* ── Left: mode + page label ───────────────────────── */}
        <div className="flex items-center gap-2">
          <ModeChip mode={mode} />
          {pageLabel && (
            <>
              <div className="h-3 w-px bg-border" />
              <span className="text-data font-medium tracking-tight text-foreground">
                {pageLabel}
              </span>
            </>
          )}
        </div>

        <div className="mx-1 h-5 w-px bg-border" />

        {/* ── Center: KPI metrics ──────────────────────────── */}
        <div className="flex flex-1 items-center gap-5 overflow-hidden">
          <Metric
            label="Net P&L"
            value={`${netPnl >= 0 ? "+" : ""}${formatCurrency(netPnl)}`}
            tone={netPnl >= 0 ? "profit" : "loss"}
          />
          <Metric
            label="Daily"
            value={
              isDailyLoss
                ? `−${formatCurrency(Math.abs(dailyPnl))}`
                : dailyPnl > 0
                ? `+${formatCurrency(dailyPnl)}`
                : "$0.00"
            }
            tone={isDailyLoss ? "loss" : dailyPnl > 0 ? "profit" : "neutral"}
            bar={
              isDailyLoss
                ? {
                    value: dailyDrawdownPct,
                    cap: dailyLimitPct,
                    tone:
                      dailyDrawdownPct > dailyLimitPct * 0.8
                        ? "loss"
                        : dailyDrawdownPct > dailyLimitPct * 0.5
                        ? "warning"
                        : "profit",
                  }
                : undefined
            }
          />
          <Metric
            label="Open"
            value={`${openPositions}/${maxOpen}`}
            tone={openPositions >= maxOpen ? "warning" : "neutral"}
          />
          <Metric
            label="Win Rate"
            value={
              stats?.win_rate != null
                ? `${(stats.win_rate * 100).toFixed(1)}%`
                : "—"
            }
            tone="info"
            compact
          />
          <Metric
            label="Trades"
            value={stats?.total_trades?.toString() ?? "0"}
            tone="neutral"
            compact
          />
          {halted && (
            <span className="rounded border border-loss/50 bg-loss/15 px-2 py-0.5 text-micro font-semibold uppercase tracking-wider text-loss">
              HALTED
            </span>
          )}
        </div>

        <div className="mx-1 h-5 w-px bg-border" />

        {/* ── Right: health dots + actions ─────────────────── */}
        <div className="flex items-center gap-3">
          <HealthTriplet
            wsStatus={wsStatus}
            ollamaStatus={ollamaStatus}
            dbStatus={dbStatus}
            wsLatency={wsHealth?.latencyMs ?? null}
            ollamaMs={ollamaMs}
          />
          <KillSwitch halted={halted} />
        </div>
      </div>
      <ExitFailureBanner />
    </div>
  );
}

function HealthTriplet({
  wsStatus,
  ollamaStatus,
  dbStatus,
  wsLatency,
  ollamaMs,
}: {
  wsStatus: HealthStatus;
  ollamaStatus: HealthStatus;
  dbStatus: HealthStatus;
  wsLatency: number | null;
  ollamaMs: number | null;
}) {
  return (
    <div className="flex items-center gap-3 text-micro font-mono tabular-nums text-muted-foreground">
      <span
        className="flex items-center gap-1"
        title={`Binance WS ${wsStatus}${wsLatency != null ? ` · ${wsLatency}ms` : ""}`}
      >
        <HealthDot status={wsStatus} />
        <span className="hidden md:inline">WS</span>
      </span>
      <span
        className="flex items-center gap-1"
        title={`Ollama ${ollamaStatus}${ollamaMs != null ? ` · ${(ollamaMs / 1000).toFixed(1)}s` : ""}`}
      >
        <HealthDot status={ollamaStatus} />
        <span className="hidden md:inline">LLM</span>
      </span>
      <span className="flex items-center gap-1" title={`DB sync ${dbStatus}`}>
        <HealthDot status={dbStatus} />
        <span className="hidden md:inline">DB</span>
      </span>
    </div>
  );
}
