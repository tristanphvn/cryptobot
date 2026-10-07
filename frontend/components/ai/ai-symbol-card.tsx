"use client";

import { useEffect, useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { Zap, Clock, ShieldAlert, ChevronDown, ChevronUp } from "lucide-react";
import { AuditLog } from "@/lib/api";

interface AISymbolCardProps {
  symbol: string;
  logs: AuditLog[];
}

export const DEFAULT_TICK_MS = 5 * 60 * 1000;
const MAX_HISTORY = 8;

export function AISymbolCard({ symbol, logs }: AISymbolCardProps) {
  const [expanded, setExpanded] = useState(false);
  const latest = logs[0];
  const tickMs = inferTickInterval(logs);
  const nextAt = latest ? Date.parse(latest.prompt_sent_at) + tickMs : null;

  if (!latest) {
    return (
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-sm font-mono">{prettySymbol(symbol)}</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-xs text-muted-foreground">No evaluations yet for this symbol.</p>
        </CardContent>
      </Card>
    );
  }

  return (
    <Card
      className={cn(
        latest.action === "BUY" && "border-profit/20",
        latest.action === "SELL" && "border-loss/20"
      )}
    >
      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
        <CardTitle className="flex items-center gap-2 text-sm">
          <span className="font-mono">{prettySymbol(symbol)}</span>
          <span className="text-micro font-normal text-muted-foreground">
            {latest.interval}
          </span>
        </CardTitle>
        <Badge
          variant={
            latest.action === "BUY"
              ? "default"
              : latest.action === "SELL"
              ? "destructive"
              : "secondary"
          }
          className="text-micro"
        >
          {latest.action}
        </Badge>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="flex items-center gap-4">
          <ConfidenceGauge value={latest.confidence} />
          <div className="flex-1 space-y-1">
            <div className="flex items-center gap-1.5">
              <Zap
                className={cn(
                  "h-3.5 w-3.5",
                  latest.action === "BUY"
                    ? "text-profit"
                    : latest.action === "SELL"
                    ? "text-loss"
                    : "text-warning"
                )}
              />
              <span
                className={cn(
                  "text-lg font-bold font-mono",
                  latest.action === "BUY"
                    ? "text-profit"
                    : latest.action === "SELL"
                    ? "text-loss"
                    : "text-warning"
                )}
              >
                {latest.action}
              </span>
              <span className="text-micro text-muted-foreground font-mono">
                @ ${formatPrice(latest.market_price)}
              </span>
            </div>
            <div className="flex flex-wrap items-center gap-1.5 text-micro">
              <span className="inline-flex items-center gap-1 text-muted-foreground font-mono">
                <Clock className="h-3 w-3" />
                {latest.inference_time_ms > 0
                  ? `${(latest.inference_time_ms / 1000).toFixed(1)}s`
                  : "—"}
              </span>
              {latest.blocked_by && (
                <Badge
                  variant="outline"
                  className="h-4 gap-1 border-warning/40 px-1.5 text-micro text-warning"
                >
                  <ShieldAlert className="h-2.5 w-2.5" />
                  {latest.blocked_by}
                </Badge>
              )}
              {latest.executed && (
                <Badge
                  variant="outline"
                  className="h-4 border-profit/40 px-1.5 text-micro text-profit"
                >
                  executed
                </Badge>
              )}
            </div>
          </div>
        </div>

        <div className="rounded-lg border border-border bg-muted/40 p-3">
          <p className="text-xs leading-relaxed text-muted-foreground">
            {latest.reasoning || <em>(no reasoning provided)</em>}
          </p>
        </div>

        <CountdownLine sentAt={latest.prompt_sent_at} nextAt={nextAt} tickMs={tickMs} />

        {logs.length > 1 && (
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="flex w-full items-center justify-center gap-1 text-micro text-muted-foreground hover:text-foreground"
          >
            {expanded ? <ChevronUp className="h-3 w-3" /> : <ChevronDown className="h-3 w-3" />}
            {expanded ? "Hide history" : `History (${Math.min(logs.length - 1, MAX_HISTORY)})`}
          </button>
        )}

        {expanded && (
          <ul className="space-y-1.5 border-t border-border pt-2">
            {logs.slice(1, 1 + MAX_HISTORY).map((l) => (
              <li
                key={l.id}
                className="flex items-center gap-2 text-micro text-muted-foreground font-mono"
              >
                <span className="w-20 shrink-0">{formatTime(l.prompt_sent_at)}</span>
                <Badge
                  variant={
                    l.action === "BUY"
                      ? "default"
                      : l.action === "SELL"
                      ? "destructive"
                      : "secondary"
                  }
                  className="h-4 px-1 text-micro"
                >
                  {l.action}
                </Badge>
                <span className="w-10 tabular-nums">{(l.confidence * 100).toFixed(0)}%</span>
                {l.blocked_by && (
                  <span className="text-warning/80">↳ {l.blocked_by}</span>
                )}
                <span className="flex-1 truncate">{l.reasoning}</span>
              </li>
            ))}
          </ul>
        )}
      </CardContent>
    </Card>
  );
}

function CountdownLine({
  sentAt,
  nextAt,
  tickMs,
}: {
  sentAt: string;
  nextAt: number | null;
  tickMs: number;
}) {
  const [, forceRender] = useState(0);
  useEffect(() => {
    const id = setInterval(() => forceRender((n) => n + 1), 1000);
    return () => clearInterval(id);
  }, []);

  const sentMs = Date.parse(sentAt);
  const lastAgo = Number.isNaN(sentMs)
    ? "—"
    : formatRelative(Math.max(0, Date.now() - sentMs));
  let nextLabel = "—";
  if (nextAt != null) {
    const diff = nextAt - Date.now();
    nextLabel = diff <= 0 ? "due now" : `~${formatRelative(diff)}`;
  }

  return (
    <div className="flex items-center justify-between text-micro text-muted-foreground font-mono">
      <span>Last eval {lastAgo} ago</span>
      <span>
        Next {nextLabel} <span className="opacity-60">(tick {formatRelative(tickMs)})</span>
      </span>
    </div>
  );
}

function ConfidenceGauge({ value }: { value: number }) {
  const size = 56;
  const stroke = 3.5;
  const radius = (size - stroke) / 2;
  const circumference = 2 * Math.PI * radius;
  const offset = circumference - value * circumference;
  const color =
    value > 0.7
      ? "hsl(var(--profit))"
      : value > 0.4
      ? "hsl(var(--warning))"
      : "hsl(var(--loss))";
  return (
    <div className="relative flex-shrink-0" style={{ width: size, height: size }}>
      <svg width={size} height={size} className="-rotate-90">
        <circle
          cx={size / 2}
          cy={size / 2}
          r={radius}
          fill="none"
          className="stroke-border"
          strokeWidth={stroke}
        />
        <circle
          cx={size / 2}
          cy={size / 2}
          r={radius}
          fill="none"
          stroke={color}
          strokeWidth={stroke}
          strokeLinecap="round"
          strokeDasharray={circumference}
          strokeDashoffset={offset}
          className="transition-all duration-700"
        />
      </svg>
      <div className="absolute inset-0 flex items-center justify-center">
        <span className="text-xs font-bold tabular-nums font-mono" style={{ color }}>
          {(value * 100).toFixed(0)}
        </span>
      </div>
    </div>
  );
}

export function inferTickInterval(logs: AuditLog[]): number {
  if (logs.length < 2) return DEFAULT_TICK_MS;
  const deltas: number[] = [];
  for (let i = 0; i < logs.length - 1 && deltas.length < 5; i++) {
    const a = Date.parse(logs[i].prompt_sent_at);
    const b = Date.parse(logs[i + 1].prompt_sent_at);
    if (Number.isFinite(a) && Number.isFinite(b) && a > b) deltas.push(a - b);
  }
  if (deltas.length === 0) return DEFAULT_TICK_MS;
  deltas.sort((a, b) => a - b);
  return deltas[Math.floor(deltas.length / 2)];
}

function prettySymbol(s: string): string {
  return s.endsWith("USDT") ? s.replace("USDT", "/USDT") : s;
}

function formatPrice(p: string): string {
  const n = parseFloat(p || "0");
  if (!Number.isFinite(n)) return p;
  if (n >= 1000) return n.toLocaleString(undefined, { maximumFractionDigits: 2 });
  if (n >= 1) return n.toFixed(4);
  return n.toFixed(6);
}

function formatTime(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "—";
  return new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
}

function formatRelative(ms: number): string {
  const sec = Math.round(ms / 1000);
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  const remSec = sec % 60;
  if (min < 60) return remSec === 0 ? `${min}m` : `${min}m${remSec}s`;
  const hr = Math.floor(min / 60);
  return `${hr}h${min % 60}m`;
}
