"use client";

import { useQuery } from "@tanstack/react-query";
import { api, AuditLogResponse } from "@/lib/api";

const REFETCH_MS = 15_000;

interface UseAuditLogsOptions {
  symbol?: string;
  limit?: number;
}

export function useAuditLogs({ symbol, limit = 50 }: UseAuditLogsOptions = {}) {
  return useQuery<AuditLogResponse>({
    queryKey: ["audit-logs", { symbol: symbol ?? null, limit }],
    queryFn: () => api.getAuditLogs(symbol, limit),
    refetchInterval: REFETCH_MS,
    refetchOnWindowFocus: true,
    staleTime: REFETCH_MS,
  });
}
