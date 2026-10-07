"use client";

import { cn } from "@/lib/utils";

interface ConfidenceCellProps {
  /** Confidence in [0, 1]. */
  value: number;
  /** Hide the inline percentage label. */
  hideLabel?: boolean;
  className?: string;
}

function toneOf(value: number): "profit" | "warning" | "loss" {
  if (value > 0.7) return "profit";
  if (value >= 0.4) return "warning";
  return "loss";
}

const BAR: Record<ReturnType<typeof toneOf>, string> = {
  profit: "bg-profit",
  warning: "bg-warning",
  loss: "bg-loss",
};
const TEXT: Record<ReturnType<typeof toneOf>, string> = {
  profit: "text-profit",
  warning: "text-warning",
  loss: "text-loss",
};

/**
 * Compact confidence visualization for blotter cells. Tone bands are
 * fixed to the same thresholds the executor uses for the BUY gate
 * (≥0.6) so a quick scan tells the operator whether the signal would
 * pass risk. The 40-pixel bar plus the numeric label are designed to
 * read at body-text size without forcing the row taller.
 */
export function ConfidenceCell({ value, hideLabel, className }: ConfidenceCellProps) {
  const clamped = Math.max(0, Math.min(1, value));
  const tone = toneOf(clamped);
  const pct = (clamped * 100).toFixed(0);

  return (
    <div className={cn("inline-flex items-center justify-end gap-1.5", className)}>
      <div className="h-1.5 w-10 overflow-hidden rounded-full bg-muted">
        <div
          className={cn("h-full rounded-full transition-all duration-500", BAR[tone])}
          style={{ width: `${clamped * 100}%` }}
        />
      </div>
      {!hideLabel && (
        <span className={cn("w-7 text-right text-micro font-mono font-semibold tabular-nums", TEXT[tone])}>
          {pct}
        </span>
      )}
    </div>
  );
}
