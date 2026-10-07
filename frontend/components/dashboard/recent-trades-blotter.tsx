"use client";

import { useMemo } from "react";
import { CircleAlert } from "lucide-react";
import { DataTable, type DataTableColumn } from "@/components/ui/data-table";
import { PnLCell } from "@/components/ui/pnl-cell";
import { Badge } from "@/components/ui/badge";
import { useLivePrice } from "@/hooks/use-live-prices";
import type { Position, Trade } from "@/lib/api";
import { cn, formatCurrency, formatPrice } from "@/lib/utils";

const MAX_ROWS = 20;
const STALE_OPEN_THRESHOLD_MS = 24 * 60 * 60 * 1000;

interface RecentTradesBlotterProps {
  trades: Trade[];
  /** Optional position snapshots used to backstop OPEN-row math. */
  positions?: Position[];
  className?: string;
}

/**
 * Dense blotter of the most recent trades. Status-aware columns so an
 * OPEN row shows the live mark-to-market while a CLOSED row shows the
 * realized net.
 */
export function RecentTradesBlotter({
  trades,
  positions = [],
  className,
}: RecentTradesBlotterProps) {
  const positionsBySymbol = useMemo(() => {
    const m = new Map<string, Position>();
    for (const p of positions) {
      if (p.is_open) m.set(p.symbol, p);
    }
    return m;
  }, [positions]);

  const rows = useMemo(() => trades.slice(0, MAX_ROWS), [trades]);

  const columns: DataTableColumn<Trade>[] = useMemo(
    () => [
      {
        key: "time",
        header: "Time",
        align: "left",
        width: "w-[96px]",
        cell: (t) => (
          <span className="text-muted-foreground">{formatTime(t.entry_at)}</span>
        ),
      },
      {
        key: "symbol",
        header: "Symbol",
        align: "left",
        width: "w-[88px]",
        cell: (t) => (
          <span className="font-medium">{t.symbol.replace("USDT", "/USDT")}</span>
        ),
      },
      {
        key: "side",
        header: "Side",
        align: "left",
        width: "w-[56px]",
        cell: (t) => <SideBadge side={t.side} />,
      },
      {
        key: "status",
        header: "Status",
        align: "left",
        width: "w-[96px]",
        cell: (t) => <StatusBadge trade={t} />,
      },
      {
        key: "mode",
        header: "Mode",
        align: "left",
        width: "w-[56px]",
        hideBelow: "md",
        cell: (t) => <ModeBadge isPaper={t.is_paper} />,
      },
      {
        key: "qty",
        header: "Qty",
        width: "w-[80px]",
        hideBelow: "lg",
        cell: (t) => t.quantity,
      },
      {
        key: "entry",
        header: "Entry",
        width: "w-[88px]",
        cell: (t) => formatPrice(parseFloat(t.entry_price || "0")),
      },
      {
        key: "now_or_exit",
        header: "Now / Exit",
        title: "Live mark for OPEN, exit price for CLOSED",
        width: "w-[96px]",
        cell: (t) => <NowOrExitCell trade={t} position={positionsBySymbol.get(t.symbol)} />,
      },
      {
        key: "pnl",
        header: "Net / Unrealized",
        title: "Realized net for CLOSED, live unrealized for OPEN",
        cell: (t) => <PnLOrUnrealizedCell trade={t} />,
      },
      {
        key: "conf",
        header: "Conf",
        title: "AI confidence at signal time",
        width: "w-[56px]",
        hideBelow: "lg",
        cell: (t) => (
          <span className="text-muted-foreground">
            {t.ai_confidence > 0 ? t.ai_confidence.toFixed(2) : "—"}
          </span>
        ),
      },
    ],
    [positionsBySymbol]
  );

  return (
    <DataTable
      className={className}
      columns={columns}
      rows={rows}
      rowKey={(t) => t.id}
      empty="No executed trades yet — signals fire on the configured cadence and trades materialize once confidence + risk gates pass."
    />
  );
}

function SideBadge({ side }: { side: Trade["side"] }) {
  return (
    <Badge
      variant={side === "BUY" ? "default" : "destructive"}
      className="px-1.5 py-0 text-micro"
    >
      {side}
    </Badge>
  );
}

function StatusBadge({ trade }: { trade: Trade }) {
  const status = trade.status;
  const isStaleOpen =
    status === "OPEN" &&
    Date.now() - Date.parse(trade.entry_at) > STALE_OPEN_THRESHOLD_MS;
  const variant: "outline" | "secondary" | "destructive" =
    status === "FAILED"
      ? "destructive"
      : status === "CLOSED"
      ? "secondary"
      : "outline";
  return (
    <span className="inline-flex items-center gap-1">
      <Badge variant={variant} className="px-1.5 py-0 text-micro">
        {status}
      </Badge>
      {isStaleOpen && (
        <span
          className="rounded border border-warning/40 px-1 text-micro font-semibold uppercase tracking-wider text-warning"
          title="OPEN > 24h"
        >
          stale
        </span>
      )}
    </span>
  );
}

function ModeBadge({ isPaper }: { isPaper: boolean }) {
  return (
    <span
      className={cn(
        "rounded border px-1 py-0.5 text-micro font-semibold uppercase tracking-wider",
        isPaper
          ? "border-warning/30 bg-warning/10 text-warning"
          : "border-loss/30 bg-loss/10 text-loss"
      )}
    >
      {isPaper ? "paper" : "live"}
    </span>
  );
}

function NowOrExitCell({
  trade,
  position,
}: {
  trade: Trade;
  position: Position | undefined;
}) {
  const live = useLivePrice(trade.symbol);
  if (trade.status === "CLOSED") {
    const exit = parseFloat(trade.exit_price || "0");
    if (exit <= 0) return <span className="text-muted-foreground">—</span>;
    return <>{formatPrice(exit)}</>;
  }
  if (trade.status === "OPEN") {
    const livePx = live?.price ?? 0;
    const dbPx = position ? parseFloat(position.current_price || "0") : 0;
    const px = livePx > 0 ? livePx : dbPx;
    if (px <= 0) return <span className="text-muted-foreground">—</span>;
    const isStale = live?.isStale ?? !live;
    return (
      <span
        className={cn("inline-flex items-center justify-end gap-1", isStale && "text-muted-foreground")}
        title={isStale ? "Live price stale (>30s old or WS disconnected)" : undefined}
      >
        {isStale && <CircleAlert className="h-3 w-3" />}
        {formatPrice(px)}
      </span>
    );
  }
  return <span className="text-muted-foreground">—</span>;
}

function PnLOrUnrealizedCell({ trade }: { trade: Trade }) {
  const live = useLivePrice(trade.symbol);
  if (trade.status === "CLOSED") {
    const net = parseFloat(trade.net_pnl || "0");
    const pct = parseFloat(trade.net_pnl_percent || "0");
    return <PnLCell value={net} pct={pct} />;
  }
  if (trade.status === "FAILED") {
    return (
      <span className="block text-right text-loss" title={trade.ai_reasoning}>
        execution_error
      </span>
    );
  }
  if (trade.status === "OPEN") {
    const px = live?.price ?? 0;
    if (px <= 0) {
      return <span className="block text-right text-muted-foreground/60">unreal —</span>;
    }
    const entry = parseFloat(trade.entry_price || "0");
    const qty = parseFloat(trade.quantity || "0");
    const entryFee = parseFloat(trade.entry_fee || "0");
    const entryCost = parseFloat(trade.entry_cost || "0");
    if (entry <= 0 || qty <= 0) {
      return <span className="block text-right text-muted-foreground/60">—</span>;
    }
    const grossPnL = (px - entry) * qty;
    const netPnL = grossPnL - entryFee;
    const denom = entryCost > 0 ? entryCost : entry * qty;
    const netPct = denom > 0 ? (netPnL / denom) * 100 : 0;
    return <PnLCell value={netPnL} pct={netPct} />;
  }
  return <span className="block text-right text-muted-foreground">—</span>;
}

function formatTime(iso: string): string {
  return new Date(iso).toLocaleString([], {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}
