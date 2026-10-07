"use client";

import { useMemo, useState } from "react";
import { useTheme } from "next-themes";
import {
  ResponsiveContainer,
  LineChart,
  Line,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  Legend,
} from "recharts";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { Skeleton } from "@/components/ui/skeleton";
import { AVAILABLE_COINS, type HistoryPoint, type TimeRange } from "@/lib/api";
import { getChartColors } from "@/lib/chart-theme";

type ChartMode = "absolute" | "percent";

const TIME_RANGES: { value: TimeRange; label: string }[] = [
  { value: "1H", label: "1H" },
  { value: "4H", label: "4H" },
  { value: "24H", label: "24H" },
  { value: "7D", label: "7D" },
  { value: "30D", label: "30D" },
  { value: "90D", label: "90D" },
  { value: "1Y", label: "1Y" },
  { value: "ALL", label: "ALL" },
];


interface MultiLineChartProps {
  points: HistoryPoint[];
  symbols: string[];
  isLoading: boolean;
  timeRange?: TimeRange;
  onTimeRangeChange?: (r: TimeRange) => void;
}

interface MergedDataPoint {
  time: string;
  timestamp: number;
  [symbol: string]: string | number;
}

const PRICE_SPREAD_RATIO = 50;

export function MultiLineChart({
  points,
  symbols,
  isLoading,
  timeRange,
  onTimeRangeChange,
}: MultiLineChartProps) {
  const { resolvedTheme } = useTheme();
  const isDark = resolvedTheme === "dark";
  const cc = getChartColors(isDark);

  const [userMode, setUserMode] = useState<ChartMode>("absolute");

  const forcePercent = useMemo(() => {
    if (symbols.length < 2) return false;
    const lastPrice: Record<string, number> = {};
    for (const p of points) {
      lastPrice[p.symbol] = parseFloat(p.close);
    }
    const vals = Object.values(lastPrice).filter((v) => v > 0);
    if (vals.length < 2) return false;
    return Math.max(...vals) / Math.min(...vals) > PRICE_SPREAD_RATIO;
  }, [points, symbols.length]);

  const mode: ChartMode = forcePercent ? "percent" : userMode;

  const data = useMemo(() => {
    const sortedPoints = [...points].sort(
      (a, b) => new Date(a.time).getTime() - new Date(b.time).getTime()
    );

    const timeMap = new Map<string, MergedDataPoint>();
    const firsts: Record<string, number> = {};

    for (const p of sortedPoints) {
      const price = parseFloat(p.close);
      if (!Number.isFinite(price)) continue;

      if (firsts[p.symbol] === undefined) firsts[p.symbol] = price;

      const timeKey = p.time;
      if (!timeMap.has(timeKey)) {
        timeMap.set(timeKey, {
          time: new Date(timeKey).toLocaleString("en-US", {
            month: "short",
            day: "numeric",
            hour: "2-digit",
            minute: "2-digit",
            hour12: false,
          }),
          timestamp: new Date(timeKey).getTime(),
        });
      }
      const entry = timeMap.get(timeKey)!;

      entry[p.symbol] =
        mode === "percent" && firsts[p.symbol] > 0
          ? ((price - firsts[p.symbol]) / firsts[p.symbol]) * 100
          : price;
    }

    return Array.from(timeMap.values()).sort(
      (a, b) => a.timestamp - b.timestamp
    );
  }, [points, mode]);

  const coinColorMap = Object.fromEntries(
    AVAILABLE_COINS.map((c) => [c.symbol, c.color])
  );

  if (isLoading) {
    return (
      <Card className="col-span-full">
        <CardHeader>
          <Skeleton className="h-5 w-44" />
        </CardHeader>
        <CardContent>
          <Skeleton className="h-[400px] w-full rounded-lg" />
        </CardContent>
      </Card>
    );
  }

  if (symbols.length === 0) {
    return (
      <Card className="col-span-full">
        <CardHeader>
          <CardTitle>Price Chart</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="flex h-[400px] items-center justify-center text-sm text-muted-foreground">
            Select at least one coin to view the chart
          </div>
        </CardContent>
      </Card>
    );
  }

  return (
    <Card className="col-span-full">
      <CardHeader className="flex flex-col gap-3 space-y-0 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex flex-col gap-1">
          <CardTitle>
            {mode === "absolute" ? "Price Chart" : "Performance Comparison"}
          </CardTitle>
          {forcePercent && (
            <span className="text-micro text-muted-foreground">
              Auto-switched to % (price scales differ &gt;{PRICE_SPREAD_RATIO}x)
            </span>
          )}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {timeRange && onTimeRangeChange && (
            <div className="flex items-center gap-0.5 rounded-lg bg-ov-2 p-0.5">
              {TIME_RANGES.map((r) => (
                <button
                  key={r.value}
                  type="button"
                  onClick={() => onTimeRangeChange(r.value)}
                  className={
                    "rounded-md px-2 py-1 text-micro font-medium transition-all duration-150 " +
                    (r.value === timeRange
                      ? "bg-ov-4 text-foreground shadow-sm"
                      : "text-muted-foreground hover:bg-ov-3 hover:text-foreground")
                  }
                >
                  {r.label}
                </button>
              ))}
            </div>
          )}
          <ToggleGroup
            type="single"
            value={mode}
            onValueChange={(v) => {
              if (v && !forcePercent) setUserMode(v as ChartMode);
            }}
            disabled={forcePercent}
          >
            <ToggleGroupItem value="absolute" disabled={forcePercent}>
              Price
            </ToggleGroupItem>
            <ToggleGroupItem value="percent">%</ToggleGroupItem>
          </ToggleGroup>
        </div>
      </CardHeader>
      <CardContent>
        <div className="h-[400px] w-full">
          <ResponsiveContainer width="100%" height="100%">
            <LineChart data={data}>
              <defs>
                {symbols.map((symbol) => {
                  const color = coinColorMap[symbol] || "#888";
                  return (
                    <linearGradient key={`grad-${symbol}`} id={`line-grad-${symbol}`} x1="0" y1="0" x2="0" y2="1">
                      <stop offset="0%" stopColor={color} stopOpacity={0.15} />
                      <stop offset="100%" stopColor={color} stopOpacity={0} />
                    </linearGradient>
                  );
                })}
              </defs>
              <CartesianGrid strokeDasharray="2 4" stroke={cc.grid} vertical={false} />
              <XAxis
                dataKey="time"
                tick={{ fill: cc.axisText, fontSize: 10, fontFamily: "var(--font-mono), monospace" }}
                tickLine={false}
                axisLine={{ stroke: cc.axisLine }}
                interval="preserveStartEnd"
                minTickGap={50}
              />
              <YAxis
                tick={{ fill: cc.axisText, fontSize: 10, fontFamily: "var(--font-mono), monospace" }}
                tickLine={false}
                axisLine={false}
                domain={["auto", "auto"]}
                tickFormatter={(v) =>
                  mode === "percent"
                    ? `${(v as number).toFixed(1)}%`
                    : `$${(v as number).toLocaleString()}`
                }
              />
              <Tooltip
                content={({ active, payload, label }) => {
                  if (!active || !payload || payload.length === 0) return null;
                  const sorted = [...payload].sort(
                    (a, b) => (Number(b.value) || 0) - (Number(a.value) || 0)
                  );
                  return (
                    <div
                      className="rounded-xl px-3.5 py-2.5 text-xs shadow-2xl"
                      style={{
                        background: cc.tooltipBg,
                        border: `1px solid ${cc.tooltipBorder}`,
                        backdropFilter: "blur(12px)",
                      }}
                    >
                      <div className="mb-1.5 text-micro text-muted-foreground font-mono">{label}</div>
                      <div className="space-y-1">
                        {sorted.map((item) => {
                          const coin = AVAILABLE_COINS.find((c) => c.symbol === item.dataKey);
                          const val = Number(item.value);
                          return (
                            <div key={item.dataKey} className="flex items-center justify-between gap-4">
                              <div className="flex items-center gap-1.5">
                                <span
                                  className="h-2 w-2 rounded-full"
                                  style={{ backgroundColor: item.color }}
                                />
                                <span className="text-muted-foreground">{coin?.label || item.dataKey}</span>
                              </div>
                              <span className="font-mono font-medium text-foreground/80 tabular-nums">
                                {mode === "percent"
                                  ? `${val >= 0 ? "+" : ""}${val.toFixed(2)}%`
                                  : `$${val.toLocaleString()}`}
                              </span>
                            </div>
                          );
                        })}
                      </div>
                    </div>
                  );
                }}
              />
              <Legend
                content={({ payload }) => {
                  if (!payload) return null;
                  return (
                    <div className="mt-3 flex flex-wrap items-center justify-center gap-4">
                      {payload.map((entry) => {
                        const coin = AVAILABLE_COINS.find((c) => c.symbol === entry.value);
                        return (
                          <div key={entry.value} className="flex items-center gap-1.5">
                            <span
                              className="h-2 w-2 rounded-full"
                              style={{ backgroundColor: entry.color }}
                            />
                            <span className="text-micro text-muted-foreground">{coin?.label || entry.value}</span>
                          </div>
                        );
                      })}
                    </div>
                  );
                }}
              />
              {symbols.map((symbol) => (
                <Line
                  key={symbol}
                  type="monotone"
                  dataKey={symbol}
                  stroke={coinColorMap[symbol] || "#888"}
                  strokeWidth={1.8}
                  dot={false}
                  connectNulls
                  isAnimationActive={false}
                />
              ))}
            </LineChart>
          </ResponsiveContainer>
        </div>
      </CardContent>
    </Card>
  );
}
