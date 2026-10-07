"use client";

import { ReactNode } from "react";
import { cn } from "@/lib/utils";

export type MetricTone = "neutral" | "profit" | "loss" | "warning" | "info";

interface MetricBarProps {
  value: number;
  cap: number;
  tone: MetricTone;
}

export interface MetricProps {
  label: string;
  value: ReactNode;
  tone?: MetricTone;
  icon?: ReactNode;
  bar?: MetricBarProps;
  /** Hide label on narrow widths, keep icon+value */
  compact?: boolean;
  className?: string;
}

const TONE_TEXT: Record<MetricTone, string> = {
  neutral: "text-foreground",
  profit: "text-profit",
  loss: "text-loss",
  warning: "text-warning",
  info: "text-info",
};

const TONE_BAR: Record<MetricTone, string> = {
  neutral: "bg-muted-foreground/30",
  profit: "bg-profit",
  loss: "bg-loss",
  warning: "bg-warning",
  info: "bg-info",
};

export function Metric({
  label,
  value,
  tone = "neutral",
  icon,
  bar,
  compact,
  className,
}: MetricProps) {
  const barPct = bar ? Math.max(0, Math.min(100, (Math.abs(bar.value) / bar.cap) * 100)) : 0;
  return (
    <div className={cn("flex items-center gap-2", className)}>
      {icon && <span className="text-muted-foreground">{icon}</span>}
      <div className="flex flex-col leading-tight">
        <span
          className={cn(
            "text-micro font-medium uppercase tracking-wider text-muted-foreground",
            compact && "hidden lg:inline"
          )}
        >
          {label}
        </span>
        <div className="flex items-baseline gap-1.5">
          <span
            className={cn(
              "text-data font-semibold tabular-nums font-mono",
              TONE_TEXT[tone]
            )}
          >
            {value}
          </span>
          {bar && (
            <div className="h-1 w-12 overflow-hidden rounded-full bg-muted">
              <div
                className={cn("h-full rounded-full transition-all duration-500", TONE_BAR[bar.tone])}
                style={{ width: `${barPct}%` }}
              />
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
