"use client";

import { useEffect, useState } from "react";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { usePendingSignals } from "@/hooks/use-pending-signals";
import { cn, formatCurrency, formatPrice } from "@/lib/utils";
import { AVAILABLE_COINS, type PendingSignal } from "@/lib/api";
import { AlertTriangle, Check, KeyRound, X } from "lucide-react";

/**
 * ApprovalCard — operator UI for the human-in-the-loop gate (CRY-5).
 *
 * Standard, not bandaid: the countdown timer is cosmetic only. Source of
 * truth on expiration is the server-side sweeper, so a paused tab can
 * never accidentally approve an expired signal — the backend will
 * return 410 Gone.
 *
 * Mounts the token entry inline when no APPROVAL_TOKEN has been set, so
 * a fresh browser still gets a clear path to interact with the gate
 * instead of silent 401s.
 */
export function ApprovalCard({ className }: { className?: string }) {
  const { pending, authError, token, setToken, approve, reject } = usePendingSignals();

  if (!token || authError) {
    return <TokenEntry onSet={setToken} authError={authError} className={className} />;
  }

  // Defensive UX: even when the backend has accepted the token, expose
  // a way back to TokenEntry. Otherwise a stored token that becomes
  // wrong (rotation, environment swap, backend started without
  // enforcement) traps the user with no path to re-enter.
  const ChangeTokenLink = () => (
    <button
      onClick={() => setToken("")}
      className="flex items-center gap-1 text-micro text-muted-foreground hover:text-foreground"
      title="Clear stored token and re-enter"
    >
      <KeyRound className="h-3 w-3" />
      Change token
    </button>
  );

  if (pending.length === 0) {
    return (
      <Card className={cn("border-muted/20", className)}>
        <CardContent className="flex items-center justify-between p-3 text-micro text-muted-foreground">
          <span>No pending signals. Approval gate active (live mode).</span>
          <ChangeTokenLink />
        </CardContent>
      </Card>
    );
  }

  return (
    <div className={cn("space-y-2", className)}>
      <div className="flex justify-end">
        <ChangeTokenLink />
      </div>
      {pending.map((p) => (
        <PendingSignalRow
          key={p.id}
          signal={p}
          onApprove={() => approve.mutate(p.id)}
          onReject={(reason) => reject.mutate({ id: p.id, reason })}
          busyApprove={approve.isPending && approve.variables === p.id}
          busyReject={reject.isPending && reject.variables?.id === p.id}
        />
      ))}
    </div>
  );
}

function PendingSignalRow({
  signal,
  onApprove,
  onReject,
  busyApprove,
  busyReject,
}: {
  signal: PendingSignal;
  onApprove: () => void;
  onReject: (reason?: string) => void;
  busyApprove: boolean;
  busyReject: boolean;
}) {
  const coin = AVAILABLE_COINS.find((c) => c.symbol === signal.symbol);
  const label = coin?.label || signal.symbol.replace("USDT", "");
  const color = coin?.color || "#888";
  const isBuy = signal.action === "BUY";
  const marketPrice = parseFloat(signal.market_price || "0");
  const sl = parseFloat(signal.proposed_sl || "0");
  const tp = parseFloat(signal.proposed_tp || "0");

  const remaining = useCountdownSec(signal.expires_at);
  const [confirming, setConfirming] = useState(false);

  const handleApproveClick = () => {
    if (!confirming) {
      setConfirming(true);
      return;
    }
    onApprove();
  };

  return (
    <Card
      className={cn(
        "border-warning/30 transition-colors",
        remaining <= 10 && "border-loss/50 dot-pulse"
      )}
    >
      <CardContent className="space-y-2 p-3">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2">
            <div
              className="flex h-7 w-7 items-center justify-center rounded-lg text-micro font-bold"
              style={{ backgroundColor: `${color}20`, color }}
            >
              {label.slice(0, 3)}
            </div>
            <div>
              <div className="flex items-center gap-1.5">
                <span className="text-data font-semibold">{label}/USDT</span>
                <Badge variant={isBuy ? "default" : "destructive"} className="px-1.5 py-0 text-micro">
                  {signal.action}
                </Badge>
                <Badge variant="outline" className="px-1.5 py-0 text-micro font-mono tabular-nums">
                  {(signal.confidence * 100).toFixed(0)}%
                </Badge>
              </div>
              <span className="text-micro text-muted-foreground font-mono tabular-nums">
                Mkt {formatPrice(marketPrice)} · SL {formatPrice(sl)} · TP {formatPrice(tp)}
              </span>
            </div>
          </div>
          <div className="text-right">
            <div
              className={cn(
                "font-mono text-section font-semibold tabular-nums",
                remaining <= 10 ? "text-loss" : "text-warning"
              )}
            >
              {remaining}s
            </div>
            <div className="text-micro uppercase tracking-wider text-muted-foreground">expires</div>
          </div>
        </div>

        {signal.reasoning && (
          <p className="line-clamp-2 text-micro leading-relaxed text-muted-foreground">
            {signal.reasoning}
          </p>
        )}

        {confirming && (
          <div className="flex items-start gap-2 rounded-md border border-loss/30 bg-loss/5 p-2 text-micro text-loss">
            <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
            <span>
              This will place a <b>live order</b> on Binance via the executor. Click Approve again to confirm.
            </span>
          </div>
        )}

        <div className="flex gap-2">
          <button
            onClick={handleApproveClick}
            disabled={busyApprove || busyReject || remaining <= 0}
            className={cn(
              "flex flex-1 items-center justify-center gap-1.5 rounded-md px-3 py-1.5 text-data font-medium transition-colors",
              "bg-profit text-primary-foreground hover:bg-profit/90 disabled:bg-muted disabled:text-muted-foreground"
            )}
          >
            <Check className="h-3.5 w-3.5" />
            {confirming ? "Confirm Live Order" : "Approve"}
          </button>
          <button
            onClick={() => onReject()}
            disabled={busyApprove || busyReject || remaining <= 0}
            className={cn(
              "flex flex-1 items-center justify-center gap-1.5 rounded-md px-3 py-1.5 text-data font-medium transition-colors",
              "bg-muted text-foreground hover:bg-muted/80 disabled:opacity-50"
            )}
          >
            <X className="h-3.5 w-3.5" />
            Reject
          </button>
        </div>
      </CardContent>
    </Card>
  );
}

function TokenEntry({
  onSet,
  authError,
  className,
}: {
  onSet: (token: string) => void;
  authError: boolean;
  className?: string;
}) {
  const [value, setValue] = useState("");
  return (
    <Card className={cn("border-warning/30", className)}>
      <CardContent className="space-y-2 p-3">
        <div className="flex items-center gap-2 text-data font-medium">
          <KeyRound className="h-3.5 w-3.5 text-warning" />
          Approval token required
        </div>
        <p className="text-micro text-muted-foreground">
          {authError
            ? "The stored token was rejected by the backend (401). Enter a valid APPROVAL_TOKEN to manage live signals."
            : "Live mode requires a bearer token (env APPROVAL_TOKEN on the backend). The token is stored locally and sent only with operator actions (signal approval, circuit-breaker reset, backtest)."}
        </p>
        <div className="flex gap-2">
          <input
            type="password"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder="APPROVAL_TOKEN"
            className="flex-1 rounded-md border border-muted bg-background px-2 py-1 text-data font-mono"
          />
          <button
            onClick={() => {
              const trimmed = value.trim();
              if (trimmed) onSet(trimmed);
            }}
            disabled={!value.trim()}
            className="rounded-md bg-foreground px-3 py-1 text-data font-medium text-background disabled:opacity-50"
          >
            Save
          </button>
        </div>
      </CardContent>
    </Card>
  );
}

/** useCountdownSec returns seconds-until expiry, refreshed every 1s.
 *  Pure cosmetic — server is authoritative on expiration. */
function useCountdownSec(rfc3339: string): number {
  const [remaining, setRemaining] = useState<number>(() => secsUntil(rfc3339));
  useEffect(() => {
    const tick = () => setRemaining(secsUntil(rfc3339));
    tick();
    const t = setInterval(tick, 1000);
    return () => clearInterval(t);
  }, [rfc3339]);
  return remaining;
}

function secsUntil(rfc3339: string): number {
  const ms = new Date(rfc3339).getTime() - Date.now();
  return Math.max(0, Math.floor(ms / 1000));
}
