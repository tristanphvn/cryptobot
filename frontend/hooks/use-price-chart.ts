"use client";

import { useCallback, useEffect, useRef } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type Candle } from "@/lib/api";
import type { WSCandle } from "./use-websocket";

/**
 * Maximum candles kept in the live window. Must be >= the LIMIT we ask the
 * REST endpoint for, otherwise WS appends would push older REST candles out
 * of the window faster than refetches refill them.
 */
const MAX_CANDLES = 300;

/** Interval string → ms. Used for bucket snapping + gap detection. */
const INTERVAL_MS: Record<string, number> = {
  "1m": 60_000,
  "3m": 3 * 60_000,
  "5m": 5 * 60_000,
  "15m": 15 * 60_000,
  "30m": 30 * 60_000,
  "1h": 60 * 60_000,
  "2h": 2 * 60 * 60_000,
  "4h": 4 * 60 * 60_000,
  "6h": 6 * 60 * 60_000,
  "12h": 12 * 60 * 60_000,
  "1d": 24 * 60 * 60_000,
};

interface UsePriceChartOptions {
  symbol: string;
  interval?: string;
  enabled?: boolean;
  /**
   * When this number changes the hook forces a REST refetch. Pass the WS
   * `reconnectCount` so the chart self-heals after every disconnect by
   * pulling the authoritative snapshot.
   */
  reconnectKey?: number;
}

/** Snap a timestamp DOWN to the start of its interval bucket. */
function snapToBucket(tsMs: number, intervalMs: number): number {
  return Math.floor(tsMs / intervalMs) * intervalMs;
}

function parseTime(t: string): number {
  return new Date(t).getTime();
}

/**
 * Build an evenly-spaced array of candles from a (possibly sparse, possibly
 * out-of-order, possibly duplicated) input list. Output guarantees:
 *
 *   - sorted ascending by open_time
 *   - one entry per bucket from firstBucket .. lastBucket inclusive
 *   - missing buckets become "phantom" candles (open=high=low=close=prev.close,
 *     volume=0, id=-1) so the renderer can keep uniform spacing
 *   - duplicate buckets collapse to the freshest payload
 *   - every open_time / close_time normalised to the exact bucket boundary
 */
export function reconcileCandles(input: Candle[], intervalMs: number): Candle[] {
  if (input.length === 0 || intervalMs <= 0) return input;

  const byBucket = new Map<number, Candle>();
  for (const c of input) {
    const b = snapToBucket(parseTime(c.open_time), intervalMs);
    byBucket.set(b, c); // last write wins
  }

  const buckets = Array.from(byBucket.keys()).sort((a, b) => a - b);
  const firstBucket = buckets[0];
  const lastBucket = buckets[buckets.length - 1];

  const out: Candle[] = [];
  let prev: Candle | null = null;
  for (let b = firstBucket; b <= lastBucket; b += intervalMs) {
    const real = byBucket.get(b);
    if (real) {
      const normalized: Candle = {
        ...real,
        open_time: new Date(b).toISOString(),
        close_time: new Date(b + intervalMs - 1).toISOString(),
      };
      out.push(normalized);
      prev = normalized;
    } else if (prev) {
      out.push({
        id: -1,
        symbol: prev.symbol,
        interval: prev.interval,
        open_time: new Date(b).toISOString(),
        close_time: new Date(b + intervalMs - 1).toISOString(),
        open: prev.close,
        high: prev.close,
        low: prev.close,
        close: prev.close,
        volume: "0",
      });
    }
  }

  if (out.length > MAX_CANDLES) {
    return out.slice(out.length - MAX_CANDLES);
  }
  return out;
}

export function usePriceChart({
  symbol,
  interval = "1m",
  enabled = true,
  reconnectKey,
}: UsePriceChartOptions) {
  const queryClient = useQueryClient();
  const queryKey = ["candles", symbol, interval];
  const intervalMs = INTERVAL_MS[interval] ?? 60_000;

  const symbolRef = useRef(symbol);
  symbolRef.current = symbol;
  const intervalRef = useRef(interval);
  intervalRef.current = interval;
  const intervalMsRef = useRef(intervalMs);
  intervalMsRef.current = intervalMs;

  const { data: candles = [], isLoading } = useQuery({
    queryKey,
    queryFn: async () => {
      const raw = await api.getCandles(symbol, interval, MAX_CANDLES);
      return reconcileCandles(raw, intervalMs);
    },
    enabled: enabled && !!symbol,
    refetchInterval: 10_000,
    staleTime: 8_000,
  });

  // Force a refetch whenever the WS reconnects so we backfill the offline gap.
  const lastReconnectRef = useRef<number | undefined>(undefined);
  useEffect(() => {
    if (reconnectKey === undefined) return;
    if (lastReconnectRef.current === undefined) {
      lastReconnectRef.current = reconnectKey;
      return;
    }
    if (reconnectKey !== lastReconnectRef.current) {
      lastReconnectRef.current = reconnectKey;
      queryClient.invalidateQueries({ queryKey });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reconnectKey, symbol, interval]);

  const onTick = useCallback(
    (tick: WSCandle) => {
      if (tick.symbol !== symbolRef.current) return;
      if (tick.interval !== intervalRef.current) return;

      const ms = intervalMsRef.current;
      const tickBucket = snapToBucket(parseTime(tick.open_time), ms);

      queryClient.setQueryData<Candle[]>(
        ["candles", symbolRef.current, intervalRef.current],
        (prev) => {
          const incoming: Candle = {
            id: tick.id || 0,
            symbol: tick.symbol,
            interval: tick.interval,
            open_time: new Date(tickBucket).toISOString(),
            close_time: new Date(tickBucket + ms - 1).toISOString(),
            open: tick.open,
            high: tick.high,
            low: tick.low,
            close: tick.close,
            volume: tick.volume,
          };

          // Fast path: in-progress candle update — patch tail in place,
          // avoid an O(n) reconcile per tick.
          if (prev && prev.length > 0) {
            let lastRealIdx = prev.length - 1;
            while (lastRealIdx >= 0 && prev[lastRealIdx].id === -1) {
              lastRealIdx--;
            }
            if (lastRealIdx >= 0) {
              const lastReal = prev[lastRealIdx];
              const lastBucket = snapToBucket(parseTime(lastReal.open_time), ms);
              if (tickBucket === lastBucket) {
                const merged: Candle = {
                  ...lastReal,
                  close: incoming.close,
                  high: maxStr(lastReal.high, incoming.high),
                  low: minStr(lastReal.low, incoming.low),
                  volume: incoming.volume,
                  close_time: incoming.close_time,
                };
                if (
                  merged.close === lastReal.close &&
                  merged.high === lastReal.high &&
                  merged.low === lastReal.low &&
                  merged.volume === lastReal.volume
                ) {
                  return prev;
                }
                const next = prev.slice();
                next[lastRealIdx] = merged;
                // Drop any phantoms after the active candle (keeps invariant).
                return next.slice(0, lastRealIdx + 1);
              }
              if (tickBucket < lastBucket) {
                // Stale / out-of-order tick — ignore.
                return prev;
              }
            }
          }

          // Slow path: window advances. Full reconcile gap-fills correctly.
          const merged = prev ? [...prev, incoming] : [incoming];
          return reconcileCandles(merged, ms);
        }
      );
    },
    [queryClient]
  );

  return { candles, isLoading, onTick };
}

function maxStr(a: string, b: string): string {
  return parseFloat(a) >= parseFloat(b) ? a : b;
}
function minStr(a: string, b: string): string {
  return parseFloat(a) <= parseFloat(b) ? a : b;
}
