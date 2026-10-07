"use client";

import { useMemo, useState } from "react";
import { CircleAlert } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import {
  DataTable,
  type DataTableColumn,
  type SortState,
} from "@/components/ui/data-table";
import { PnLCell } from "@/components/ui/pnl-cell";
import { useLivePrices } from "@/hooks/use-live-prices";
import type { Trade } from "@/lib/api";
import { cn, formatCurrency, formatPrice } from "@/lib/utils";

const STALE_AFTER_MS = 30_000;

type SortKey =
  | "entry_at"
  | "net_pnl"
  | "pnl"
  | "entry_cost"
  | "fee"
  | "unrealized_pnl";

interface TradesTableProps {
  /** null = first fetch in flight; Trade[] = ready, possibly empty. */
  trades: Trade[] | null;
  /** When false, every live-quote-derived cell renders in stale style. */
  wsConnected?: boolean;
}

interface UnrealizedView {
  netPnL: number;
  netPct: number;
  isStale: boolean;
}

function computeUnrealized(
  trade: Trade,
  price: number | undefined,
  quoteAt: number | undefined,
  wsConnected: boolean
): UnrealizedView | null {
  if (trade.status !== "OPEN") return null;
  if (price === undefined || !Number.isFinite(price)) return null;
  const entry = parseFloat(trade.entry_price || "0");
  const qty = parseFloat(trade.quantity || "0");
  const entryFee = parseFloat(trade.entry_fee || "0");
  const entryCost = parseFloat(trade.entry_cost || "0");
  if (entry <= 0 || qty <= 0) return null;
  const grossPnL = (price - entry) * qty;
  const netPnL = grossPnL - entryFee;
  const denom = entryCost > 0 ? entryCost : entry * qty;
  const netPct = denom > 0 ? (netPnL / denom) * 100 : 0;
  const isStale =
    !wsConnected || quoteAt === undefined || Date.now() - quoteAt > STALE_AFTER_MS;
  return { netPnL, netPct, isStale };
}

export function TradesTable({ trades, wsConnected = true }: TradesTableProps) {
  const [sort, setSort] = useState<SortState>({ key: "net_pnl", dir: "desc" });
  const livePrices = useLivePrices();

  const sorted = useMemo(() => {
    if (!trades) return [];
    const arr = [...trades];
    arr.sort((a, b) => {
      const cmp = compareTrades(a, b, sort.key as SortKey, livePrices.get);
      return sort.dir === "asc" ? cmp : -cmp;
    });
    return arr;
    // livePrices.version intentionally in deps so unrealized sort
    // recomputes as quotes tick.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [trades, sort, livePrices.version]);

  const columns: DataTableColumn<Trade>[] = useMemo(
    () => [
      {
        key: "entry_at",
        header: "Date",
        align: "left",
        sortable: true,
        cell: (t) => (
          <span className="text-muted-foreground whitespace-nowrap">
            {new Date(t.entry_at).toLocaleString([], {
              month: "short",
              day: "numeric",
              hour: "2-digit",
              minute: "2-digit",
              hour12: false,
            })}
          </span>
        ),
      },
      {
        key: "symbol",
        header: "Symbol",
        align: "left",
        cell: (t) => (
          <span className="font-medium">{t.symbol.replace("USDT", "/USDT")}</span>
        ),
      },
      {
        key: "side",
        header: "Side",
        align: "left",
        cell: (t) => (
          <Badge variant={t.side === "BUY" ? "default" : "destructive"} className="text-micro">
            {t.side}
          </Badge>
        ),
      },
      {
        key: "status",
        header: "Status",
        align: "left",
        cell: (t) => (
          <Badge variant={statusVariant(t.status)} className="text-micro">
            {t.status}
          </Badge>
        ),
      },
      {
        key: "quantity",
        header: "Qty",
        cell: (t) => t.quantity,
      },
      {
        key: "entry_price",
        header: "Entry",
        cell: (t) => formatPrice(parseFloat(t.entry_price || "0")),
      },
      {
        key: "exit_price",
        header: "Exit",
        cell: (t) => {
          const px = parseFloat(t.exit_price || "0");
          return px > 0 ? formatPrice(px) : "—";
        },
      },
      {
        key: "entry_cost",
        header: "Entry Cost",
        sortable: true,
        cell: (t) => formatCurrency(parseFloat(t.entry_cost || "0")),
      },
      {
        key: "exit_cost",
        header: "Exit Cost",
        cell: (t) => {
          const px = parseFloat(t.exit_cost || "0");
          return px > 0 ? formatCurrency(px) : "—";
        },
      },
      {
        key: "fee",
        header: "Fee",
        sortable: true,
        cell: (t) => {
          const fee = parseFloat(t.entry_fee || "0") + parseFloat(t.exit_fee || "0");
          return (
            <span className="text-warning">
              {fee > 0 ? formatPrice(fee) : "—"}
            </span>
          );
        },
      },
      {
        key: "pnl",
        header: "Gross PnL",
        sortable: true,
        cell: (t) => {
          const gross = parseFloat(t.pnl || "0");
          if (t.status !== "CLOSED") return <span className="text-muted-foreground">—</span>;
          return <PnLCell value={gross} />;
        },
      },
      {
        key: "net_pnl",
        header: "Net PnL",
        sortable: true,
        cell: (t) => {
          if (t.status !== "CLOSED") return <span className="text-muted-foreground">—</span>;
          const net = parseFloat(t.net_pnl || "0");
          const pct = parseFloat(t.net_pnl_percent || "0");
          return <PnLCell value={net} pct={pct} />;
        },
      },
      {
        key: "unrealized_pnl",
        header: "Unrealized",
        sortable: true,
        cell: (t) => {
          if (t.status !== "OPEN") return <span className="text-muted-foreground">—</span>;
          const quote = livePrices.get(t.symbol);
          const view = computeUnrealized(t, quote?.price, quote?.at, wsConnected);
          if (!view) {
            return (
              <span
                className="inline-flex items-center justify-end gap-1 text-muted-foreground/60"
                title="No live price yet"
              >
                <CircleAlert className="h-3 w-3 opacity-60" />—
              </span>
            );
          }
          return (
            <span
              className={cn(
                view.netPnL >= 0 ? "text-profit" : "text-loss",
                view.isStale && "opacity-50"
              )}
              title={view.isStale ? "Live price stale (>30s)" : undefined}
            >
              <span className="inline-flex items-center justify-end gap-1 font-semibold">
                {view.isStale && <CircleAlert className="h-3 w-3" />}
                {formatSigned(view.netPnL)}{" "}
                <span className="text-micro opacity-80">
                  ({view.netPct >= 0 ? "+" : ""}
                  {view.netPct.toFixed(2)}%)
                </span>
              </span>
            </span>
          );
        },
      },
    ],
    [livePrices, wsConnected]
  );

  if (trades === null) {
    return (
      <DataTable
        columns={columns}
        rows={[]}
        rowKey={() => 0}
        empty="Loading…"
      />
    );
  }

  return (
    <DataTable
      columns={columns}
      rows={sorted}
      rowKey={(t) => t.id}
      sort={sort}
      onSortChange={setSort}
      empty="No trades yet."
    />
  );
}

function statusVariant(
  status: Trade["status"]
): "default" | "destructive" | "outline" | "secondary" {
  switch (status) {
    case "OPEN":
      return "outline";
    case "CLOSED":
      return "secondary";
    case "FAILED":
      return "destructive";
    default:
      return "outline";
  }
}

function compareTrades(
  a: Trade,
  b: Trade,
  key: SortKey,
  getQuote: (sym: string) => { price: number } | undefined
): number {
  switch (key) {
    case "entry_at":
      return Date.parse(a.entry_at) - Date.parse(b.entry_at);
    case "net_pnl":
      return parseFloat(a.net_pnl || "0") - parseFloat(b.net_pnl || "0");
    case "pnl":
      return parseFloat(a.pnl || "0") - parseFloat(b.pnl || "0");
    case "entry_cost":
      return parseFloat(a.entry_cost || "0") - parseFloat(b.entry_cost || "0");
    case "fee":
      return (
        parseFloat(a.entry_fee || "0") +
        parseFloat(a.exit_fee || "0") -
        (parseFloat(b.entry_fee || "0") + parseFloat(b.exit_fee || "0"))
      );
    case "unrealized_pnl": {
      // CLOSED rows tie-break to the bottom of the sort regardless of
      // direction — they have no unrealized concept.
      const aOpen = a.status === "OPEN";
      const bOpen = b.status === "OPEN";
      if (!aOpen && !bOpen) return 0;
      if (!aOpen) return 1;
      if (!bOpen) return -1;
      const av = unrealizedFor(a, getQuote(a.symbol)?.price);
      const bv = unrealizedFor(b, getQuote(b.symbol)?.price);
      return av - bv;
    }
  }
}

function unrealizedFor(t: Trade, price: number | undefined): number {
  if (price === undefined || !Number.isFinite(price)) return 0;
  const entry = parseFloat(t.entry_price || "0");
  const qty = parseFloat(t.quantity || "0");
  const entryFee = parseFloat(t.entry_fee || "0");
  if (entry <= 0 || qty <= 0) return 0;
  return (price - entry) * qty - entryFee;
}

function formatSigned(n: number): string {
  return n >= 0 ? `+${formatCurrency(n)}` : formatCurrency(n);
}
