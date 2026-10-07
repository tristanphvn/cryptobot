"use client";

import { useQuery } from "@tanstack/react-query";
import { api, AIHealthSnapshot } from "@/lib/api";

const REFETCH_MS = 15_000;

export function useAIHealth() {
  return useQuery<AIHealthSnapshot>({
    queryKey: ["ai-health"],
    queryFn: api.getAIHealth,
    refetchInterval: REFETCH_MS,
    refetchOnWindowFocus: true,
    staleTime: REFETCH_MS,
  });
}
