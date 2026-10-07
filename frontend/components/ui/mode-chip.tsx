"use client";

import { cn } from "@/lib/utils";

export type ExecutionMode = "dry-run" | "paper" | "live";

interface ModeChipProps {
  mode: ExecutionMode;
  size?: "sm" | "md";
  className?: string;
}

const LABEL: Record<ExecutionMode, string> = {
  "dry-run": "DRY",
  paper: "PAPER",
  live: "LIVE",
};

export function ModeChip({ mode, size = "md", className }: ModeChipProps) {
  const sizeCls =
    size === "sm"
      ? "h-5 px-1.5 text-micro"
      : "h-6 px-2 text-micro";

  return (
    <span
      className={cn(
        "inline-flex items-center justify-center rounded-md border font-semibold uppercase tracking-wider tabular-nums font-mono",
        sizeCls,
        mode === "dry-run" &&
          "border-border bg-transparent text-muted-foreground",
        mode === "paper" &&
          "border-warning/40 bg-warning/15 text-warning",
        mode === "live" &&
          "border-loss/50 bg-loss/20 text-loss dot-pulse",
        className
      )}
      title={`Execution mode: ${mode}`}
    >
      {LABEL[mode]}
    </span>
  );
}
