/**
 * Module-level subscribable live-price store.
 *
 * Single writer: the `useLivePricesWS` hook bridges WS candle ticks into
 * `setLivePrice`. Many readers: components subscribe via `useLivePrice` /
 * `useLivePrices` (useSyncExternalStore-friendly). Decoupling store from
 * the WS plumbing means a price consumer never needs to know which page
 * is hosting the websocket connection — readers just read.
 */

export interface LiveQuote {
  /** Last close price (parsed) for the symbol. */
  price: number;
  /** Epoch ms of the tick. */
  at: number;
}

const quotes = new Map<string, LiveQuote>();
const listeners = new Set<() => void>();

/** Internal version counter so useSyncExternalStore re-renders on every
 *  write, even when the same symbol's snapshot object is recreated. */
let version = 0;

export function setLivePrice(symbol: string, price: number, atMs: number): void {
  if (!Number.isFinite(price) || price <= 0) return;
  const prev = quotes.get(symbol);
  if (prev && prev.at >= atMs) return; // Out-of-order tick — ignore.
  quotes.set(symbol, { price, at: atMs });
  version++;
  for (const fn of listeners) fn();
}

export function getLivePrice(symbol: string): LiveQuote | undefined {
  return quotes.get(symbol);
}

export function subscribeLivePrices(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function getLivePricesVersion(): number {
  return version;
}

/** Test/reset utility — not used by production code. */
export function __resetLivePricesForTest(): void {
  quotes.clear();
  version = 0;
}
