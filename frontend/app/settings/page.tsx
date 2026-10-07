"use client";

import { Sidebar } from "@/components/layout/sidebar";
import { TopStatusBar } from "@/components/dashboard/top-status-bar";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Settings as SettingsIcon } from "lucide-react";

const configItems = [
  { label: "Mode", value: "Dry Run", color: "text-warning" },
  { label: "Symbol", value: "BTCUSDT", color: "text-foreground" },
  { label: "Interval", value: "1h", color: "text-foreground" },
  { label: "AI Model", value: "Llama 3 (Ollama)", color: "text-foreground" },
  { label: "Evaluation Cycle", value: "Every 5 minutes", color: "text-foreground" },
];

export default function SettingsPage() {
  return (
    <div className="flex min-h-screen bg-background">
      <Sidebar />
      <main className="flex-1 pl-14">
        <TopStatusBar pageLabel="Settings" />
        <div className="space-y-4 p-5">
          <Card>
            <CardHeader className="flex flex-row items-center gap-2 space-y-0">
              <div className="flex h-6 w-6 items-center justify-center rounded-lg bg-muted">
                <SettingsIcon className="h-3.5 w-3.5 text-muted-foreground" />
              </div>
              <CardTitle>Bot Configuration</CardTitle>
            </CardHeader>
            <CardContent>
              <div className="space-y-0.5">
                {configItems.map((item) => (
                  <div
                    key={item.label}
                    className="flex items-center justify-between rounded-lg px-3 py-2.5 transition-colors hover:bg-muted/50"
                  >
                    <span className="text-sm text-muted-foreground">{item.label}</span>
                    <span className={`text-sm font-medium font-mono ${item.color}`}>
                      {item.value}
                    </span>
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
