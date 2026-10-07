"use client";

import { useEffect, useState, useCallback } from "react";
import { useRouter } from "next/navigation";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  Command,
  CommandInput,
  CommandList,
  CommandEmpty,
  CommandItem,
  CommandGroup,
} from "@/components/ui/command";
import { api, AVAILABLE_COINS } from "@/lib/api";
import {
  LayoutDashboard,
  LineChart,
  History,
  Brain,
  Settings,
  OctagonX,
  ShieldCheck,
  Play,
  Pause,
  Search,
  FlaskConical,
} from "lucide-react";

interface CommandPaletteProps {
  onPauseToggle?: () => void;
  isPaused?: boolean;
}

export function CommandPalette({ onPauseToggle, isPaused }: CommandPaletteProps) {
  const [open, setOpen] = useState(false);
  const router = useRouter();
  const queryClient = useQueryClient();

  const resetMutation = useMutation({
    mutationFn: () => api.resetCircuitBreaker(),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["risk-status"] }),
  });

  useEffect(() => {
    function handler(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key === "k") {
        e.preventDefault();
        setOpen((prev) => !prev);
      }
      if (e.key === "Escape" && open) {
        setOpen(false);
      }
    }
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [open]);

  const runAction = useCallback(
    (action: () => void) => {
      action();
      setOpen(false);
    },
    []
  );

  if (!open) return null;

  return (
    <div className="fixed inset-0 z-[100] flex items-start justify-center pt-[20vh] animate-fade-in">
      {/* Backdrop */}
      <div
        className="absolute inset-0 bg-black/40 dark:bg-black/70 backdrop-blur-sm"
        onClick={() => setOpen(false)}
      />

      {/* Palette */}
      <div className="relative w-full max-w-lg rounded-xl border border-border bg-card shadow-2xl animate-slide-up">
        <Command>
          <CommandInput placeholder="Type a command or search..." />
          <CommandList className="max-h-[320px]">
            <CommandEmpty>No results found.</CommandEmpty>

            <CommandGroup heading="Navigation">
              <CommandItem onSelect={() => runAction(() => router.push("/"))}>
                <LayoutDashboard className="mr-2 h-4 w-4 text-muted-foreground" />
                Dashboard
              </CommandItem>
              <CommandItem onSelect={() => runAction(() => router.push("/chart"))}>
                <LineChart className="mr-2 h-4 w-4 text-muted-foreground" />
                Chart View
              </CommandItem>
              <CommandItem onSelect={() => runAction(() => router.push("/trades"))}>
                <History className="mr-2 h-4 w-4 text-muted-foreground" />
                Trade History
              </CommandItem>
              <CommandItem onSelect={() => runAction(() => router.push("/ai"))}>
                <Brain className="mr-2 h-4 w-4 text-muted-foreground" />
                AI Brain
              </CommandItem>
              <CommandItem onSelect={() => runAction(() => router.push("/settings"))}>
                <Settings className="mr-2 h-4 w-4 text-muted-foreground" />
                Settings
              </CommandItem>
            </CommandGroup>

            <CommandGroup heading="Risk Management">
              <CommandItem
                onSelect={() => runAction(() => resetMutation.mutate())}
              >
                <ShieldCheck className="mr-2 h-4 w-4 text-profit" />
                Reset Circuit Breaker
              </CommandItem>
              {onPauseToggle && (
                <CommandItem onSelect={() => runAction(onPauseToggle)}>
                  {isPaused ? (
                    <Play className="mr-2 h-4 w-4 text-warning" />
                  ) : (
                    <Pause className="mr-2 h-4 w-4 text-warning" />
                  )}
                  {isPaused ? "Resume AI Trading" : "Pause AI Trading"}
                </CommandItem>
              )}
            </CommandGroup>

            <CommandGroup heading="Quick Actions">
              <CommandItem
                onSelect={() =>
                  runAction(() =>
                    api.runBacktest({ symbol: "BTCUSDT", interval: "1h" })
                  )
                }
              >
                <FlaskConical className="mr-2 h-4 w-4 text-info" />
                Run BTC Backtest
              </CommandItem>
            </CommandGroup>

            <CommandGroup heading="Focus Symbol">
              {AVAILABLE_COINS.map((coin) => (
                <CommandItem
                  key={coin.symbol}
                  onSelect={() =>
                    runAction(() =>
                      router.push(`/?coins=${coin.symbol}&range=24H`)
                    )
                  }
                >
                  <div
                    className="mr-2 h-2.5 w-2.5 rounded-full"
                    style={{ backgroundColor: coin.color }}
                  />
                  {coin.label} — {coin.symbol}
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
        </Command>

        {/* Footer hint */}
        <div className="flex items-center justify-between border-t border-border px-3 py-1.5">
          <div className="flex items-center gap-3">
            <span className="flex items-center gap-1 text-micro text-muted-foreground">
              <Search className="h-3 w-3" /> Search
            </span>
            <span className="text-micro text-border">|</span>
            <span className="text-micro text-muted-foreground">
              <kbd className="rounded border border-border bg-muted px-1 py-0.5 text-micro font-mono">
                Esc
              </kbd>{" "}
              Close
            </span>
          </div>
          <span className="text-micro text-muted-foreground">
            <kbd className="rounded border border-border bg-muted px-1 py-0.5 text-micro font-mono">
              {"\u2318"}K
            </kbd>
          </span>
        </div>
      </div>
    </div>
  );
}
