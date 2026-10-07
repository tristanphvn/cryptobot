"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import {
  api,
  ApprovalError,
  getApprovalToken,
  setApprovalToken,
  type PendingSignal,
} from "@/lib/api";

/**
 * usePendingSignals wraps the approval-gate REST endpoints with a
 * TanStack Query + mutation layer.
 *
 * Standard, not bandaid:
 * - WS-driven invalidation (use-websocket.ts handles `signal.*` events
 *   and invalidates the `pending-signals` queryKey) is the primary
 *   freshness mechanism.
 * - A 10s polling fallback covers the WS-disconnected window. The
 *   countdown timer is purely cosmetic — the server is authoritative on
 *   expiration, so even a frozen UI cannot accidentally approve an
 *   expired signal (the API will return 410 Gone).
 * - The 401 path triggers the operator to enter / re-enter their
 *   bearer token via `setToken`; we never silently treat missing auth
 *   as anonymous.
 */
export function usePendingSignals() {
  const qc = useQueryClient();
  // Initial state matches SSR (window unavailable → empty string). Reading
  // localStorage in the useState initializer would diverge between server
  // and client render, breaking hydration. We hydrate from localStorage
  // in a post-mount effect instead — first paint is consistent with the
  // server, then state catches up.
  const [token, setTokenState] = useState<string>("");
  const [authError, setAuthError] = useState<boolean>(false);

  useEffect(() => {
    const stored = getApprovalToken();
    if (stored) setTokenState(stored);
  }, []);

  const query = useQuery<PendingSignal[]>({
    queryKey: ["pending-signals"],
    queryFn: async () => {
      try {
        const res = await api.getPendingSignals();
        setAuthError(false);
        return res.pending;
      } catch (err) {
        if (err instanceof ApprovalError && err.status === 401) {
          setAuthError(true);
          return [];
        }
        throw err;
      }
    },
    refetchInterval: 10_000,
    refetchIntervalInBackground: false,
    enabled: typeof window !== "undefined",
  });

  // Re-fetch whenever the token changes so a freshly-entered token is
  // exercised immediately (no stale 401 left over).
  useEffect(() => {
    if (token) qc.invalidateQueries({ queryKey: ["pending-signals"] });
  }, [token, qc]);

  const approve = useMutation({
    mutationFn: (id: number) => api.approveSignal(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["pending-signals"] }),
  });

  const reject = useMutation({
    mutationFn: ({ id, reason }: { id: number; reason?: string }) => api.rejectSignal(id, reason),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["pending-signals"] }),
  });

  const setToken = (value: string) => {
    setApprovalToken(value);
    setTokenState(value);
    // Do NOT optimistically clear authError here. The refetch fired by the
    // useEffect below is what validates the new token: queryFn clears
    // authError on success, sets it on 401. Resetting eagerly would unmount
    // TokenEntry between Save and the failed response, briefly flashing
    // "No pending signals" and re-mounting the form with an empty input —
    // confusing on a wrong-token retry. Keep the form visible until the
    // backend actually accepts the token.
  };

  return {
    pending: query.data ?? [],
    isLoading: query.isLoading,
    error: query.error,
    authError,
    token,
    setToken,
    approve,
    reject,
  };
}
