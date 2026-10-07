"use client";

import { useEffect, useMemo, useState } from "react";
import {
  ResponsiveContainer,
  LineChart,
  Line,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
} from "recharts";
import { Sidebar } from "@/components/layout/sidebar";
import { TopStatusBar } from "@/components/dashboard/top-status-bar";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { cn, formatCurrency, formatPrice } from "@/lib/utils";
import { api, type PortfolioResponse, type Position } from "@/lib/api";
import { Wallet, TrendingUp, TrendingDown, Receipt, Activity } from "lucide-react";

type Tab = "closed" | "open";

export default function PortfolioPage() {
  const [data, setData] = useState<PortfolioResponse | null>(null);
  const [positions, setPositions] = useState<Position[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [tab, setTab] = useState<Tab>("closed");

  // Standard, not bandaid: separate `error` from `null data`. A
  // transient 5xx during the 10s poll must not wipe the dashboard back
  // to the loading skeleton — we keep the last good snapshot and surface
  // the failure as a small banner. Initial-load failures (still null)
  // show a full error card with retry hint.
  useEffect(() => {
    const load = async () => {
      try {
        const [p, pos] = await Promise.all([api.getPortfolio(), api.getPositions()]);
        setData(p);
        setPositions(pos);
        setError(null);
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    };
    load();
    const t = setInterval(load, 10_000);
    return () => clearInterval(t);
  }, []);

  return (
    <div className="flex min-h-screen bg-background">
      <Sidebar />
      <main className="flex-1 pl-14">
        <TopStatusBar pageLabel="Portfolio" />
        <div className="space-y-5 p-5">
          {/* Initial-load failure: nothing usable to show, full card. */}
          {error && data === null && (
            <Card className="border-loss/40">
              <CardContent className="py-8 text-center">
                <p className="text-sm font-medium text-loss">Failed to load portfolio</p>
                <p className="mt-1 text-xs text-muted-foreground">{error}</p>
                <p className="mt-2 text-xs text-muted-foreground">Auto-retrying every 10 seconds…</p>
              </CardContent>
            </Card>
          )}

          {/* Stale-data banner: we have a snapshot but the latest poll failed. */}
          {error && data !== null && (
            <Card className="border-warning/40 bg-warning/5">
              <CardContent className="py-2 text-center text-xs text-warning">
                Showing last known data — latest refresh failed ({error}). Auto-retrying…
              </CardContent>
            </Card>
          )}

          <AggregateCards data={data} />
          <CumulativeChart data={data} />

          {/* Tabs */}
          <div className="flex items-center gap-1 border-b border-border">
            <TabButton active={tab === "closed"} onClick={() => setTab("closed")}>
              Closed Trades (per-symbol)
            </TabButton>
            <TabButton active={tab === "open"} onClick={() => setTab("open")}>
              Open Positions
              {positions && positions.length > 0 && (
                <Badge variant="outline" className="ml-2 text-micro">
                  {positions.length}
                </Badge>
              )}
            </TabButton>
          </div>

          {tab === "closed" ? <BySymbolTable data={data} /> : <OpenPositionsTable positions={positions} />}
        </div>
      </main>
    </div>
  );
}

function AggregateCards({ data }: { data: PortfolioResponse | null }) {
  const netPnL = parseFloat(data?.aggregate.total_net_pnl || "0");
  const fees = parseFloat(data?.aggregate.total_fees || "0");
  const invested = parseFloat(data?.aggregate.total_invested || "0");
  const openPos = data?.aggregate.open_positions ?? 0;

  return (
    <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      <AggCard
        title="Net P&L"
        value={data ? formatCurrency(netPnL) : "—"}
        icon={netPnL >= 0 ? TrendingUp : TrendingDown}
        color={netPnL >= 0 ? "text-profit" : "text-loss"}
        tooltip="Realised net of fees across all CLOSED trades."
      />
      <AggCard
        title="Fees Paid"
        value={data ? formatCurrency(fees) : "—"}
        icon={Receipt}
        color="text-warning"
        tooltip="Total Binance commissions paid (entry + exit legs)."
      />
      <AggCard
        title="Total Invested"
        value={data ? formatCurrency(invested) : "—"}
        icon={Wallet}
        color="text-info"
        tooltip="Cumulative entry cost across the symbol's lifetime — not current outstanding exposure."
      />
      <AggCard
        title="Open Positions"
        value={openPos.toString()}
        icon={Activity}
        color="text-info"
        tooltip="Positions currently held, awaiting exit."
      />
    </div>
  );
}

function AggCard({
  title,
  value,
  icon: Icon,
  color,
  tooltip,
}: {
  title: string;
  value: string;
  icon: React.ComponentType<{ className?: string }>;
  color: string;
  tooltip: string;
}) {
  return (
    <Card title={tooltip}>
      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
        <CardTitle className="text-data font-medium text-muted-foreground">{title}</CardTitle>
        <Icon className={cn("h-4 w-4", color)} />
      </CardHeader>
      <CardContent>
        <div className={cn("text-2xl font-bold", color)}>{value}</div>
      </CardContent>
    </Card>
  );
}

function CumulativeChart({ data }: { data: PortfolioResponse | null }) {
  // Cumulative running sum computed render-side — daily_net_pnl is
  // already date-bucketed by the backend (last 90 days).
  const series = useMemo(() => {
    if (!data?.daily_net_pnl?.length) return [];
    let running = 0;
    return data.daily_net_pnl.map((p) => {
      running += parseFloat(p.net_pnl || "0");
      return { date: p.date, cumulative: running };
    });
  }, [data]);

  if (!data) {
    return (
      <Card>
        <CardContent className="flex h-[300px] items-center justify-center">
          <p className="text-sm text-muted-foreground">Loading…</p>
        </CardContent>
      </Card>
    );
  }

  if (series.length === 0) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Cumulative Net PnL (90 days)</CardTitle>
        </CardHeader>
        <CardContent className="flex h-[260px] items-center justify-center">
          <div className="text-center">
            <Wallet className="mx-auto mb-2 h-8 w-8 text-muted-foreground/40" />
            <p className="text-sm text-muted-foreground">No closed trades yet.</p>
            <p className="mt-1 text-xs text-muted-foreground/70">
              Chart populates once trades close.
            </p>
          </div>
        </CardContent>
      </Card>
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Cumulative Net PnL (90 days)</CardTitle>
      </CardHeader>
      <CardContent>
        <div className="h-[280px] w-full">
          <ResponsiveContainer width="100%" height="100%">
            <LineChart data={series}>
              <CartesianGrid strokeDasharray="3 3" className="stroke-border/50" />
              <XAxis dataKey="date" tick={{ fontSize: 11 }} stroke="currentColor" className="text-muted-foreground" />
              <YAxis tick={{ fontSize: 11 }} stroke="currentColor" className="text-muted-foreground" />
              <Tooltip
                contentStyle={{
                  background: "hsl(var(--card))",
                  border: "1px solid hsl(var(--border))",
                  borderRadius: "8px",
                  fontSize: "12px",
                }}
                formatter={(value: number) => [formatCurrency(value), "Cumulative"]}
              />
              <Line type="monotone" dataKey="cumulative" stroke="hsl(var(--primary))" strokeWidth={2} dot={false} />
            </LineChart>
          </ResponsiveContainer>
        </div>
      </CardContent>
    </Card>
  );
}

function BySymbolTable({ data }: { data: PortfolioResponse | null }) {
  if (!data) {
    return <Card><CardContent className="py-8 text-center text-sm text-muted-foreground">Loading…</CardContent></Card>;
  }
  if (!data.by_symbol || data.by_symbol.length === 0) {
    return (
      <Card>
        <CardContent className="flex flex-col items-center justify-center py-12">
          <Wallet className="mb-2 h-8 w-8 text-muted-foreground/40" />
          <p className="text-sm text-muted-foreground">No trades yet.</p>
          <p className="mt-1 text-xs text-muted-foreground/70">
            Per-symbol breakdown will appear once trades execute.
          </p>
        </CardContent>
      </Card>
    );
  }
  const sorted = [...data.by_symbol].sort(
    (a, b) => parseFloat(b.realized_pnl) - parseFloat(a.realized_pnl),
  );

  return (
    <Card>
      <CardContent className="overflow-x-auto p-0">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-border text-micro uppercase tracking-wider text-muted-foreground">
              <th className="px-3 py-2 text-left">Symbol</th>
              <th className="px-3 py-2 text-right">Closed</th>
              <th className="px-3 py-2 text-right">Invested</th>
              <th className="px-3 py-2 text-right">Realized Net</th>
              <th className="px-3 py-2 text-right">Unrealized</th>
              <th className="px-3 py-2 text-right">Fees</th>
              <th className="px-3 py-2 text-right">Open Qty</th>
              <th className="px-3 py-2 text-right">Current Price</th>
            </tr>
          </thead>
          <tbody>
            {sorted.map((s) => {
              const realized = parseFloat(s.realized_pnl);
              const unrealized = parseFloat(s.unrealized_pnl);
              return (
                <tr key={s.symbol} className="border-b border-border/40 hover:bg-muted/40">
                  <td className="px-3 py-2 font-medium">{s.symbol.replace("USDT", "/USDT")}</td>
                  <td className="px-3 py-2 text-right font-mono tabular-nums">{s.closed_trades}</td>
                  <td className="px-3 py-2 text-right font-mono tabular-nums">{formatCurrency(parseFloat(s.total_invested))}</td>
                  <td className={cn(
                    "px-3 py-2 text-right font-mono tabular-nums font-semibold",
                    realized >= 0 ? "text-profit" : "text-loss",
                  )}>
                    {realized >= 0 ? "+" : ""}
                    {formatCurrency(realized)}
                  </td>
                  <td className={cn(
                    "px-3 py-2 text-right font-mono tabular-nums",
                    unrealized === 0 ? "text-muted-foreground" : unrealized > 0 ? "text-profit" : "text-loss",
                  )}>
                    {unrealized === 0 ? "—" : (unrealized > 0 ? "+" : "") + formatCurrency(unrealized)}
                  </td>
                  <td className="px-3 py-2 text-right font-mono tabular-nums text-warning">
                    {formatCurrency(parseFloat(s.total_fees))}
                  </td>
                  <td className="px-3 py-2 text-right font-mono tabular-nums">
                    {parseFloat(s.open_qty) > 0 ? s.open_qty : "—"}
                  </td>
                  <td className="px-3 py-2 text-right font-mono tabular-nums">
                    {parseFloat(s.current_price) > 0 ? formatPrice(parseFloat(s.current_price)) : "—"}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </CardContent>
    </Card>
  );
}

function OpenPositionsTable({ positions }: { positions: Position[] | null }) {
  if (positions === null) {
    return <Card><CardContent className="py-8 text-center text-sm text-muted-foreground">Loading…</CardContent></Card>;
  }
  if (positions.length === 0) {
    return (
      <Card>
        <CardContent className="flex flex-col items-center justify-center py-12">
          <Activity className="mb-2 h-8 w-8 text-muted-foreground/40" />
          <p className="text-sm text-muted-foreground">No open positions.</p>
        </CardContent>
      </Card>
    );
  }
  return (
    <Card>
      <CardContent className="overflow-x-auto p-0">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-border text-micro uppercase tracking-wider text-muted-foreground">
              <th className="px-3 py-2 text-left">Symbol</th>
              <th className="px-3 py-2 text-left">Side</th>
              <th className="px-3 py-2 text-right">Quantity</th>
              <th className="px-3 py-2 text-right">Avg Entry</th>
              <th className="px-3 py-2 text-right">Current</th>
              <th className="px-3 py-2 text-right">Unrealized PnL</th>
            </tr>
          </thead>
          <tbody>
            {positions.map((p) => {
              const unrealized = parseFloat(p.unrealized_pnl || "0");
              return (
                <tr key={p.id} className="border-b border-border/40 hover:bg-muted/40">
                  <td className="px-3 py-2 font-medium">{p.symbol.replace("USDT", "/USDT")}</td>
                  <td className="px-3 py-2">
                    <Badge variant={p.side === "BUY" ? "default" : "destructive"} className="text-micro">
                      {p.side}
                    </Badge>
                  </td>
                  <td className="px-3 py-2 text-right font-mono tabular-nums">{p.quantity}</td>
                  <td className="px-3 py-2 text-right font-mono tabular-nums">{formatPrice(parseFloat(p.avg_entry_price))}</td>
                  <td className="px-3 py-2 text-right font-mono tabular-nums">
                    {parseFloat(p.current_price) > 0 ? formatPrice(parseFloat(p.current_price)) : "—"}
                  </td>
                  <td className={cn(
                    "px-3 py-2 text-right font-mono tabular-nums font-semibold",
                    unrealized === 0 ? "text-muted-foreground" : unrealized > 0 ? "text-profit" : "text-loss",
                  )}>
                    {unrealized === 0 ? "—" : (unrealized > 0 ? "+" : "") + formatCurrency(unrealized)}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </CardContent>
    </Card>
  );
}

function TabButton({
  active,
  onClick,
  children,
}: {
  active: boolean;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      onClick={onClick}
      className={cn(
        "px-4 py-2 text-sm font-medium transition-colors",
        active
          ? "border-b-2 border-primary text-foreground"
          : "text-muted-foreground hover:text-foreground",
      )}
    >
      {children}
    </button>
  );
}
