"use client";

import { cn, formatCurrency } from "@/lib/utils";

interface PnLCellProps {
  /** Absolute PnL in USD. */
  value: number;
  /** Optional percent component rendered beneath the value. */
  pct?: number | null;
  /** Render zero as muted "—" instead of "$0.00". */
  muteZero?: boolean;
  className?: string;
}

/**
 * Right-aligned PnL value with semantic tone. Profit/loss color decided
 * by the sign of `value`. Zero falls back to neutral foreground (or "—"
 * when `muteZero` is set — useful in the blotter where a position with
 * no live price yet should not flash green).
 */
export function PnLCell({ value, pct, muteZero, className }: PnLCellProps) {
  const zero = value === 0;
  const tone = zero ? "neutral" : value > 0 ? "profit" : "loss";
  const sign = value > 0 ? "+" : "";

  if (zero && muteZero) {
    return (
      <div className={cn("text-right text-muted-foreground", className)}>—</div>
    );
  }

  return (
    <div className={cn("text-right font-mono tabular-nums leading-tight", className)}>
      <div
        className={cn(
          "font-semibold",
          tone === "profit" && "text-profit",
          tone === "loss" && "text-loss",
          tone === "neutral" && "text-foreground"
        )}
      >
        {sign}
        {formatCurrency(value)}
      </div>
      {pct != null && Number.isFinite(pct) && (
        <div
          className={cn(
            "text-micro",
            tone === "profit" && "text-profit/70",
            tone === "loss" && "text-loss/70",
            tone === "neutral" && "text-muted-foreground"
          )}
        >
          {pct > 0 ? "+" : ""}
          {pct.toFixed(2)}%
        </div>
      )}
    </div>
  );
}
