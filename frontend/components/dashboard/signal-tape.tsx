"use client";

import { useMemo, useState } from "react";
import { ShieldAlert, CheckCircle2 } from "lucide-react";
import { DataTable, type DataTableColumn } from "@/components/ui/data-table";
import { ConfidenceCell } from "@/components/ui/confidence-cell";
import { Drawer } from "@/components/ui/drawer";
import { Badge } from "@/components/ui/badge";
import { AISymbolCard } from "@/components/ai/ai-symbol-card";
import type { AuditLog } from "@/lib/api";
import { cn } from "@/lib/utils";

const MAX_ROWS = 50;

interface SignalTapeProps {
  /**
   * All recent audit logs, newest first. Rows where `interval ===
   * "monitor"` are filtered out — those are SL/TP checks, not
   * actionable signals.
   */
  logs: AuditLog[];
  className?: string;
}

/**
 * Chronological dense feed of AI evaluations — one row per non-monitor
 * audit log. Click any row to open the per-symbol drilldown drawer with
 * the full AISymbolCard history.
 */
export function SignalTape({ logs, className }: SignalTapeProps) {
  const [focusedSymbol, setFocusedSymbol] = useState<string | null>(null);

  const rows = useMemo(
    () => logs.filter((l) => l.interval !== "monitor").slice(0, MAX_ROWS),
    [logs]
  );

  // Counter for the header strip. Computed off the visible window so
  // the numbers always match what the operator sees in the table.
  const counts = useMemo(() => {
    let executed = 0;
    let blocked = 0;
    let hold = 0;
    for (const l of rows) {
      if (l.action === "HOLD") hold++;
      else if (l.executed) executed++;
      else if (l.blocked_by) blocked++;
    }
    return { total: rows.length, executed, blocked, hold };
  }, [rows]);

  // Per-symbol history backing the drawer. The source array is already
  // DESC by prompt_sent_at, so grouping preserves recency order — the
  // drilldown card's "latest" view sees the correct top entry.
  const logsBySymbol = useMemo(() => {
    const map = new Map<string, AuditLog[]>();
    for (const log of logs) {
      if (log.interval === "monitor") continue;
      const list = map.get(log.symbol) ?? [];
      list.push(log);
      map.set(log.symbol, list);
    }
    return map;
  }, [logs]);

  const columns: DataTableColumn<AuditLog>[] = useMemo(
    () => [
      {
        key: "time",
        header: "Time",
        align: "left",
        width: "w-[72px]",
        cell: (l) => (
          <span className="text-muted-foreground">{formatTime(l.prompt_sent_at)}</span>
        ),
      },
      {
        key: "symbol",
        header: "Symbol",
        align: "left",
        width: "w-[88px]",
        cell: (l) => <span className="font-medium">{prettySymbol(l.symbol)}</span>,
      },
      {
        key: "action",
        header: "Action",
        align: "left",
        width: "w-[64px]",
        cell: (l) => <ActionBadge action={l.action} />,
      },
      {
        key: "conf",
        header: "Conf",
        title: "Confidence (0–100)",
        width: "w-[88px]",
        cell: (l) => <ConfidenceCell value={l.confidence} />,
      },
      {
        key: "price",
        header: "Price",
        title: "Market price at evaluation",
        width: "w-[88px]",
        cell: (l) => formatPrice(l.market_price),
      },
      {
        key: "status",
        header: "Status",
        align: "left",
        width: "w-[112px]",
        hideBelow: "md",
        cell: (l) => <StatusCell log={l} />,
      },
      {
        key: "reason",
        header: "Reasoning",
        align: "left",
        hideBelow: "lg",
        cell: (l) => (
          <span className="block max-w-[480px] truncate text-muted-foreground">
            {l.reasoning || "—"}
          </span>
        ),
      },
    ],
    []
  );

  const focusedLogs = focusedSymbol ? logsBySymbol.get(focusedSymbol) ?? [] : [];

  return (
    <>
      <div className={cn("space-y-2", className)}>
        <SignalCounter counts={counts} />
        <DataTable
          columns={columns}
          rows={rows}
          rowKey={(l) => l.id}
          onRowClick={(l) => setFocusedSymbol(l.symbol)}
          empty="No AI signals yet — waiting for first evaluation"
        />
      </div>
      <Drawer
        open={focusedSymbol !== null}
        onClose={() => setFocusedSymbol(null)}
        title={focusedSymbol ? `${prettySymbol(focusedSymbol)} · signal history` : "Signal"}
      >
        {focusedSymbol && (
          <AISymbolCard symbol={focusedSymbol} logs={focusedLogs} />
        )}
      </Drawer>
    </>
  );
}

function SignalCounter({
  counts,
}: {
  counts: { total: number; executed: number; blocked: number; hold: number };
}) {
  return (
    <div className="flex flex-wrap items-center gap-2 px-1 text-micro font-mono">
      <span className="text-muted-foreground">
        {counts.total} signal{counts.total === 1 ? "" : "s"}
      </span>
      <span className="text-muted-foreground/50">·</span>
      <CounterPill label="executed" value={counts.executed} tone="profit" />
      <CounterPill label="blocked" value={counts.blocked} tone="warning" />
      <CounterPill label="hold" value={counts.hold} tone="muted" />
    </div>
  );
}

function CounterPill({
  label,
  value,
  tone,
}: {
  label: string;
  value: number;
  tone: "profit" | "warning" | "muted";
}) {
  const toneClass =
    tone === "profit"
      ? "bg-profit/10 text-profit"
      : tone === "warning"
      ? "bg-warning/10 text-warning"
      : "bg-muted text-muted-foreground";
  return (
    <span className={cn("inline-flex items-center gap-1 rounded px-1.5 py-0.5", toneClass)}>
      <span className="font-mono tabular-nums font-semibold">{value}</span>
      <span className="opacity-80">{label}</span>
    </span>
  );
}

function ActionBadge({ action }: { action: string }) {
  if (action === "BUY") {
    return (
      <Badge variant="default" className="px-1.5 py-0 text-micro font-semibold">
        BUY
      </Badge>
    );
  }
  if (action === "SELL") {
    return (
      <Badge variant="destructive" className="px-1.5 py-0 text-micro font-semibold">
        SELL
      </Badge>
    );
  }
  return (
    <span className="text-micro font-semibold uppercase tracking-wider text-muted-foreground">
      {action || "—"}
    </span>
  );
}

function StatusCell({ log }: { log: AuditLog }) {
  // HOLD takes precedence — backend tags HOLD signals with
  // blocked_by="confidence" which looks like a risk block but is just
  // "AI returned HOLD". Surfacing it as a yellow shield misleads the
  // operator into thinking a trade was suppressed.
  if (log.action === "HOLD") {
    return (
      <span className="inline-flex items-center gap-1 rounded bg-muted px-1.5 py-0.5 text-micro text-muted-foreground">
        hold
      </span>
    );
  }
  if (log.executed) {
    return (
      <span className="inline-flex items-center gap-1 rounded bg-profit/10 px-1.5 py-0.5 text-micro font-medium text-profit">
        <CheckCircle2 className="h-3 w-3" /> executed
      </span>
    );
  }
  if (log.blocked_by) {
    return (
      <span
        className="inline-flex items-center gap-1 rounded bg-warning/10 px-1.5 py-0.5 text-micro font-medium text-warning"
        title={`Blocked by ${log.blocked_by}`}
      >
        <ShieldAlert className="h-3 w-3" /> {log.blocked_by}
      </span>
    );
  }
  return <span className="text-micro text-muted-foreground">—</span>;
}

function prettySymbol(s: string): string {
  return s.endsWith("USDT") ? s.replace("USDT", "/USDT") : s;
}

function formatPrice(p: string): string {
  const n = parseFloat(p || "0");
  if (!Number.isFinite(n) || n <= 0) return "—";
  if (n >= 1000) return n.toLocaleString(undefined, { maximumFractionDigits: 2 });
  if (n >= 1) return n.toFixed(4);
  return n.toFixed(6);
}

function formatTime(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "—";
  return new Date(t).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}
