"use client";

import { useMemo } from "react";
import { cn } from "@/lib/utils";

export type SparklineTone = "auto" | "profit" | "loss" | "neutral";

interface SparklineProps {
  points: number[];
  /**
   * `auto` (default) picks profit/loss from first-vs-last. Override when
   * the parent already knows the verdict — keeps a flat run from being
   * mis-coloured by float noise.
   */
  tone?: SparklineTone;
  width?: number;
  height?: number;
  className?: string;
}

const STROKE: Record<Exclude<SparklineTone, "auto">, string> = {
  profit: "stroke-profit",
  loss: "stroke-loss",
  neutral: "stroke-muted-foreground/60",
};

/**
 * Minimal inline sparkline — no axes, no tooltip. Renders a polyline
 * across `width × height`. Empty / single-point inputs render a muted
 * placeholder dash so the column never collapses, preserving row
 * height in the blotter.
 */
export function Sparkline({
  points,
  tone = "auto",
  width = 60,
  height = 16,
  className,
}: SparklineProps) {
  const { d, resolvedTone } = useMemo(() => {
    if (points.length < 2) return { d: "", resolvedTone: "neutral" as const };
    const min = Math.min(...points);
    const max = Math.max(...points);
    const range = max - min || 1;
    const stepX = width / (points.length - 1);
    const path = points
      .map((p, i) => {
        const x = i * stepX;
        const y = height - ((p - min) / range) * height;
        return `${i === 0 ? "M" : "L"}${x.toFixed(2)},${y.toFixed(2)}`;
      })
      .join(" ");
    const auto =
      tone === "auto"
        ? points[points.length - 1] >= points[0]
          ? ("profit" as const)
          : ("loss" as const)
        : tone;
    return { d: path, resolvedTone: auto };
  }, [points, tone, width, height]);

  if (!d) {
    return (
      <span
        className={cn("inline-block text-muted-foreground/40", className)}
        style={{ width, lineHeight: 0 }}
      >
        —
      </span>
    );
  }

  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      className={cn("inline-block align-middle", className)}
      aria-hidden
    >
      <path
        d={d}
        fill="none"
        strokeWidth={1.25}
        strokeLinecap="round"
        strokeLinejoin="round"
        className={STROKE[resolvedTone]}
      />
    </svg>
  );
}
