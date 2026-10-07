"use client";

import { useMemo } from "react";
import { Sidebar } from "@/components/layout/sidebar";
import { TopStatusBar } from "@/components/dashboard/top-status-bar";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { AIProviderStrip } from "@/components/ai/ai-provider-strip";
import { AISymbolCard } from "@/components/ai/ai-symbol-card";
import { SignalTape } from "@/components/dashboard/signal-tape";
import { useAIHealth } from "@/hooks/use-ai-health";
import { useAuditLogs } from "@/hooks/use-audit-logs";
import { AuditLog } from "@/lib/api";
import { Brain, Cpu, LineChart, BarChart3, Zap } from "lucide-react";

// 100 matches the prior Mission Control fetch so SignalTape's 50-row
// window sees the same depth of history it did when it lived there.
const FETCH_LIMIT = 100;

export default function AIPage() {
  const health = useAIHealth();
  const audit = useAuditLogs({ limit: FETCH_LIMIT });

  const logs = useMemo<AuditLog[]>(() => audit.data?.logs ?? [], [audit.data?.logs]);
  const bySymbol = useMemo(() => groupBySymbol(logs), [logs]);
  const symbols = useMemo(
    () =>
      Object.keys(bySymbol).sort((a, b) => {
        const at = Date.parse(bySymbol[a][0]?.prompt_sent_at ?? "");
        const bt = Date.parse(bySymbol[b][0]?.prompt_sent_at ?? "");
        return (bt || 0) - (at || 0);
      }),
    [bySymbol]
  );

  const latest = logs[0];

  return (
    <div className="flex min-h-screen bg-background">
      <Sidebar />
      <main className="flex-1 pl-14">
        <TopStatusBar pageLabel="AI Brain" ollamaInferenceMs={latest?.inference_time_ms ?? null} />
        <div className="space-y-4 p-5">
          <AIProviderStrip
            health={health.data}
            isLoading={health.isLoading}
            isError={health.isError}
            latestInferenceMs={latest?.inference_time_ms ?? null}
            latestEvalAt={latest?.prompt_sent_at ?? null}
          />

          {audit.isLoading && logs.length === 0 && (
            <Card>
              <CardContent className="py-8 text-center text-sm text-muted-foreground">
                Loading evaluations…
              </CardContent>
            </Card>
          )}

          {audit.isError && (
            <Card>
              <CardContent className="py-6 text-center text-sm text-loss">
                Failed to load audit logs.
              </CardContent>
            </Card>
          )}

          {!audit.isLoading && symbols.length === 0 && !audit.isError && (
            <Card>
              <CardContent className="py-8 text-center text-sm text-muted-foreground">
                No AI evaluations yet. The executor will populate this once the first tick fires.
              </CardContent>
            </Card>
          )}

          {symbols.length > 0 && (
            <div className="grid gap-3 lg:grid-cols-2 xl:grid-cols-3">
              {symbols.map((sym) => (
                <AISymbolCard key={sym} symbol={sym} logs={bySymbol[sym]} />
              ))}
            </div>
          )}

          {logs.length > 0 && <SignalTape logs={logs} />}

          <Card>
            <CardHeader className="flex flex-row items-center gap-2 space-y-0">
              <div className="flex h-6 w-6 items-center justify-center rounded-lg bg-info/10">
                <Cpu className="h-3.5 w-3.5 text-info" />
              </div>
              <CardTitle>How it works</CardTitle>
            </CardHeader>
            <CardContent>
              <div className="space-y-3">
                {[
                  { icon: LineChart, color: "text-info", bg: "bg-info/10", text: "Market data (OHLCV) is fetched from Binance every hour" },
                  { icon: BarChart3, color: "text-profit", bg: "bg-profit/10", text: "RSI (14) and MACD (12,26,9) are calculated locally" },
                  { icon: Brain, color: "text-info", bg: "bg-info/10", text: "A structured prompt is sent to the local Ollama LLM" },
                  { icon: Cpu, color: "text-warning", bg: "bg-warning/10", text: "The LLM returns a JSON signal: BUY / SELL / HOLD with confidence" },
                  { icon: Zap, color: "text-profit", bg: "bg-profit/10", text: "Signals with confidence ≥ 0.6 are executed (dry-run / paper mode)" },
                ].map((step, i) => (
                  <div key={i} className="flex items-start gap-3 rounded-lg border border-border bg-card p-3">
                    <div className={`flex h-6 w-6 shrink-0 items-center justify-center rounded-md ${step.bg}`}>
                      <step.icon className={`h-3.5 w-3.5 ${step.color}`} />
                    </div>
                    <div className="flex items-center gap-2">
                      <span className="text-micro font-bold text-muted-foreground font-mono">{String(i + 1).padStart(2, "0")}</span>
                      <span className="text-sm text-foreground/80">{step.text}</span>
                    </div>
                  </div>
                ))}
              </div>
            </CardContent>
          </Card>
        </div>
      </main>
    </div>
  );
}

function groupBySymbol(logs: AuditLog[]): Record<string, AuditLog[]> {
  const map: Record<string, AuditLog[]> = {};
  for (const l of logs) {
    if (l.interval === "monitor") continue;
    if (!map[l.symbol]) map[l.symbol] = [];
    map[l.symbol].push(l);
  }
  for (const sym of Object.keys(map)) {
    map[sym].sort(
      (a, b) => Date.parse(b.prompt_sent_at) - Date.parse(a.prompt_sent_at)
    );
  }
  return map;
}
