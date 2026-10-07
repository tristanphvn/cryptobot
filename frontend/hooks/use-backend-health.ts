"use client";

import { useQuery } from "@tanstack/react-query";
import { api, type BackendHealth } from "@/lib/api";

/**
 * Polls the backend /health endpoint every 10s so the dashboard can
 * surface collector reconnects, persister write failures, and hub
 * client-drop counters without opening a second websocket.
 *
 * Failures are silent — health is informational; the dashboard should
 * keep working even if /health is briefly unreachable.
 */
export function useBackendHealth(intervalMs = 10_000): BackendHealth | null {
  const { data } = useQuery<BackendHealth>({
    queryKey: ["backend-health"],
    queryFn: () => api.getBackendHealth(),
    refetchInterval: intervalMs,
    refetchIntervalInBackground: false,
    retry: false,
    staleTime: intervalMs / 2,
  });
  return data ?? null;
}
