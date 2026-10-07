"use client";

import React from "react";
import { CircleAlert } from "lucide-react";
import { DataTable, type DataTableColumn } from "@/components/ui/data-table";
import { PnLCell } from "@/components/ui/pnl-cell";
import { Sparkline } from "@/components/ui/sparkline";
import { Badge } from "@/components/ui/badge";
import { AVAILABLE_COINS, type Position } from "@/lib/api";
import { cn, formatCurrency, formatPrice } from "@/lib/utils";
import { useLivePrice } from "@/hooks/use-live-prices";

interface OpenPositionsBlotterProps {
  positions: Position[];
  /**
   * Optional close-price series per symbol. Mission Control passes the
   * existing analytics history slice; pages without a history query can
   * omit it and the spark column renders the muted placeholder.
   */
  sparkPointsBySymbol?: Record<string, number[]>;
  className?: string;
}

/**
 * Tabular blotter for open positions — replaces the ActiveTrades card
 * grid. One row per open position, dense numeric columns, sparkline
 * on the right. Backed by the reusable `DataTable` primitive so phase 5
 * can mount the same primitive for the closed-trades blotter.
 *
 * Live price + PnL derivation: same rule as the old ActiveTradeCard —
 * prefer the WS live-price store; fall back to the DB-resident value
 * only until the first tick lands. PnL is computed in-row from the live
 * price so the column doesn't lag the candle.
 */
export function OpenPositionsBlotter({
  positions,
  sparkPointsBySymbol,
  className,
}: OpenPositionsBlotterProps) {
  const openPositions = React.useMemo(
    () => positions.filter((p) => p.is_open),
    [positions]
  );

  const columns: DataTableColumn<Position>[] = React.useMemo(
    () => [
      {
        key: "symbol",
        header: "Symbol",
        align: "left",
        cell: (p) => <SymbolCell position={p} />,
      },
      {
        key: "qty",
        header: "Qty",
        title: "Position quantity",
        cell: (p) => p.quantity,
      },
      {
        key: "entry",
        header: "Entry",
        title: "Average entry price",
        cell: (p) => formatPrice(parseFloat(p.avg_entry_price || "0")),
      },
      {
        key: "current",
        header: "Current",
        title: "Live last price",
        cell: (p) => <CurrentPriceCell position={p} />,
      },
      {
        key: "sl",
        header: "SL",
        title: "Stop-loss (immutable since OPEN)",
        hideBelow: "md",
        cell: (p) => <PlanPriceCell value={p.stop_loss} tone="loss" />,
      },
      {
        key: "tp",
        header: "TP",
        title: "Take-profit (immutable since OPEN)",
        hideBelow: "md",
        cell: (p) => <PlanPriceCell value={p.take_profit} tone="profit" />,
      },
      {
        key: "pnl",
        header: "Unrealized",
        title: "Unrealized P&L from live price",
        cell: (p) => <LivePnLCell position={p} />,
      },
      {
        key: "spark",
        header: "",
        hideBelow: "lg",
        width: "w-[64px]",
        cell: (p) => (
          <Sparkline
            points={sparkPointsBySymbol?.[p.symbol] ?? []}
            tone="auto"
          />
        ),
      },
    ],
    [sparkPointsBySymbol]
  );

  return (
    <DataTable
      className={className}
      columns={columns}
      rows={openPositions}
      rowKey={(p) => p.id}
      empty="No open positions"
    />
  );
}

function SymbolCell({ position }: { position: Position }) {
  const coin = AVAILABLE_COINS.find((c) => c.symbol === position.symbol);
  const color = coin?.color ?? "#888";
  const label = coin?.label ?? position.symbol.replace("USDT", "");
  return (
    <div className="flex items-center gap-2">
      <span
        className="flex h-5 w-5 items-center justify-center rounded text-micro font-bold"
        style={{ backgroundColor: `${color}20`, color }}
      >
        {label.slice(0, 3)}
      </span>
      <span className="font-medium text-foreground">{label}/USDT</span>
      {position.exit_failure && (
        <span
          className="text-loss"
          title={`${position.exit_failure.exit_reason} exit failed to sell (attempt ${position.exit_failure.attempt}): ${position.exit_failure.reason}`}
          aria-label="Live exit failed"
        >
          <CircleAlert className="h-3.5 w-3.5" />
        </span>
      )}
      <Badge
        variant={position.side === "BUY" ? "default" : "destructive"}
        className="px-1 py-0 text-micro"
      >
        {position.side}
      </Badge>
    </div>
  );
}

function CurrentPriceCell({ position }: { position: Position }) {
  const live = useLivePrice(position.symbol);
  const dbPrice = parseFloat(position.current_price || "0");
  const price = live?.price ?? dbPrice;
  const hasPrice = price > 0;
  const isStale = live?.isStale ?? !hasPrice;

  if (!hasPrice) {
    return <span className="text-muted-foreground">—</span>;
  }
  return (
    <span
      className={cn("inline-flex items-center justify-end gap-1", isStale && "text-muted-foreground")}
      title={isStale ? "Live price stale (>30s old or WS disconnected)" : undefined}
    >
      {isStale && <CircleAlert className="h-3 w-3" />}
      {formatPrice(price)}
    </span>
  );
}

function PlanPriceCell({
  value,
  tone,
}: {
  value: string | null | undefined;
  tone: "profit" | "loss";
}) {
  if (value == null || value === "") {
    return <span className="text-muted-foreground/50">—</span>;
  }
  const n = parseFloat(value);
  if (!Number.isFinite(n) || n <= 0) {
    return <span className="text-muted-foreground/50">—</span>;
  }
  return (
    <span className={tone === "profit" ? "text-profit/80" : "text-loss/80"}>
      {formatPrice(n)}
    </span>
  );
}

function LivePnLCell({ position }: { position: Position }) {
  const live = useLivePrice(position.symbol);
  const entry = parseFloat(position.avg_entry_price || "0");
  const qty = parseFloat(position.quantity || "0");
  const dbPnl = parseFloat(position.unrealized_pnl || "0");
  const dbPrice = parseFloat(position.current_price || "0");
  const entryFee = parseFloat(position.open_entry_fee || "0");
  const price = live?.price ?? dbPrice;
  const hasPrice = price > 0;

  // Net of the entry fee already paid — same figure the API reports.
  const pnl =
    hasPrice && entry > 0 && qty > 0 ? (price - entry) * qty - entryFee : dbPnl;
  const pct = hasPrice && entry > 0 ? ((price - entry) / entry) * 100 : null;

  return <PnLCell value={pnl} pct={pct} muteZero={!hasPrice && dbPnl === 0} />;
}
