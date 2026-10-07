/**
 * Chart color tokens resolved from CSS custom properties (globals.css).
 *
 * Source of truth: globals.css :root / .dark variables.
 * Recharts can't read CSS vars directly, so we resolve them via getComputedStyle
 * at runtime. SSR/static builds fall back to the DARK/LIGHT maps below, which
 * MUST mirror globals.css — update both when changing theme values.
 */

export interface ChartColors {
  /* Structural */
  grid: string;
  axisLine: string;
  axisText: string;
  tooltipBg: string;
  tooltipBorder: string;
  cursor: string;
  subtleLine: string;
  cardBg: string;
  /* Indicators */
  up: string;
  down: string;
  upCandle: string;
  downCandle: string;
  line: string;
  ema20: string;
  ema50: string;
  rsi: string;
}

function hsl(raw: string): string {
  return raw ? `hsl(${raw})` : "";
}

export function getChartColors(isDark: boolean): ChartColors {
  if (typeof window !== "undefined") {
    const s = getComputedStyle(document.documentElement);
    const v = (prop: string) => hsl(s.getPropertyValue(prop).trim());
    return {
      grid: v("--chart-grid"),
      axisLine: v("--chart-axis-line"),
      axisText: v("--chart-axis-text"),
      tooltipBg: v("--chart-tooltip-bg"),
      tooltipBorder: v("--chart-tooltip-border"),
      cursor: v("--chart-cursor"),
      subtleLine: v("--chart-subtle-line"),
      cardBg: v("--card"),
      up: v("--chart-up"),
      down: v("--chart-down"),
      upCandle: v("--chart-up-candle"),
      downCandle: v("--chart-down-candle"),
      line: v("--chart-line"),
      ema20: v("--chart-ema20"),
      ema50: v("--chart-ema50"),
      rsi: v("--chart-rsi"),
    };
  }
  return isDark ? DARK : LIGHT;
}

// ── SSR fallbacks (must mirror globals.css) ──────────────────────────

const DARK: ChartColors = {
  grid: "hsl(220 15% 10%)",
  axisLine: "hsl(220 15% 12%)",
  axisText: "hsl(215 15% 40%)",
  tooltipBg: "hsl(225 30% 5%)",
  tooltipBorder: "hsl(220 15% 15%)",
  cursor: "hsl(220 15% 25%)",
  subtleLine: "hsl(220 15% 12%)",
  cardBg: "hsl(225 25% 7%)",
  up: "hsl(142 71% 45%)",
  down: "hsl(0 84% 60%)",
  upCandle: "hsl(142 76% 36%)",
  downCandle: "hsl(0 72% 51%)",
  line: "hsl(48 96% 53%)",
  ema20: "hsl(217 91% 60%)",
  ema50: "hsl(263 70% 50%)",
  rsi: "hsl(292 84% 61%)",
};

const LIGHT: ChartColors = {
  grid: "hsl(220 13% 92%)",
  axisLine: "hsl(220 13% 88%)",
  axisText: "hsl(220 10% 46%)",
  tooltipBg: "hsl(0 0% 100%)",
  tooltipBorder: "hsl(220 13% 90%)",
  cursor: "hsl(220 10% 76%)",
  subtleLine: "hsl(220 13% 92%)",
  cardBg: "hsl(0 0% 100%)",
  up: "hsl(142 76% 36%)",
  down: "hsl(0 72% 51%)",
  upCandle: "hsl(142 76% 30%)",
  downCandle: "hsl(0 72% 45%)",
  line: "hsl(45 93% 47%)",
  ema20: "hsl(217 91% 50%)",
  ema50: "hsl(263 70% 45%)",
  rsi: "hsl(292 84% 50%)",
};
