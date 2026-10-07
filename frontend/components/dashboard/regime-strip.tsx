"use client";

import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { Sparkline } from "@/components/ui/sparkline";
import { useLivePrice } from "@/hooks/use-live-prices";
import {
  api,
  AVAILABLE_COINS,
  type AuditLog,
  type CoinOption,
  type HistoryPoint,
} from "@/lib/api";
import { cn, formatCurrency, formatPrice } from "@/lib/utils";

interface RegimeStripProps {
  /**
   * Watchlist to render. Defaults to AVAILABLE_COINS so the strip is
   * drop-in on any page that wants a market overview.
   */
  symbols?: CoinOption[];
  /**
   * Recent non-monitor audit logs from /audit-logs. Drives the last
   * verdict pill; empty array → "—".
   */
  recentAuditLogs?: AuditLog[];
  className?: string;
}

/**
 * Horizontal strip — one cell per watchlist symbol. Each cell answers
 * "where is this market and what is the AI saying about it" without
 * the operator having to pick a coin. Sparklines come from a local
 * 24-hour analytics query keyed on the constant symbol list so the
 * strip is fully self-contained; latest AI verdict is shared from the
 * parent's already-cached audit query to avoid duplicate fetches.
 */
export function RegimeStrip({
  symbols = AVAILABLE_COINS,
  recentAuditLogs = [],
  className,
}: RegimeStripProps) {
  const symbolKey = useMemo(() => symbols.map((s) => s.symbol), [symbols]);

  const { data: history } = useQuery({
    queryKey: ["regime-history", symbolKey],
    queryFn: () => api.getHistory(symbolKey, "24H"),
    refetchInterval: 30_000,
    staleTime: 15_000,
  });

  const sparkBySymbol = useMemo(() => {
    const map: Record<string, number[]> = {};
    for (const p of (history?.points ?? []) as HistoryPoint[]) {
      const arr = map[p.symbol] ?? (map[p.symbol] = []);
      const c = parseFloat(p.close);
      if (Number.isFinite(c)) arr.push(c);
    }
    return map;
  }, [history]);

  const verdictBySymbol = useMemo(() => {
    const map = new Map<string, AuditLog>();
    for (const log of recentAuditLogs) {
      if (log.interval === "monitor") continue;
      if (!map.has(log.symbol)) map.set(log.symbol, log);
    }
    return map;
  }, [recentAuditLogs]);

  return (
    <div
      className={cn(
        "grid gap-2 rounded-lg border border-border bg-card p-2 sm:grid-cols-2 md:grid-cols-4 2xl:grid-cols-8",
        className
      )}
    >
      {symbols.map((coin) => (
        <RegimeCell
          key={coin.symbol}
          coin={coin}
          points={sparkBySymbol[coin.symbol] ?? []}
          verdict={verdictBySymbol.get(coin.symbol) ?? null}
        />
      ))}
    </div>
  );
}

function RegimeCell({
  coin,
  points,
  verdict,
}: {
  coin: CoinOption;
  points: number[];
  verdict: AuditLog | null;
}) {
  const live = useLivePrice(coin.symbol);
  const last = points.length > 0 ? points[points.length - 1] : null;
  const first = points.length > 0 ? points[0] : null;
  const price = live?.price ?? last ?? 0;
  const hasPrice = price > 0;

  const pct =
    first != null && last != null && first > 0
      ? ((last - first) / first) * 100
      : null;
  const trendTone =
    pct == null ? "neutral" : pct >= 0 ? "profit" : "loss";

  return (
    <div className="flex flex-col gap-1 rounded-md border border-border/60 bg-background/40 p-2">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-1.5 min-w-0">
          <span
            className="flex h-4 w-4 shrink-0 items-center justify-center rounded text-micro font-bold"
            style={{ backgroundColor: `${coin.color}22`, color: coin.color }}
          >
            {coin.label.slice(0, 1)}
          </span>
          <span className="truncate text-micro font-semibold">{coin.label}</span>
        </div>
        <VerdictPill verdict={verdict} />
      </div>
      <div className="flex items-end justify-between gap-1">
        <div className="leading-tight">
          <div className="text-data font-mono font-semibold tabular-nums">
            {hasPrice ? formatPrice(price) : "—"}
          </div>
          <div
            className={cn(
              "text-micro font-mono tabular-nums",
              trendTone === "profit" && "text-profit",
              trendTone === "loss" && "text-loss",
              trendTone === "neutral" && "text-muted-foreground"
            )}
          >
            {pct == null
              ? "—"
              : `${pct >= 0 ? "+" : ""}${pct.toFixed(2)}%`}
          </div>
        </div>
        <Sparkline
          points={points}
          tone={trendTone === "neutral" ? "neutral" : trendTone}
          width={56}
          height={20}
        />
      </div>
    </div>
  );
}

function VerdictPill({ verdict }: { verdict: AuditLog | null }) {
  if (!verdict) {
    return (
      <span className="text-micro font-semibold uppercase tracking-wider text-muted-foreground/60">
        —
      </span>
    );
  }
  const action = verdict.action;
  const className = cn(
    "rounded px-1 py-0 text-micro font-semibold uppercase tracking-wider",
    action === "BUY" && "bg-profit/15 text-profit",
    action === "SELL" && "bg-loss/15 text-loss",
    action === "HOLD" && "bg-muted text-muted-foreground",
    action !== "BUY" && action !== "SELL" && action !== "HOLD" && "text-muted-foreground"
  );
  return (
    <span className={className} title={`Confidence ${(verdict.confidence * 100).toFixed(0)}%`}>
      {action || "—"}
    </span>
  );
}
