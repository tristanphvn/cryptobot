"use client";

import { useQuery } from "@tanstack/react-query";
import { CircleAlert } from "lucide-react";
import { api, type Position } from "@/lib/api";

/**
 * ExitFailureBanner — CRY-34. Shown under the TopStatusBar on every page
 * while a live stop-loss / take-profit / trailing exit keeps failing to
 * sell on the venue: the coins are still held and unprotected.
 *
 * Driven by the polled positions API (not a one-shot WS toast) so it
 * can't be missed on a page without a WS connection; the WS
 * `position.exit_failed` event only triggers an immediate refetch. It
 * disappears once the position closes or the SELL fills.
 */
export function ExitFailureBanner() {
  const { data: positions = [] } = useQuery({
    queryKey: ["positions"],
    queryFn: api.getPositions,
    refetchInterval: 5_000,
  });

  const failing = positions.filter(
    (p): p is Position & { exit_failure: NonNullable<Position["exit_failure"]> } =>
      p.is_open && !!p.exit_failure
  );
  if (failing.length === 0) return null;

  return (
    <div
      role="alert"
      className="flex flex-col gap-1 border-b border-loss/50 bg-loss/15 px-4 py-1.5 text-data text-loss"
    >
      {failing.map((p) => {
        const f = p.exit_failure;
        return (
          <div key={p.id} className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
            <CircleAlert className="h-3.5 w-3.5 shrink-0" />
            <span className="font-semibold uppercase tracking-wider">Live exit failed</span>
            <span className="font-medium text-foreground">{p.symbol}</span>
            <span>
              {f.exit_reason} · attempt {f.attempt} · next retry {formatClock(f.next_retry_at)}
            </span>
            <span className="truncate text-muted-foreground" title={f.detail}>
              {f.reason}
            </span>
          </div>
        );
      })}
    </div>
  );
}

function formatClock(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "—" : d.toLocaleTimeString();
}
