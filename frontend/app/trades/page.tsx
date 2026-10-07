"use client";

import { useQuery } from "@tanstack/react-query";
import { Sidebar } from "@/components/layout/sidebar";
import { TopStatusBar } from "@/components/dashboard/top-status-bar";
import { TradesTable } from "@/components/trades/trades-table";
import { useLivePricesWS } from "@/hooks/use-live-prices";
import { api } from "@/lib/api";

export default function TradesPage() {
  // WS feeds the live-price store; throttled candle invalidations from
  // useWebSocket also keep the trades query reasonably fresh, and the
  // explicit 5s refetchInterval guarantees convergence even when WS is
  // down. Standard, not bandaid: a transient API failure must not flip
  // the table to "No trades yet" — react-query's `data` stays at the
  // last good snapshot while `isError` flips, and we surface that inline.
  const wsHealth = useLivePricesWS();
  const {
    data: trades,
    error,
    isLoading,
  } = useQuery({
    queryKey: ["trades"],
    queryFn: api.getTrades,
    refetchInterval: 5_000,
    refetchOnWindowFocus: true,
  });

  const errorMsg = error instanceof Error ? error.message : error ? String(error) : null;
  const tradesOrNull = isLoading ? null : trades ?? [];

  return (
    <div className="flex min-h-screen bg-background">
      <Sidebar />
      <main className="flex-1 pl-14">
        <TopStatusBar pageLabel="Trades" wsHealth={wsHealth} />
        <div className="space-y-3 p-5">
          {errorMsg && !trades && (
            <div className="rounded-md border border-loss/40 bg-loss/5 px-4 py-3 text-center text-sm text-loss">
              Failed to load trades: {errorMsg}
              <div className="mt-1 text-xs text-muted-foreground">Auto-retrying every 5 seconds…</div>
            </div>
          )}
          {errorMsg && trades && (
            <div className="rounded-md border border-warning/40 bg-warning/5 px-4 py-2 text-center text-xs text-warning">
              Showing last known data — latest refresh failed ({errorMsg}).
            </div>
          )}
          <TradesTable trades={tradesOrNull} wsConnected={wsHealth.connected} />
        </div>
      </main>
    </div>
  );
}
