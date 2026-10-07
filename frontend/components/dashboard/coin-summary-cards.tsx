"use client";

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { TrendingUp, TrendingDown } from "lucide-react";
import { AVAILABLE_COINS, type SymbolSummary } from "@/lib/api";
import { cn, formatCurrency, formatPrice } from "@/lib/utils";

interface CoinSummaryCardsProps {
  summaries: SymbolSummary[];
  isLoading: boolean;
}

export function CoinSummaryCards({
  summaries,
  isLoading,
}: CoinSummaryCardsProps) {
  if (isLoading) {
    return (
      <div className="grid gap-3 md:grid-cols-2 lg:grid-cols-3">
        {[1, 2, 3].map((i) => (
          <Card key={i}>
            <CardHeader className="pb-2">
              <Skeleton className="h-4 w-24" />
            </CardHeader>
            <CardContent className="space-y-2">
              <Skeleton className="h-7 w-32" />
              <Skeleton className="h-4 w-20" />
            </CardContent>
          </Card>
        ))}
      </div>
    );
  }

  if (summaries.length === 0) {
    return null;
  }

  return (
    <div className="grid gap-3 md:grid-cols-2 lg:grid-cols-3">
      {summaries.map((summary) => {
        const coin = AVAILABLE_COINS.find((c) => c.symbol === summary.symbol);
        const pnlPct = parseFloat(summary.pnl_percent || "0");
        const pnlAbs = parseFloat(summary.pnl_absolute || "0");
        const currentPrice = parseFloat(summary.current_price || "0");
        const isPositive = pnlPct >= 0;

        return (
          <Card
            key={summary.symbol}
            className="transition-all duration-200 hover:border-border/80"
          >
            <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
              <CardTitle className="flex items-center gap-2 text-data font-medium">
                <div
                  className="h-2.5 w-2.5 rounded-full"
                  style={{ backgroundColor: coin?.color || "hsl(var(--muted-foreground))" }}
                />
                {coin?.label || summary.symbol}
              </CardTitle>
              <div className={cn(
                "flex h-6 w-6 items-center justify-center rounded-md",
                isPositive ? "bg-profit/10" : "bg-loss/10"
              )}>
                {isPositive ? (
                  <TrendingUp className="h-3.5 w-3.5 text-profit" />
                ) : (
                  <TrendingDown className="h-3.5 w-3.5 text-loss" />
                )}
              </div>
            </CardHeader>
            <CardContent>
              <div className="text-hero font-bold font-mono tabular-nums">
                {formatPrice(currentPrice)}
              </div>
              <div className="mt-1.5 flex items-center gap-2">
                <span
                  className={cn(
                    "text-data font-semibold font-mono tabular-nums",
                    isPositive ? "text-profit" : "text-loss"
                  )}
                >
                  {isPositive ? "+" : ""}
                  {pnlPct.toFixed(2)}%
                </span>
                <span className="text-micro text-muted-foreground font-mono tabular-nums">
                  ({isPositive ? "+" : ""}
                  {formatPrice(pnlAbs)})
                </span>
              </div>
              <div className="mt-2.5 flex justify-between rounded-md bg-muted/50 px-2 py-1 text-micro text-muted-foreground font-mono tabular-nums">
                <span className="font-mono">H <span className="text-foreground/70">{formatPrice(parseFloat(summary.high || "0"))}</span></span>
                <span className="font-mono">L <span className="text-foreground/70">{formatPrice(parseFloat(summary.low || "0"))}</span></span>
              </div>
            </CardContent>
          </Card>
        );
      })}
    </div>
  );
}
