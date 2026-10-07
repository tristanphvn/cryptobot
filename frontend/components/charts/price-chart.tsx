"use client";

import React, { useEffect, useMemo, useRef, useState } from "react";
import {
  ResponsiveContainer,
  ComposedChart,
  Bar,
  Line,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ReferenceDot,
  ReferenceLine,
} from "recharts";
import { useTheme } from "next-themes";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { getChartColors, type ChartColors } from "@/lib/chart-theme";
import type { Candle, Trade } from "@/lib/api";

const TIMEFRAMES = ["1m", "5m", "15m", "1h", "4h", "1d"] as const;
type Timeframe = (typeof TIMEFRAMES)[number];

const VISIBLE_BARS = 80;

function formatBucket(openTime: string, interval: string): string {
  const d = new Date(openTime);
  if (isNaN(d.getTime())) return "";
  if (interval === "1m" || interval === "5m" || interval === "15m") {
    const ageH = (Date.now() - d.getTime()) / 3_600_000;
    const t = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
    if (ageH > 24) {
      return `${d.toLocaleDateString([], { day: "2-digit", month: "2-digit" })} ${t}`;
    }
    return t;
  }
  if (interval === "1h" || interval === "4h") {
    return d.toLocaleString([], {
      day: "2-digit",
      month: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      hour12: false,
    });
  }
  return d.toLocaleDateString([], { day: "2-digit", month: "short", year: "numeric" });
}

function formatFull(openTime: string): string {
  const d = new Date(openTime);
  if (isNaN(d.getTime())) return openTime;
  return d.toLocaleString([], {
    day: "2-digit",
    month: "short",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

// ─── Types ─────────────────────────────────────────────────────────
interface PriceChartProps {
  candles: Candle[];
  trades: Trade[];
  symbol?: string;
  interval?: string;
  onIntervalChange?: (i: string) => void;
}

interface ChartDatum {
  bucket: string;
  time: string;
  open: number;
  high: number;
  low: number;
  close: number;
  volume: number;
  range: [number, number];
  isUp: boolean;
  ema20: number | null;
  ema50: number | null;
  isPhantom: boolean;
}

interface RsiDatum {
  bucket: string;
  time: string;
  rsi: number | null;
}

// ─── Indicator math ────────────────────────────────────────────────
function computeEMA(values: number[], period: number): (number | null)[] {
  const out: (number | null)[] = new Array(values.length).fill(null);
  if (values.length < period) return out;
  const k = 2 / (period + 1);
  let sum = 0;
  for (let i = 0; i < period; i++) sum += values[i];
  let ema = sum / period;
  out[period - 1] = ema;
  for (let i = period; i < values.length; i++) {
    ema = (values[i] - ema) * k + ema;
    out[i] = ema;
  }
  return out;
}

function computeRSI(closes: number[], period = 14): (number | null)[] {
  const out: (number | null)[] = new Array(closes.length).fill(null);
  if (closes.length <= period) return out;
  let gainSum = 0;
  let lossSum = 0;
  for (let i = 1; i <= period; i++) {
    const diff = closes[i] - closes[i - 1];
    if (diff >= 0) gainSum += diff;
    else lossSum -= diff;
  }
  let avgGain = gainSum / period;
  let avgLoss = lossSum / period;
  out[period] =
    avgLoss === 0 ? 100 : 100 - 100 / (1 + avgGain / avgLoss);
  for (let i = period + 1; i < closes.length; i++) {
    const diff = closes[i] - closes[i - 1];
    const gain = diff > 0 ? diff : 0;
    const loss = diff < 0 ? -diff : 0;
    avgGain = (avgGain * (period - 1) + gain) / period;
    avgLoss = (avgLoss * (period - 1) + loss) / period;
    out[i] = avgLoss === 0 ? 100 : 100 - 100 / (1 + avgGain / avgLoss);
  }
  return out;
}

// ─── Candle + volume shapes ────────────────────────────────────────
type ShapeProps = {
  x?: number;
  y?: number;
  width?: number;
  height?: number;
  payload?: ChartDatum;
  cc?: ChartColors;
};

function CandleShape({ x = 0, y = 0, width = 0, height = 0, payload, cc }: ShapeProps) {
  if (!payload || !cc) return null;
  if (payload.isPhantom) return null;
  const { open, high, low, close, isUp } = payload;
  const bodyColor = isUp ? cc.upCandle : cc.downCandle;
  const wickColor = isUp ? cc.up : cc.down;
  const wickX = x + width / 2;
  const range = high - low || 1;
  const pxPerPrice = height / range;
  const openY = y + (high - open) * pxPerPrice;
  const closeY = y + (high - close) * pxPerPrice;
  const bodyTop = Math.min(openY, closeY);
  const bodyHeight = Math.max(Math.abs(closeY - openY), 1);
  const bodyW = Math.max(width * 0.55, 4);
  const bodyX = x + (width - bodyW) / 2;
  return (
    <g>
      <line x1={wickX} x2={wickX} y1={y} y2={y + height} stroke={wickColor} strokeWidth={1} strokeOpacity={0.7} shapeRendering="crispEdges" />
      <rect x={bodyX} y={bodyTop} width={bodyW} height={bodyHeight} fill={bodyColor} rx={1} shapeRendering="crispEdges" />
    </g>
  );
}

function VolumeShape({ x = 0, y = 0, width = 0, height = 0, payload, cc }: ShapeProps) {
  if (!payload || !cc) return null;
  if (payload.isPhantom) return null;
  const color = payload.isUp ? cc.up : cc.down;
  const w = Math.max(width * 0.55, 4);
  return (
    <rect x={x + (width - w) / 2} y={y} width={w} height={height} fill={color} opacity={0.15} rx={1} shapeRendering="crispEdges" />
  );
}

const TAG_W = 68;
const TAG_H = 20;
function PriceTag({
  viewBox,
  value,
  fill,
}: {
  viewBox?: { x?: number; y?: number; width?: number; height?: number };
  value: number;
  fill: string;
}) {
  if (!viewBox) return null;
  const plotRight = (viewBox.x ?? 0) + (viewBox.width ?? 0);
  const cy = viewBox.y ?? 0;
  const text = `$${value.toLocaleString(undefined, { maximumFractionDigits: 2 })}`;
  const x = plotRight + 4;
  const y = cy - TAG_H / 2;
  return (
    <g pointerEvents="none">
      <rect x={x} y={y} width={TAG_W} height={TAG_H} rx={4} ry={4} fill={fill} />
      <text
        x={x + TAG_W / 2}
        y={cy}
        textAnchor="middle"
        dominantBaseline="central"
        fontSize={10}
        fontWeight={600}
        fontFamily="var(--font-mono), monospace"
        fill="hsl(var(--primary-foreground))"
        style={{ fontVariantNumeric: "tabular-nums" }}
      >
        {text}
      </text>
    </g>
  );
}

// ─── UI sub-components ─────────────────────────────────────────────
function Pill({
  active,
  onClick,
  children,
  title,
}: {
  active?: boolean;
  onClick?: () => void;
  children: React.ReactNode;
  title?: string;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      title={title}
      className={cn(
        "rounded-md px-2 py-1 text-micro font-medium transition-all duration-150",
        active
          ? "bg-ov-4 text-foreground shadow-sm"
          : "text-muted-foreground hover:bg-ov-2 hover:text-foreground"
      )}
    >
      {children}
    </button>
  );
}

function HeaderTicker({
  symbol,
  data,
  flash,
  cc,
}: {
  symbol?: string;
  data: ChartDatum[];
  flash: boolean;
  cc: ChartColors;
}) {
  const last = data[data.length - 1];
  const first = data[0];
  if (!last || !first) {
    return (
      <div className="text-sm font-medium text-foreground">
        {symbol ?? "BTC/USDT"}
      </div>
    );
  }
  const change = last.close - first.open;
  const changePct = first.open ? (change / first.open) * 100 : 0;
  const isUp = change >= 0;
  const color = isUp ? cc.up : cc.down;
  const fmt = (n: number) => n.toLocaleString(undefined, { maximumFractionDigits: 2 });

  let lo = Infinity;
  let hi = -Infinity;
  let vol = 0;
  for (const d of data) {
    if (d.low < lo) lo = d.low;
    if (d.high > hi) hi = d.high;
    vol += d.volume;
  }

  return (
    <div className="flex flex-wrap items-baseline gap-x-5 gap-y-1">
      <div className="text-sm font-semibold text-foreground">{symbol ?? "BTC/USDT"}</div>
      <div
        className={cn(
          "text-2xl font-bold tabular-nums font-mono transition-all duration-300",
          flash && "scale-[1.03]"
        )}
        style={{ color }}
      >
        ${fmt(last.close)}
      </div>
      <div className="text-xs tabular-nums font-mono" style={{ color }}>
        {isUp ? "+" : ""}
        {fmt(change)} ({isUp ? "+" : ""}
        {changePct.toFixed(2)}%)
      </div>
      <div className="hidden gap-4 text-micro text-muted-foreground sm:flex font-mono">
        <span><span className="opacity-60">H </span><span className="tabular-nums text-foreground/70">${fmt(hi)}</span></span>
        <span><span className="opacity-60">L </span><span className="tabular-nums text-foreground/70">${fmt(lo)}</span></span>
        <span><span className="opacity-60">Vol </span><span className="tabular-nums text-foreground/70">{fmt(vol)}</span></span>
      </div>
    </div>
  );
}

// ─── Main component ────────────────────────────────────────────────
export const PriceChart = React.memo(function PriceChart({
  candles,
  trades,
  symbol,
  interval = "1m",
  onIntervalChange,
}: PriceChartProps) {
  const { resolvedTheme } = useTheme();
  const isDark = resolvedTheme === "dark";
  const cc = getChartColors(isDark);

  const [showLine, setShowLine] = useState(true);
  const [showVolume, setShowVolume] = useState(true);
  const [showEma20, setShowEma20] = useState(true);
  const [showEma50, setShowEma50] = useState(true);
  const [showRsi, setShowRsi] = useState(true);

  const [flash, setFlash] = useState(false);
  const prevCloseRef = useRef<number | null>(null);

  const cardRef = useRef<HTMLDivElement | null>(null);
  const [isFs, setIsFs] = useState(false);
  useEffect(() => {
    const onFsChange = () => setIsFs(!!document.fullscreenElement);
    document.addEventListener("fullscreenchange", onFsChange);
    return () => document.removeEventListener("fullscreenchange", onFsChange);
  }, []);
  const toggleFullscreen = () => {
    if (!cardRef.current) return;
    if (document.fullscreenElement) {
      document.exitFullscreen?.();
    } else {
      cardRef.current.requestFullscreen?.();
    }
  };

  const [hoverIdx, setHoverIdx] = useState<number | null>(null);

  // ─── Derived data ────────────────────────────────────────────────
  const closes = useMemo(() => candles.map((c) => parseFloat(c.close)), [candles]);
  const ema20 = useMemo(() => computeEMA(closes, 20), [closes]);
  const ema50 = useMemo(() => computeEMA(closes, 50), [closes]);
  const rsi = useMemo(() => computeRSI(closes, 14), [closes]);

  const data: ChartDatum[] = useMemo(
    () => {
      const full = candles.map((c, i) => {
        const open = parseFloat(c.open);
        const high = parseFloat(c.high);
        const low = parseFloat(c.low);
        const close = parseFloat(c.close);
        return {
          bucket: c.open_time,
          time: formatFull(c.open_time),
          open,
          high,
          low,
          close,
          volume: parseFloat(c.volume),
          range: [low, high] as [number, number],
          isUp: close >= open,
          ema20: ema20[i],
          ema50: ema50[i],
          isPhantom: c.id === -1,
        };
      });
      return full.length > VISIBLE_BARS
        ? full.slice(full.length - VISIBLE_BARS)
        : full;
    },
    [candles, ema20, ema50]
  );

  const rsiData: RsiDatum[] = useMemo(
    () => {
      const offset = candles.length > VISIBLE_BARS ? candles.length - VISIBLE_BARS : 0;
      return data.map((d, i) => ({ bucket: d.bucket, time: d.time, rsi: rsi[offset + i] }));
    },
    [data, rsi, candles.length]
  );

  const tradeMarkers = useMemo(() => {
    if (data.length === 0) return [];
    const bucketsMs = data.map((d) => new Date(d.bucket).getTime());
    return trades
      .filter((t) => t.status === "CLOSED" || t.status === "OPEN")
      .map((t) => {
        const tMs = new Date(t.entry_at).getTime();
        let idx = -1;
        for (let i = bucketsMs.length - 1; i >= 0; i--) {
          if (bucketsMs[i] <= tMs) {
            idx = i;
            break;
          }
        }
        if (idx < 0) return null;
        return {
          bucket: data[idx].bucket,
          price: parseFloat(t.entry_price),
          side: t.side,
        };
      })
      .filter(Boolean) as { bucket: string; price: number; side: string }[];
  }, [trades, data]);

  const priceDomain = useMemo<[number | string, number | string]>(() => {
    if (data.length === 0) return ["auto", "auto"];
    let lo = Infinity;
    let hi = -Infinity;
    for (const d of data) {
      if (d.low < lo) lo = d.low;
      if (d.high > hi) hi = d.high;
    }
    const pad = (hi - lo) * 0.08 || hi * 0.001;
    return [lo - pad, hi + pad];
  }, [data]);

  const lastClose = data.length ? data[data.length - 1].close : null;
  const lastIsUp = data.length ? data[data.length - 1].isUp : true;

  useEffect(() => {
    if (lastClose === null) return;
    if (prevCloseRef.current !== null && prevCloseRef.current !== lastClose) {
      setFlash(true);
      const t = setTimeout(() => setFlash(false), 400);
      return () => clearTimeout(t);
    }
    prevCloseRef.current = lastClose;
  }, [lastClose]);

  // ─── Render ──────────────────────────────────────────────────────
  const hoveredClose = hoverIdx !== null ? data[hoverIdx]?.close ?? null : null;

  return (
    <Card
      ref={cardRef}
      className={cn(
        "col-span-full overflow-hidden",
        isFs && "fixed inset-0 z-50 rounded-none"
      )}
    >
      <CardHeader className="space-y-3 border-b border-border px-5 py-3">
        <div className="flex items-start justify-between gap-2">
          <HeaderTicker symbol={symbol} data={data} flash={flash} cc={cc} />
          <button
            type="button"
            onClick={toggleFullscreen}
            title={isFs ? "Exit fullscreen" : "Fullscreen"}
            className="rounded-lg p-1.5 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
          >
            {isFs ? (
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M8 3v4a1 1 0 0 1-1 1H3"/><path d="M21 8h-4a1 1 0 0 1-1-1V3"/><path d="M3 16h4a1 1 0 0 1 1 1v4"/><path d="M16 21v-4a1 1 0 0 1 1-1h4"/></svg>
            ) : (
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M3 8V5a2 2 0 0 1 2-2h3"/><path d="M21 8V5a2 2 0 0 0-2-2h-3"/><path d="M3 16v3a2 2 0 0 0 2 2h3"/><path d="M21 16v3a2 2 0 0 1-2 2h-3"/></svg>
            )}
          </button>
        </div>

        {/* Toolbar: timeframes + indicator toggles */}
        <div className="flex flex-wrap items-center gap-3">
          {onIntervalChange && (
            <div className="flex items-center gap-0.5 rounded-lg bg-ov-2 p-0.5">
              {TIMEFRAMES.map((tf) => (
                <Pill key={tf} active={tf === interval} onClick={() => onIntervalChange(tf)}>
                  {tf.toUpperCase()}
                </Pill>
              ))}
            </div>
          )}
          <div className="h-4 w-px bg-border" />
          <div className="flex flex-wrap items-center gap-0.5">
            <Pill active={showLine} onClick={() => setShowLine((v) => !v)} title="Toggle close line">
              <span className="inline-block h-1.5 w-1.5 rounded-full mr-1" style={{ backgroundColor: showLine ? cc.line : "currentColor" }} />Line
            </Pill>
            <Pill active={showEma20} onClick={() => setShowEma20((v) => !v)} title="Toggle EMA 20">
              <span className="inline-block h-1.5 w-1.5 rounded-full mr-1" style={{ backgroundColor: showEma20 ? cc.ema20 : "currentColor" }} />EMA 20
            </Pill>
            <Pill active={showEma50} onClick={() => setShowEma50((v) => !v)} title="Toggle EMA 50">
              <span className="inline-block h-1.5 w-1.5 rounded-full mr-1" style={{ backgroundColor: showEma50 ? cc.ema50 : "currentColor" }} />EMA 50
            </Pill>
            <Pill active={showVolume} onClick={() => setShowVolume((v) => !v)} title="Toggle volume">
              Vol
            </Pill>
            <Pill active={showRsi} onClick={() => setShowRsi((v) => !v)} title="Toggle RSI panel">
              RSI
            </Pill>
          </div>
        </div>
      </CardHeader>

      <CardContent className="p-0">
        {/* ─── Main chart ─── */}
        <div className={cn("w-full", isFs ? "h-[calc(70vh)]" : "h-[440px]")}>
          <ResponsiveContainer width="100%" height="100%">
            <ComposedChart
              data={data}
              margin={{ top: 14, right: 72, left: 0, bottom: 6 }}
              barCategoryGap="50%"
              onMouseMove={(s) => {
                if (typeof s?.activeTooltipIndex === "number") setHoverIdx(s.activeTooltipIndex);
              }}
              onMouseLeave={() => setHoverIdx(null)}
            >
              <defs>
                <linearGradient id="priceGlow" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0%" stopColor={cc.line} stopOpacity={0.12} />
                  <stop offset="100%" stopColor={cc.line} stopOpacity={0} />
                </linearGradient>
              </defs>

              <CartesianGrid strokeDasharray="2 4" stroke={cc.grid} vertical={false} />
              <XAxis
                dataKey="bucket"
                tick={{ fill: cc.axisText, fontSize: 10, fontFamily: "var(--font-mono), monospace" }}
                tickLine={false}
                axisLine={{ stroke: cc.axisLine }}
                minTickGap={40}
                tickFormatter={(v) => formatBucket(v as string, interval)}
              />
              <YAxis
                yAxisId="price"
                orientation="right"
                domain={priceDomain}
                tick={{ fill: cc.axisText, fontSize: 10, fontFamily: "var(--font-mono), monospace" }}
                tickLine={false}
                axisLine={false}
                width={72}
                tickFormatter={(v) => `$${(v as number).toLocaleString(undefined, { maximumFractionDigits: 2 })}`}
              />
              <YAxis yAxisId="volume" orientation="left" domain={[0, (m: number) => m * 4]} hide />

              <Tooltip
                cursor={{ stroke: cc.cursor, strokeWidth: 1 }}
                content={({ active, payload }) => {
                  if (!active || !payload || payload.length === 0) return null;
                  const d = payload[0].payload as ChartDatum;
                  const fmt = (n: number) => `$${n.toLocaleString(undefined, { maximumFractionDigits: 2 })}`;
                  const change = d.close - d.open;
                  const changePct = d.open ? (change / d.open) * 100 : 0;
                  const color = d.isUp ? cc.up : cc.down;
                  return (
                    <div
                      className="rounded-xl px-3.5 py-2.5 text-xs shadow-2xl"
                      style={{
                        background: cc.tooltipBg,
                        border: `1px solid ${cc.tooltipBorder}`,
                        backdropFilter: "blur(12px)",
                      }}
                    >
                      <div className="mb-1.5 text-micro text-muted-foreground font-mono">{d.time}</div>
                      <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 tabular-nums font-mono text-foreground/80">
                        <span className="text-muted-foreground">O</span><span>{fmt(d.open)}</span>
                        <span className="text-muted-foreground">H</span><span>{fmt(d.high)}</span>
                        <span className="text-muted-foreground">L</span><span>{fmt(d.low)}</span>
                        <span className="text-muted-foreground">C</span><span style={{ color }}>{fmt(d.close)}</span>
                        <span className="text-muted-foreground">&Delta;</span>
                        <span style={{ color }}>
                          {change >= 0 ? "+" : ""}{fmt(change)} ({changePct >= 0 ? "+" : ""}{changePct.toFixed(2)}%)
                        </span>
                        {d.ema20 !== null && (<><span style={{ color: cc.ema20 }}>E20</span><span>{fmt(d.ema20)}</span></>)}
                        {d.ema50 !== null && (<><span style={{ color: cc.ema50 }}>E50</span><span>{fmt(d.ema50)}</span></>)}
                        <span className="text-muted-foreground">Vol</span><span>{d.volume.toLocaleString(undefined, { maximumFractionDigits: 2 })}</span>
                      </div>
                    </div>
                  );
                }}
              />

              {/* Volume bars */}
              {showVolume && (
                <Bar yAxisId="volume" dataKey="volume" shape={<VolumeShape cc={cc} />} isAnimationActive={false} />
              )}

              {/* Candles */}
              <Bar yAxisId="price" dataKey="range" shape={<CandleShape cc={cc} />} isAnimationActive={false} />

              {/* Close line */}
              {showLine && (
                <Line
                  yAxisId="price"
                  type="monotone"
                  dataKey="close"
                  stroke={cc.line}
                  strokeWidth={1.2}
                  strokeOpacity={0.8}
                  dot={false}
                  activeDot={{ r: 3.5, fill: cc.line, stroke: cc.cardBg, strokeWidth: 2 }}
                  isAnimationActive={false}
                />
              )}

              {/* EMAs */}
              {showEma20 && (
                <Line yAxisId="price" type="monotone" dataKey="ema20" stroke={cc.ema20} strokeWidth={1.2} strokeOpacity={0.85} dot={false} isAnimationActive={false} connectNulls />
              )}
              {showEma50 && (
                <Line yAxisId="price" type="monotone" dataKey="ema50" stroke={cc.ema50} strokeWidth={1.2} strokeOpacity={0.85} dot={false} isAnimationActive={false} connectNulls />
              )}

              {/* Horizontal crosshair snapped to hovered candle's close */}
              {hoveredClose !== null && hoverIdx !== null && hoverIdx !== data.length - 1 && (
                <ReferenceLine
                  yAxisId="price"
                  y={hoveredClose}
                  stroke={cc.cursor}
                  strokeDasharray="3 3"
                  ifOverflow="extendDomain"
                  label={<PriceTag value={hoveredClose} fill={cc.cursor} />}
                />
              )}

              {/* Last-price reference line + pill */}
              {lastClose !== null && (
                <ReferenceLine
                  yAxisId="price"
                  y={lastClose}
                  stroke={lastIsUp ? cc.up : cc.down}
                  strokeDasharray="4 4"
                  strokeOpacity={flash ? 1 : 0.6}
                  ifOverflow="extendDomain"
                  label={<PriceTag value={lastClose} fill={lastIsUp ? cc.upCandle : cc.downCandle} />}
                />
              )}

              {/* Trade markers */}
              {tradeMarkers.map((m, i) => (
                <ReferenceDot
                  key={i}
                  yAxisId="price"
                  x={m.bucket}
                  y={m.price}
                  r={4}
                  fill={m.side === "BUY" ? cc.up : cc.down}
                  stroke={cc.cardBg}
                  strokeWidth={2}
                />
              ))}
            </ComposedChart>
          </ResponsiveContainer>
        </div>

        {/* ─── RSI panel ─── */}
        {showRsi && (
          <div className={cn("w-full border-t border-border", isFs ? "h-[18vh]" : "h-[120px]")}>
            <div className="flex items-center justify-between px-4 pt-2 text-micro uppercase tracking-wider text-muted-foreground">
              <span className="font-medium">RSI 14</span>
              <span className="tabular-nums font-mono">
                {(() => {
                  const v = rsi[rsi.length - 1];
                  if (v === null || v === undefined) return "—";
                  const c = v > 70 ? cc.down : v < 30 ? cc.up : cc.axisText;
                  return <span style={{ color: c }}>{v.toFixed(1)}</span>;
                })()}
              </span>
            </div>
            <ResponsiveContainer width="100%" height="80%">
              <ComposedChart
                data={rsiData}
                margin={{ top: 4, right: 72, left: 0, bottom: 4 }}
                syncId={undefined}
              >
                <CartesianGrid strokeDasharray="2 4" stroke={cc.grid} vertical={false} />
                <XAxis dataKey="bucket" hide />
                <YAxis
                  domain={[0, 100]}
                  ticks={[30, 50, 70]}
                  orientation="right"
                  tick={{ fill: cc.axisText, fontSize: 10, fontFamily: "var(--font-mono), monospace" }}
                  tickLine={false}
                  axisLine={false}
                  width={72}
                />
                <ReferenceLine y={70} stroke={cc.down} strokeDasharray="2 4" strokeOpacity={0.4} />
                <ReferenceLine y={30} stroke={cc.up} strokeDasharray="2 4" strokeOpacity={0.4} />
                <ReferenceLine y={50} stroke={cc.subtleLine} />
                <Line
                  type="monotone"
                  dataKey="rsi"
                  stroke={cc.rsi}
                  strokeWidth={1.3}
                  dot={false}
                  isAnimationActive={false}
                  connectNulls
                />
                <Tooltip
                  cursor={{ stroke: cc.cursor, strokeWidth: 1 }}
                  content={({ active, payload }) => {
                    if (!active || !payload || payload.length === 0) return null;
                    const v = payload[0].value as number | null;
                    const d = payload[0].payload as RsiDatum;
                    if (v === null) return null;
                    const c = v > 70 ? cc.down : v < 30 ? cc.up : cc.axisText;
                    return (
                      <div
                        className="rounded-lg px-2.5 py-1.5 text-xs shadow-xl"
                        style={{
                          background: cc.tooltipBg,
                          border: `1px solid ${cc.tooltipBorder}`,
                          backdropFilter: "blur(12px)",
                        }}
                      >
                        <div className="text-micro text-muted-foreground font-mono">{d.time}</div>
                        <div className="tabular-nums font-mono font-semibold" style={{ color: c }}>RSI {v.toFixed(2)}</div>
                      </div>
                    );
                  }}
                />
              </ComposedChart>
            </ResponsiveContainer>
          </div>
        )}
      </CardContent>
    </Card>
  );
});
