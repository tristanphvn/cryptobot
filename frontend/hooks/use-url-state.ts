"use client";

import { useSearchParams, useRouter } from "next/navigation";
import { useCallback, useMemo } from "react";
import { AVAILABLE_COINS, type TimeRange } from "@/lib/api";

const VALID_RANGES = new Set<string>(["1H", "4H", "24H", "7D", "30D", "90D", "1Y", "ALL"]);

interface UrlState {
  symbols: string[];
  /**
   * True when ?coins= is present in the URL — i.e. operator made an
   * explicit selection (via CoinPicker or a shared link). False when
   * the URL is bare; callers are then free to derive symbols from
   * live state (open positions, AI signals) instead of falling back
   * to a hard-coded list.
   */
  coinsExplicit: boolean;
  timeRange: TimeRange;
}

/**
 * Single source of truth for dashboard URL state.
 * Reads from URL params, writes back on change.
 * No local useState — the URL IS the state.
 */
export function useUrlState() {
  const searchParams = useSearchParams();
  const router = useRouter();

  const state: UrlState = useMemo(() => {
    const coinParam = searchParams.get("coins");
    const rangeParam = searchParams.get("range");

    const symbols = coinParam
      ? coinParam.split(",").filter((s) => AVAILABLE_COINS.some((c) => c.symbol === s))
      : [];

    const timeRange =
      rangeParam && VALID_RANGES.has(rangeParam) ? (rangeParam as TimeRange) : "24H";

    return {
      symbols,
      coinsExplicit: !!coinParam,
      timeRange,
    };
  }, [searchParams]);

  const setSymbols = useCallback(
    (symbols: string[]) => {
      const params = new URLSearchParams(searchParams.toString());
      if (symbols.length === 0) {
        params.delete("coins");
      } else {
        params.set("coins", symbols.join(","));
      }
      router.replace(`?${params.toString()}`, { scroll: false });
    },
    [searchParams, router]
  );

  const setTimeRange = useCallback(
    (range: TimeRange) => {
      const params = new URLSearchParams(searchParams.toString());
      params.set("range", range);
      router.replace(`?${params.toString()}`, { scroll: false });
    },
    [searchParams, router]
  );

  return { ...state, setSymbols, setTimeRange };
}
