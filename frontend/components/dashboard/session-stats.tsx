"use client";

import { useMemo } from "react";
import { Metric } from "@/components/ui/metric";
import type { Trade } from "@/lib/api";
import { cn } from "@/lib/utils";

interface SessionStatsProps {
  trades: Trade[];
  className?: string;
}

/**
 * Compact KPI strip above the recent-trades blotter. Computed entirely
 * client-side from the trades query already in flight — no new fetch.
 * Reads three numbers a desk trader checks first when scanning the
 * session: win rate, average net P&L %, and the active win/loss streak.
 *
 * Closed trades only. An OPEN trade has no realized outcome and would
 * skew the streak — those rows are excluded.
 */
export function SessionStats({ trades, className }: SessionStatsProps) {
  const { winRate, avgNetPct, streak, streakKind, sampleSize } = useMemo(
    () => computeStats(trades),
    [trades]
  );

  const streakLabel =
    streakKind === "none"
      ? "—"
      : `${streak} ${streakKind === "win" ? "W" : "L"}`;

  return (
    <div
      className={cn(
        "flex flex-wrap items-center gap-6 rounded-lg border border-border bg-card px-4 py-2",
        className
      )}
    >
      <Metric
        label="Win Rate"
        value={
          sampleSize === 0 ? "—" : `${(winRate * 100).toFixed(1)}%`
        }
        tone={
          sampleSize === 0
            ? "neutral"
            : winRate >= 0.55
            ? "profit"
            : winRate >= 0.45
            ? "neutral"
            : "loss"
        }
      />
      <Metric
        label="Avg Net %"
        value={
          sampleSize === 0
            ? "—"
            : `${avgNetPct >= 0 ? "+" : ""}${avgNetPct.toFixed(2)}%`
        }
        tone={
          sampleSize === 0
            ? "neutral"
            : avgNetPct > 0
            ? "profit"
            : avgNetPct < 0
            ? "loss"
            : "neutral"
        }
      />
      <Metric
        label="Streak"
        value={streakLabel}
        tone={
          streakKind === "win" ? "profit" : streakKind === "loss" ? "loss" : "neutral"
        }
      />
      <Metric
        label="Closed"
        value={sampleSize.toString()}
        tone="info"
        compact
      />
    </div>
  );
}

function computeStats(trades: Trade[]): {
  winRate: number;
  avgNetPct: number;
  streak: number;
  streakKind: "win" | "loss" | "none";
  sampleSize: number;
} {
  const closed = trades.filter((t) => t.status === "CLOSED");
  if (closed.length === 0) {
    return { winRate: 0, avgNetPct: 0, streak: 0, streakKind: "none", sampleSize: 0 };
  }

  let wins = 0;
  let sumNetPct = 0;
  for (const t of closed) {
    const net = parseFloat(t.net_pnl || "0");
    if (net > 0) wins++;
    sumNetPct += parseFloat(t.net_pnl_percent || "0");
  }

  // Streak walks the trades in exit-time order (newest first). Closed
  // trades come from the API DESC by created_at; for runs we want
  // outcome chronology — accept the API order as a reasonable proxy
  // (a trade closes shortly after it opens for this bot's cadence).
  const sortedByExit = [...closed].sort((a, b) => {
    const aExit = a.exit_at ? Date.parse(a.exit_at) : Date.parse(a.created_at);
    const bExit = b.exit_at ? Date.parse(b.exit_at) : Date.parse(b.created_at);
    return bExit - aExit;
  });
  const firstOutcome = parseFloat(sortedByExit[0].net_pnl || "0") >= 0 ? "win" : "loss";
  let streak = 0;
  for (const t of sortedByExit) {
    const isWin = parseFloat(t.net_pnl || "0") >= 0;
    const kind = isWin ? "win" : "loss";
    if (kind !== firstOutcome) break;
    streak++;
  }

  return {
    winRate: wins / closed.length,
    avgNetPct: sumNetPct / closed.length,
    streak,
    streakKind: firstOutcome,
    sampleSize: closed.length,
  };
}
