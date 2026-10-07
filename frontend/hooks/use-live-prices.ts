"use client";

import { useCallback, useSyncExternalStore } from "react";
import { useWebSocket, type WSHealth } from "./use-websocket";
import {
  getLivePrice,
  getLivePricesVersion,
  subscribeLivePrices,
  type LiveQuote,
} from "@/lib/live-prices";

/** How old a quote must be before a reader considers it stale. */
const STALE_AFTER_MS = 30_000;

export interface LivePriceSnapshot extends LiveQuote {
  /** True when no recent tick or the WS is disconnected. */
  isStale: boolean;
}

/**
 * Mount once on a page that wants WS to be active. The candle → live
 * price store write happens inside useWebSocket itself, so this is a
 * thin wrapper — kept as a named hook to make the "WS is live for
 * prices" intent explicit at the call site. Returns the WS health
 * snapshot so the caller can render connection status if desired.
 *
 * Important: the store is module-scoped, so multiple `useLivePricesWS`
 * mounts would each open a WS. Mount it at most once per page (the
 * dashboard already mounts `useWebSocket` directly for chart ticks; the
 * trades page mounts this hook because it has no other WS).
 */
export function useLivePricesWS(): WSHealth {
  return useWebSocket();
}

/**
 * Subscribe a component to the latest quote for `symbol`. Returns
 * undefined until the first tick lands. `isStale` flips true when no
 * tick has arrived in STALE_AFTER_MS or the caller passes `wsConnected
 * === false`.
 *
 * Note: staleness is re-evaluated on every store update, not on a timer
 * — a price that goes quiet will not "flip" to stale until the next
 * unrelated tick triggers a re-render. For the trades table that's
 * fine: at least one symbol ticks every few seconds and re-renders the
 * whole table.
 */
export function useLivePrice(
  symbol: string,
  wsConnected = true,
): LivePriceSnapshot | undefined {
  const getSnapshot = useCallback(() => getLivePrice(symbol), [symbol]);
  const quote = useSyncExternalStore(subscribeLivePrices, getSnapshot, () => undefined);
  if (!quote) return undefined;
  const isStale = !wsConnected || Date.now() - quote.at > STALE_AFTER_MS;
  return { ...quote, isStale };
}

/**
 * Read all currently-known quotes. The version counter is the
 * useSyncExternalStore snapshot so React subscribes correctly without
 * us having to allocate a new Map on every render.
 */
export function useLivePrices(): { version: number; get: (symbol: string) => LiveQuote | undefined } {
  const version = useSyncExternalStore(
    subscribeLivePrices,
    getLivePricesVersion,
    () => 0,
  );
  return { version, get: getLivePrice };
}
