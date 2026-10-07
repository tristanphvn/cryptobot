"use client";

import { useEffect, useRef, useCallback, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { setLivePrice } from "@/lib/live-prices";

const WS_URL = process.env.NEXT_PUBLIC_WS_URL || "ws://localhost:8081/ws";

export interface WSHealth {
  connected: boolean;
  latencyMs: number | null;
  lastMessageAt: number | null;
  reconnectCount: number;
}

interface WSMessage {
  type: string;
  data: unknown;
}

/** Raw candle tick from the backend WS broadcast (mirrors models.Candle JSON) */
export interface WSCandle {
  id: number;
  symbol: string;
  interval: string;
  open_time: string;
  close_time: string;
  open: string;
  high: string;
  low: string;
  close: string;
  volume: string;
}

interface UseWebSocketOptions {
  /** Minimum ms between query invalidations (default 3000) */
  throttleMs?: number;
  onSignal?: (data: { action: string; confidence: number; reasoning: string }) => void;
  /** Called on every candle tick — not throttled. Used by usePriceChart for live merge. */
  onCandle?: (candle: WSCandle) => void;
}

/**
 * Optimized WebSocket hook with:
 * - Throttled query invalidation to prevent UI jank
 * - Connection health tracking (latency, reconnect count)
 * - Auto-reconnect with exponential backoff capped at 10s
 * - Message batching via requestAnimationFrame
 */
export function useWebSocket(options: UseWebSocketOptions = {}) {
  const { throttleMs = 3000, onSignal, onCandle } = options;
  const queryClient = useQueryClient();
  const wsRef = useRef<WebSocket | null>(null);
  const closedRef = useRef(false);
  const lastInvalidateRef = useRef(0);
  // Independent throttle for approval-related events. A sweeper batch
  // can fire many signal.expired in quick succession; we coalesce them
  // into one query refetch within a 500ms window.
  const lastSignalInvalidateRef = useRef(0);
  const signalInvalidateTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const reconnectCountRef = useRef(0);
  const pingIntervalRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const onSignalRef = useRef(onSignal);
  onSignalRef.current = onSignal;
  const onCandleRef = useRef(onCandle);
  onCandleRef.current = onCandle;

  const [health, setHealth] = useState<WSHealth>({
    connected: false,
    latencyMs: null,
    lastMessageAt: null,
    reconnectCount: 0,
  });

  const connect = useCallback(() => {
    if (closedRef.current) return;

    const ws = new WebSocket(WS_URL);
    wsRef.current = ws;

    ws.onopen = () => {
      reconnectCountRef.current = 0;
      setHealth((h) => ({ ...h, connected: true, reconnectCount: 0 }));

      // Application-level ping/pong: client tags a timestamp, server
      // echoes it back, client computes RTT. We don't rely on the WS
      // ping/pong control frames because the browser hides them.
      pingIntervalRef.current = setInterval(() => {
        if (ws.readyState === WebSocket.OPEN) {
          ws.send(JSON.stringify({ type: "ping", ts: Date.now() }));
        }
      }, 15000);
    };

    ws.onmessage = (event) => {
      const now = Date.now();
      setHealth((h) => ({ ...h, lastMessageAt: now }));

      try {
        const msg = JSON.parse(event.data) as WSMessage;

        // Pong is handled outside rAF — RTT measurement should not be
        // delayed by render scheduling.
        if (msg.type === "pong") {
          const sent = (msg as { ts?: number }).ts;
          if (typeof sent === "number") {
            setHealth((h) => ({ ...h, latencyMs: Math.max(0, now - sent) }));
          }
          return;
        }

        // Use requestAnimationFrame to batch state updates
        requestAnimationFrame(() => {
          switch (msg.type) {
            case "candle": {
              // Forward every tick to usePriceChart — no throttle
              const cd = msg.data as WSCandle;
              onCandleRef.current?.(cd);

              // Backend never updates positions.current_price /
              // unrealized_pnl on the server side, so the dashboard's
              // OpenPositionsBlotter would otherwise render the stale
              // DB zero. Sink every tick into the shared live-price store
              // unconditionally — any component subscribed via
              // useLivePrice picks up the fresh quote immediately, no
              // matter which page mounted the WS.
              const px = parseFloat(cd.close);
              if (Number.isFinite(px)) {
                setLivePrice(cd.symbol, px, now);
              }

              // Throttle query invalidation for non-chart queries
              if (now - lastInvalidateRef.current > throttleMs) {
                lastInvalidateRef.current = now;
                queryClient.invalidateQueries({ queryKey: ["analytics-history"] });
                queryClient.invalidateQueries({ queryKey: ["stats"] });
                queryClient.invalidateQueries({ queryKey: ["risk-status"] });
                queryClient.invalidateQueries({ queryKey: ["positions"] });
              }
              break;
            }
            case "trade": {
              queryClient.invalidateQueries({ queryKey: ["trades"] });
              queryClient.invalidateQueries({ queryKey: ["stats"] });
              queryClient.invalidateQueries({ queryKey: ["positions"] });
              queryClient.invalidateQueries({ queryKey: ["risk-status"] });
              break;
            }
            // CRY-34: a live exit failed to sell — refetch positions
            // now so the exit-failure banner doesn't wait for polling.
            case "position.exit_failed": {
              queryClient.invalidateQueries({ queryKey: ["positions"] });
              break;
            }
            case "signal": {
              onSignalRef.current?.(
                msg.data as { action: string; confidence: number; reasoning: string }
              );
              break;
            }
            // Approval gate (CRY-5). Every transition triggers a
            // refetch of the pending list — but a sweeper batch can
            // fire many events in <100ms, so coalesce into one refetch
            // per 500ms window. The 10s polling fallback in
            // use-pending-signals guarantees convergence regardless.
            case "signal.pending":
            case "signal.approved":
            case "signal.rejected":
            case "signal.expired":
            case "signal.crash_recovered":
            case "signal.queue_full": {
              const SIG_THROTTLE_MS = 500;
              const sinceLast = now - lastSignalInvalidateRef.current;
              if (sinceLast >= SIG_THROTTLE_MS) {
                lastSignalInvalidateRef.current = now;
                queryClient.invalidateQueries({ queryKey: ["pending-signals"] });
              } else if (signalInvalidateTimerRef.current === null) {
                // Schedule a single trailing refetch so the dashboard
                // reflects the last event in a burst, not just the first.
                signalInvalidateTimerRef.current = setTimeout(() => {
                  lastSignalInvalidateRef.current = Date.now();
                  signalInvalidateTimerRef.current = null;
                  queryClient.invalidateQueries({ queryKey: ["pending-signals"] });
                }, SIG_THROTTLE_MS - sinceLast);
              }
              break;
            }
          }
        });
      } catch {
        // Ignore parse errors
      }
    };

    ws.onerror = () => {
      setHealth((h) => ({ ...h, connected: false }));
    };

    ws.onclose = () => {
      if (pingIntervalRef.current) clearInterval(pingIntervalRef.current);
      setHealth((h) => ({ ...h, connected: false }));

      if (!closedRef.current) {
        reconnectCountRef.current++;
        const delay = Math.min(1000 * Math.pow(2, reconnectCountRef.current), 10000);
        setHealth((h) => ({ ...h, reconnectCount: reconnectCountRef.current }));
        setTimeout(connect, delay);
      }
    };
  }, [queryClient, throttleMs]);

  useEffect(() => {
    closedRef.current = false;
    connect();

    return () => {
      closedRef.current = true;
      if (pingIntervalRef.current) clearInterval(pingIntervalRef.current);
      if (signalInvalidateTimerRef.current) {
        clearTimeout(signalInvalidateTimerRef.current);
        signalInvalidateTimerRef.current = null;
      }
      wsRef.current?.close();
    };
  }, [connect]);

  return health;
}
