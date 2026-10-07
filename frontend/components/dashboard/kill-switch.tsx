"use client";

import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { cn } from "@/lib/utils";
import {
  OctagonX,
  Pause,
  Play,
  ShieldAlert,
} from "lucide-react";

interface KillSwitchProps {
  halted: boolean;
  className?: string;
}

export function KillSwitch({ halted, className }: KillSwitchProps) {
  const queryClient = useQueryClient();
  const [paused, setPaused] = useState(false);
  const [confirmKill, setConfirmKill] = useState(false);

  const killMutation = useMutation({
    mutationFn: () => api.resetCircuitBreaker(),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["risk-status"] });
    },
  });

  const handleKillClick = () => {
    if (!confirmKill) {
      setConfirmKill(true);
      setTimeout(() => setConfirmKill(false), 3000);
      return;
    }
    killMutation.mutate();
    setConfirmKill(false);
  };

  return (
    <div className={cn("flex items-center gap-2", className)}>
      {/* Kill Switch */}
      <button
        onClick={handleKillClick}
        className={cn(
          "flex items-center gap-1.5 rounded-lg px-3 py-1.5 text-micro font-bold uppercase tracking-wider transition-all duration-200",
          halted
            ? "border border-loss/30 bg-loss/15 text-loss dot-pulse"
            : confirmKill
            ? "border border-loss/50 bg-loss/20 text-loss"
            : "border border-border bg-card text-muted-foreground hover:border-loss/20 hover:bg-loss/[0.06] hover:text-loss"
        )}
      >
        {halted ? (
          <>
            <ShieldAlert className="h-3.5 w-3.5" />
            Halted — Reset
          </>
        ) : confirmKill ? (
          <>
            <OctagonX className="h-3.5 w-3.5" />
            Confirm Kill
          </>
        ) : (
          <>
            <OctagonX className="h-3.5 w-3.5" />
            Kill Switch
          </>
        )}
      </button>

      {/* Pause AI toggle */}
      <button
        onClick={() => setPaused(!paused)}
        className={cn(
          "flex items-center gap-1.5 rounded-lg px-3 py-1.5 text-micro font-medium transition-all duration-200 border",
          paused
            ? "border-warning/20 bg-warning/10 text-warning"
            : "border-border bg-card text-muted-foreground hover:bg-muted hover:text-foreground"
        )}
      >
        {paused ? (
          <>
            <Play className="h-3.5 w-3.5" />
            Resume AI
          </>
        ) : (
          <>
            <Pause className="h-3.5 w-3.5" />
            Pause AI
          </>
        )}
      </button>
    </div>
  );
}
