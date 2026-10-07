"use client";

import { Card, CardContent } from "@/components/ui/card";
import { Brain, AlertCircle, CheckCircle2 } from "lucide-react";
import { cn } from "@/lib/utils";
import { AIHealthSnapshot } from "@/lib/api";

interface AIProviderStripProps {
  health: AIHealthSnapshot | undefined;
  isLoading: boolean;
  isError: boolean;
  latestInferenceMs?: number | null;
  latestEvalAt?: string | null;
}

export function AIProviderStrip({
  health,
  isLoading,
  isError,
  latestInferenceMs,
  latestEvalAt,
}: AIProviderStripProps) {
  const status = resolveStatus(health, isLoading, isError);
  return (
    <Card>
      <CardContent className="flex flex-wrap items-center gap-4 py-3">
        <div className="flex items-center gap-2">
          <div className="flex h-7 w-7 items-center justify-center rounded-lg bg-info/15">
            <Brain className="h-4 w-4 text-info" />
          </div>
          <div className="leading-tight">
            <div className="text-data font-semibold">
              {health?.provider ?? "AI provider"}
            </div>
            <div className="text-micro text-muted-foreground">
              {status.subtitle}
            </div>
          </div>
        </div>

        <div className="ml-auto flex flex-wrap items-center gap-x-5 gap-y-1 text-micro">
          <Stat
            label="status"
            value={
              <span className="inline-flex items-center gap-1">
                <StatusDot kind={status.kind} />
                <span className={cn("font-mono font-semibold", status.color)}>
                  {status.label}
                </span>
              </span>
            }
          />
          {health?.breaker_state && (
            <Stat label="breaker" value={<span className="font-mono">{health.breaker_state}</span>} />
          )}
          <Stat
            label="last check"
            value={
              <span className="font-mono text-muted-foreground">
                {health?.last_check_at ? formatAgo(health.last_check_at) : "—"}
              </span>
            }
          />
          <Stat
            label="last eval"
            value={
              <span className="font-mono text-muted-foreground">
                {latestEvalAt ? formatAgo(latestEvalAt) : "—"}
              </span>
            }
          />
          <Stat
            label="latency"
            value={
              <span className="font-mono">
                {latestInferenceMs != null
                  ? `${(latestInferenceMs / 1000).toFixed(1)}s`
                  : "—"}
              </span>
            }
          />
        </div>

        {health?.last_error && !health.healthy && (
          <div className="flex w-full items-center gap-1.5 text-micro text-loss">
            <AlertCircle className="h-3 w-3" />
            <span className="font-mono break-all">{health.last_error}</span>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

type StatusKind = "ok" | "warn" | "down" | "unknown";

function resolveStatus(
  h: AIHealthSnapshot | undefined,
  isLoading: boolean,
  isError: boolean
): { kind: StatusKind; label: string; color: string; subtitle: string } {
  if (isLoading && !h) {
    return { kind: "unknown", label: "loading", color: "text-muted-foreground", subtitle: "Probing /ai/health…" };
  }
  if (isError || !h) {
    return { kind: "down", label: "unreachable", color: "text-loss", subtitle: "Backend /ai/health is unreachable" };
  }
  if (!h.healthy) {
    return { kind: "down", label: "down", color: "text-loss", subtitle: "Provider failing health checks" };
  }
  if (h.breaker_state && h.breaker_state !== "closed") {
    return { kind: "warn", label: h.breaker_state, color: "text-warning", subtitle: "Circuit breaker tripped — recent failures" };
  }
  return { kind: "ok", label: "healthy", color: "text-profit", subtitle: "Latest probe succeeded" };
}

function StatusDot({ kind }: { kind: StatusKind }) {
  if (kind === "ok")
    return (
      <span className="inline-flex h-3 w-3 items-center justify-center">
        <CheckCircle2 className="h-3 w-3 text-profit" />
      </span>
    );
  return (
    <span
      className={cn(
        "h-2 w-2 rounded-full",
        kind === "warn" && "bg-warning",
        kind === "down" && "bg-loss dot-pulse",
        kind === "unknown" && "bg-muted-foreground/40"
      )}
    />
  );
}

function Stat({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex flex-col leading-tight">
      <span className="text-micro uppercase tracking-wider text-muted-foreground/70">{label}</span>
      {value}
    </div>
  );
}

function formatAgo(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "—";
  const sec = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (sec < 60) return `${sec}s ago`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  return `${Math.floor(hr / 24)}d ago`;
}
