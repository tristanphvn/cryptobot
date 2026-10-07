"use client";

import { Suspense, useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Sidebar } from "@/components/layout/sidebar";
import { TopStatusBar } from "@/components/dashboard/top-status-bar";
import { CoinPicker } from "@/components/dashboard/coin-picker";
import { CoinSummaryCards } from "@/components/dashboard/coin-summary-cards";
import { MultiLineChart } from "@/components/charts/multi-line-chart";
import { PriceChart } from "@/components/charts/price-chart";
import { useWebSocket, type WSCandle } from "@/hooks/use-websocket";
import { usePriceChart } from "@/hooks/use-price-chart";
import { useUrlState } from "@/hooks/use-url-state";
import { useKeyboardShortcuts, SHORTCUT_MAP } from "@/hooks/use-keyboard-shortcuts";
import { inferTickInterval } from "@/components/ai/ai-symbol-card";
import { api, AVAILABLE_COINS, type AuditLog } from "@/lib/api";
import { cn } from "@/lib/utils";

export default function ChartPage() {
  return (
    <Suspense>
      <ChartContent />
    </Suspense>
  );
}

function ChartContent() {
  // URL-synced selection — `?coins=BTCUSDT,ETHUSDT&range=24H`. Bare
  // URL hands off to the AI-driven derivation below so the page never
  // shows a blank chart on cold start.
  const { symbols: urlSymbols, coinsExplicit, timeRange, setSymbols, setTimeRange } = useUrlState();

  const [activeChartSymbol, setActiveChartSymbol] = useState<string>("");
  const [chartInterval, setChartInterval] = useState("1m");

  const onTickRef = useRef<((c: WSCandle) => void) | null>(null);
  const wsHealth = useWebSocket({
    throttleMs: 3000,
    onCandle: (c) => onTickRef.current?.(c),
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

  const openPositions = useMemo(
    () => positions.filter((p) => p.is_open),
    [positions]
  );

  // Bare-URL fallback: drive the chart from whatever the AI is
  // currently signaling on (or whichever symbols hold an open
  // position). Capped so the multi-line chart stays legible. Priority
  // symbol guarantees a non-empty set at cold start.
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

  const symbols = coinsExplicit ? urlSymbols : aiDrivenSymbols;

  // Keep the detail-chart symbol valid as the watchlist set changes.
  useEffect(() => {
    if (symbols.length === 0) {
      if (activeChartSymbol !== "") setActiveChartSymbol("");
      return;
    }
    if (!symbols.includes(activeChartSymbol)) {
      setActiveChartSymbol(symbols[0]);
    }
  }, [symbols, activeChartSymbol]);

  const activeSymbol = activeChartSymbol || symbols[0] || "";

  const { candles, onTick } = usePriceChart({
    symbol: activeSymbol,
    interval: chartInterval,
    enabled: !!activeSymbol,
    reconnectKey: wsHealth.reconnectCount,
  });
  onTickRef.current = onTick;

  const { data: historyData, isLoading: historyLoading } = useQuery({
    queryKey: ["analytics-history", symbols, timeRange],
    queryFn: () => api.getHistory(symbols, timeRange),
    enabled: symbols.length > 0,
    refetchInterval: 5_000,
  });

  const FULL_WATCHLIST = useMemo(
    () => AVAILABLE_COINS.map((c) => c.symbol),
    []
  );
  const shortcuts = useMemo(
    () => [
      {
        key: SHORTCUT_MAP.NEXT_SYMBOL.key,
        description: "Next symbol",
        action: () => {
          if (symbols.length >= 2) {
            const idx = symbols.indexOf(activeSymbol);
            setActiveChartSymbol(symbols[(idx + 1) % symbols.length]);
          } else {
            const idx = FULL_WATCHLIST.indexOf(activeSymbol);
            const next = FULL_WATCHLIST[(idx + 1) % FULL_WATCHLIST.length];
            setSymbols([next]);
          }
        },
      },
      {
        key: SHORTCUT_MAP.PREV_SYMBOL.key,
        description: "Previous symbol",
        action: () => {
          if (symbols.length >= 2) {
            const idx = symbols.indexOf(activeSymbol);
            const prev = symbols[(idx - 1 + symbols.length) % symbols.length];
            setActiveChartSymbol(prev);
          } else {
            const idx = FULL_WATCHLIST.indexOf(activeSymbol);
            const prev =
              FULL_WATCHLIST[(idx - 1 + FULL_WATCHLIST.length) % FULL_WATCHLIST.length];
            setSymbols([prev]);
          }
        },
      },
    ],
    [activeSymbol, symbols, setSymbols, FULL_WATCHLIST]
  );

  useKeyboardShortcuts(shortcuts);

  return (
    <div className="flex min-h-screen bg-background">
      <Sidebar />

      <main className="flex-1 pl-14">
        <TopStatusBar pageLabel="Chart" wsHealth={wsHealth} />

        <div className="space-y-4 p-5">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <CoinPicker selected={symbols} onChange={setSymbols} />
          </div>

          <CoinSummaryCards
            summaries={historyData?.summaries || []}
            isLoading={historyLoading}
          />

          <MultiLineChart
            points={historyData?.points || []}
            symbols={symbols}
            isLoading={historyLoading}
            timeRange={timeRange}
            onTimeRangeChange={setTimeRange}
          />

          {activeSymbol && (
            <div className="space-y-2">
              {symbols.length > 1 && (
                <div className="flex flex-wrap items-center gap-1 rounded-lg bg-muted/50 p-1">
                  <span className="px-2 text-micro uppercase tracking-wider text-muted-foreground">
                    Detail
                  </span>
                  {symbols.map((s) => {
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
        </div>
      </main>
    </div>
  );
}
