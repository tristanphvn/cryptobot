"use client";

import { Suspense, useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Sidebar } from "@/components/layout/sidebar";
import { TopStatusBar } from "@/components/dashboard/top-status-bar";
import { RegimeStrip } from "@/components/dashboard/regime-strip";
import { OpenPositionsBlotter } from "@/components/dashboard/open-positions-blotter";
import { ApprovalCard } from "@/components/dashboard/approval-card";
import { RecentTradesBlotter } from "@/components/dashboard/recent-trades-blotter";
import { SessionStats } from "@/components/dashboard/session-stats";
import { CommandPalette } from "@/components/command-palette";
import { PriceChart } from "@/components/charts/price-chart";
import { useWebSocket, type WSCandle } from "@/hooks/use-websocket";
import { usePriceChart } from "@/hooks/use-price-chart";
import { inferTickInterval } from "@/components/ai/ai-symbol-card";
import { api, AVAILABLE_COINS, type AuditLog } from "@/lib/api";
import { cn } from "@/lib/utils";

const WATCHLIST_SYMBOLS = AVAILABLE_COINS.map((c) => c.symbol);

export default function DashboardPage() {
  return (
    <Suspense>
      <DashboardContent />
    </Suspense>
  );
}

function DashboardContent() {
  const [paused, setPaused] = useState(false);
  const [activeChartSymbol, setActiveChartSymbol] = useState<string>("");
  const [chartInterval, setChartInterval] = useState("1m");

  // WS feeds both: live-price store for blotter rows AND candle stream
  // for the inline PriceChart. onTickRef indirection lets usePriceChart
  // register its handler after this hook returns.
  const onTickRef = useRef<((c: WSCandle) => void) | null>(null);
  const wsHealth = useWebSocket({
    throttleMs: 3000,
    onCandle: (c) => onTickRef.current?.(c),
  });

  const { data: stats } = useQuery({
    queryKey: ["stats"],
    queryFn: api.getStats,
    refetchInterval: 5_000,
  });

  const { data: trades = [] } = useQuery({
    queryKey: ["trades"],
    queryFn: api.getTrades,
    refetchInterval: 5_000,
  });

  const { data: positions = [] } = useQuery({
    queryKey: ["positions"],
    queryFn: api.getPositions,
    refetchInterval: 5_000,
  });

  const { data: riskStatus } = useQuery({
    queryKey: ["risk-status"],
    queryFn: api.getRiskStatus,
    refetchInterval: 5_000,
  });

  const { data: recentAudit } = useQuery({
    queryKey: ["audit-logs-recent"],
    queryFn: () => api.getAuditLogs(undefined, 100, 0),
    refetchInterval: 10_000,
  });

  // 24h watchlist history feeds the OpenPositionsBlotter sparkline
  // column. Same query key the RegimeStrip uses, so React Query
  // dedupes — one network fetch covers both surfaces.
  const { data: regimeHistory } = useQuery({
    queryKey: ["regime-history", WATCHLIST_SYMBOLS],
    queryFn: () => api.getHistory(WATCHLIST_SYMBOLS, "24H"),
    refetchInterval: 30_000,
    staleTime: 15_000,
  });

  const latestInferenceMs = recentAudit?.logs?.[0]?.inference_time_ms ?? null;

  const sparkPointsBySymbol = useMemo(() => {
    const out: Record<string, number[]> = {};
    for (const p of regimeHistory?.points ?? []) {
      const arr = out[p.symbol] ?? (out[p.symbol] = []);
      const close = parseFloat(p.close);
      if (Number.isFinite(close)) arr.push(close);
    }
    return out;
  }, [regimeHistory]);

  // Mirror of /chart's AI-driven fallback so the inline chart cold-
  // starts on whatever the AI is actually trading right now (or open
  // positions), instead of forcing the operator to pick. Capped at 4 so
  // the Detail toggle stays scannable.
  const openPositions = useMemo(
    () => positions.filter((p) => p.is_open),
    [positions]
  );

  const aiDrivenSymbols = useMemo(() => {
    const PRIORITY_FALLBACK = riskStatus?.priority_symbol || "ETHUSDT";
    const CAP = 4;
    const STALE_TICK_MULTIPLIER = 2;
    const logsBySymbol = new Map<string, AuditLog[]>();
    for (const log of recentAudit?.logs ?? []) {
      if (log.interval === "monitor") continue;
      const list = logsBySymbol.get(log.symbol) ?? [];
      list.push(log);
      logsBySymbol.set(log.symbol, list);
    }
    const interesting: { symbol: string; at: number }[] = [];
    const openSet = new Set(openPositions.map((p) => p.symbol));
    const now = Date.now();
    for (const [symbol, logs] of logsBySymbol) {
      const latest = logs[0];
      const at = Date.parse(latest.prompt_sent_at) || 0;
      if (openSet.has(symbol)) {
        interesting.push({ symbol, at });
        continue;
      }
      if (latest.action === "HOLD") continue;
      if (latest.executed && latest.action === "BUY") continue;
      const tickMs = inferTickInterval(logs);
      if (now - at > STALE_TICK_MULTIPLIER * tickMs) continue;
      interesting.push({ symbol, at });
    }
    for (const pos of openPositions) {
      if (!logsBySymbol.has(pos.symbol)) interesting.push({ symbol: pos.symbol, at: 0 });
    }
    interesting.sort((a, b) => b.at - a.at);
    const out = interesting.slice(0, CAP).map((e) => e.symbol);
    if (out.length === 0 && AVAILABLE_COINS.some((c) => c.symbol === PRIORITY_FALLBACK)) {
      out.push(PRIORITY_FALLBACK);
    }
    return out;
  }, [recentAudit, openPositions, riskStatus]);

  useEffect(() => {
    if (aiDrivenSymbols.length === 0) {
      if (activeChartSymbol !== "") setActiveChartSymbol("");
      return;
    }
    if (!aiDrivenSymbols.includes(activeChartSymbol)) {
      setActiveChartSymbol(aiDrivenSymbols[0]);
    }
  }, [aiDrivenSymbols, activeChartSymbol]);

  const activeSymbol = activeChartSymbol || aiDrivenSymbols[0] || "";

  const { candles, onTick } = usePriceChart({
    symbol: activeSymbol,
    interval: chartInterval,
    enabled: !!activeSymbol,
    reconnectKey: wsHealth.reconnectCount,
  });
  onTickRef.current = onTick;

  return (
    <div className="flex min-h-screen bg-background">
      <Sidebar />

      <main className="flex-1 pl-14">
        <TopStatusBar
          stats={stats || null}
          risk={riskStatus || null}
          wsHealth={wsHealth}
          ollamaInferenceMs={latestInferenceMs}
          pageLabel="Mission Control"
        />

        {/* Single-column stack — full-width blotters preserve column
            density (SL/TP/sparkline) without squeezing, and vertical
            order restores hierarchy: overview → urgent → at-risk →
            decisions → stats → history. */}
        <div className="mx-auto max-w-[1400px] space-y-4 p-5">
          <RegimeStrip recentAuditLogs={recentAudit?.logs ?? []} />
          <ApprovalCard />
          <OpenPositionsBlotter
            positions={positions}
            sparkPointsBySymbol={sparkPointsBySymbol}
          />

          {activeSymbol && (
            <div className="space-y-2">
              {aiDrivenSymbols.length > 1 && (
                <div className="flex flex-wrap items-center gap-1 rounded-lg bg-muted/50 p-1">
                  <span className="px-2 text-micro uppercase tracking-wider text-muted-foreground">
                    Detail
                  </span>
                  {aiDrivenSymbols.map((s) => {
                    const coin = AVAILABLE_COINS.find((c) => c.symbol === s);
                    const active = s === activeSymbol;
                    return (
                      <button
                        key={s}
                        type="button"
                        onClick={() => setActiveChartSymbol(s)}
                        className={cn(
                          "flex items-center gap-1.5 rounded-md px-2.5 py-1 text-data font-medium transition-all duration-150",
                          active
                            ? "bg-background text-foreground shadow-sm"
                            : "text-muted-foreground hover:bg-background/50 hover:text-foreground"
                        )}
                      >
                        <span
                          className="h-2 w-2 rounded-full"
                          style={{ backgroundColor: coin?.color ?? "#888" }}
                        />
                        {coin?.label ?? s}
                      </button>
                    );
                  })}
                </div>
              )}
              <PriceChart
                candles={candles}
                trades={trades.filter((t) => t.symbol === activeSymbol)}
                symbol={activeSymbol.replace("USDT", "/USDT")}
                interval={chartInterval}
                onIntervalChange={setChartInterval}
              />
            </div>
          )}

          <SessionStats trades={trades} />
          <RecentTradesBlotter trades={trades} positions={positions} />
        </div>
      </main>

      <CommandPalette
        onPauseToggle={() => setPaused(!paused)}
        isPaused={paused}
      />
    </div>
  );
}
