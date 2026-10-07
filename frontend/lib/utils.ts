import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

export function formatCurrency(value: number): string {
  return new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(value);
}

/**
 * Formats a coin price (or a small USDT amount such as a fee) with
 * precision scaled to its size, so sub-dollar symbols (DOGE, ADA, XRP)
 * keep their significant digits: $64,250.10 · $2.4512 · $0.1288 · $0.00001234.
 * Use formatCurrency for PnL / balances, where cents are enough.
 */
export function formatPrice(value: number): string {
  const abs = Math.abs(value);
  let digits = 2;
  if (abs > 0 && abs < 1) {
    // ~4 significant digits below $1, capped at 8 (Binance precision).
    digits = Math.min(8, Math.max(4, Math.ceil(-Math.log10(abs)) + 3));
  } else if (abs >= 1 && abs < 1000) {
    digits = 4;
  }
  return new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: abs >= 1000 ? 2 : Math.min(digits, 4),
    maximumFractionDigits: digits,
  }).format(value);
}

export function formatPercent(value: number): string {
  return `${value >= 0 ? "+" : ""}${value.toFixed(2)}%`;
}
