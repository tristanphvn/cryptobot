"use client";

import { cn } from "@/lib/utils";

export type HealthStatus = "ok" | "warn" | "down" | "idle";

interface HealthDotProps {
  status: HealthStatus;
  className?: string;
}

export function HealthDot({ status, className }: HealthDotProps) {
  return (
    <div
      className={cn(
        "h-1.5 w-1.5 rounded-full transition-colors",
        status === "ok" && "bg-profit",
        status === "warn" && "bg-warning",
        status === "down" && "bg-loss dot-pulse",
        status === "idle" && "bg-muted-foreground/40",
        className
      )}
    />
  );
}
