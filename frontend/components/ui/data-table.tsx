"use client";

import { ReactNode, useMemo } from "react";
import { ArrowUpDown } from "lucide-react";
import { cn } from "@/lib/utils";

export type ColumnAlign = "left" | "right" | "center";
export type SortDir = "asc" | "desc";

export interface SortState {
  key: string;
  dir: SortDir;
}

export interface DataTableColumn<Row> {
  /** Stable column key — also used as React key and as sort key. */
  key: string;
  /** Header label. Pass `""` to render an unlabelled column. */
  header: ReactNode;
  /** Cell renderer. */
  cell: (row: Row, index: number) => ReactNode;
  /** Defaults: numeric-looking columns right; everything else left. */
  align?: ColumnAlign;
  /** Tailwind width class, e.g. `"w-24"`. */
  width?: string;
  /** Hide on viewports narrower than this Tailwind breakpoint. */
  hideBelow?: "sm" | "md" | "lg" | "xl";
  /** Header tooltip. */
  title?: string;
  /** Marks the column as sortable; requires the parent to pass `sort` + `onSortChange`. */
  sortable?: boolean;
}

interface DataTableProps<Row> {
  columns: DataTableColumn<Row>[];
  rows: Row[];
  rowKey: (row: Row, index: number) => string | number;
  /** Click handler for the whole row — used by drawer-style drilldowns. */
  onRowClick?: (row: Row, index: number) => void;
  /** Rendered inside <tbody> when `rows.length === 0`. */
  empty?: ReactNode;
  /** Sticky header for scrollable containers. */
  stickyHeader?: boolean;
  /** Controlled sort state for sortable columns. */
  sort?: SortState;
  /** Called when the operator clicks a sortable header. */
  onSortChange?: (next: SortState) => void;
  /** Tailwind classes appended to the outer wrapper. */
  className?: string;
  /** Tailwind class for every <tr> in the body. */
  rowClassName?: string;
}

const ALIGN_CLS: Record<ColumnAlign, string> = {
  left: "text-left",
  right: "text-right",
  center: "text-center",
};

const HIDE_BELOW_CLS: Record<NonNullable<DataTableColumn<unknown>["hideBelow"]>, string> = {
  sm: "hidden sm:table-cell",
  md: "hidden md:table-cell",
  lg: "hidden lg:table-cell",
  xl: "hidden xl:table-cell",
};

/**
 * Dense tabular primitive for blotters and feeds (open positions, closed
 * trades, signal tape, audit log). Visual baseline: 11px uppercase
 * tracking-wider headers, 12-13px body in `tabular-nums font-mono`,
 * subtle row dividers. All columns default to right-aligned numerics
 * unless overridden, since trading data is overwhelmingly numeric.
 *
 * Sort is controlled: the parent owns sorted rows and decides the
 * comparator. This component only renders the sort affordance and
 * fires `onSortChange` — clicking the active column flips direction,
 * clicking a new column switches to it in descending order.
 */
export function DataTable<Row>({
  columns,
  rows,
  rowKey,
  onRowClick,
  empty,
  stickyHeader,
  sort,
  onSortChange,
  className,
  rowClassName,
}: DataTableProps<Row>) {
  const colClasses = useMemo(
    () =>
      columns.map((c) => {
        const align = ALIGN_CLS[c.align ?? "right"];
        const width = c.width ?? "";
        const hide = c.hideBelow ? HIDE_BELOW_CLS[c.hideBelow] : "";
        return cn(align, width, hide);
      }),
    [columns]
  );

  const handleSort = (key: string) => {
    if (!onSortChange) return;
    if (sort?.key === key) {
      onSortChange({ key, dir: sort.dir === "asc" ? "desc" : "asc" });
    } else {
      onSortChange({ key, dir: "desc" });
    }
  };

  return (
    <div className={cn("overflow-x-auto rounded-lg border border-border bg-card", className)}>
      <table className="w-full border-collapse text-data">
        <thead
          className={cn(
            "text-micro uppercase tracking-wider text-muted-foreground",
            stickyHeader && "sticky top-0 z-10 bg-card"
          )}
        >
          <tr className="border-b border-border">
            {columns.map((c, i) => {
              const isSortable = c.sortable && onSortChange;
              const isActive = isSortable && sort?.key === c.key;
              return (
                <th
                  key={c.key}
                  title={typeof c.title === "string" ? c.title : undefined}
                  onClick={isSortable ? () => handleSort(c.key) : undefined}
                  className={cn(
                    "px-3 py-2 font-medium",
                    colClasses[i],
                    isSortable && "cursor-pointer select-none hover:text-foreground"
                  )}
                >
                  <span className="inline-flex items-center gap-1">
                    {c.header}
                    {isSortable && (
                      <>
                        <ArrowUpDown
                          className={cn(
                            "h-3 w-3",
                            isActive ? "text-foreground" : "text-muted-foreground/40"
                          )}
                        />
                        {isActive && (
                          <span className="text-micro">{sort.dir === "asc" ? "▲" : "▼"}</span>
                        )}
                      </>
                    )}
                  </span>
                </th>
              );
            })}
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 && empty && (
            <tr>
              <td colSpan={columns.length} className="px-3 py-6 text-center text-micro text-muted-foreground">
                {empty}
              </td>
            </tr>
          )}
          {rows.map((row, ri) => (
            <tr
              key={rowKey(row, ri)}
              onClick={onRowClick ? () => onRowClick(row, ri) : undefined}
              className={cn(
                "border-b border-border/40 last:border-b-0 font-mono tabular-nums",
                onRowClick && "cursor-pointer hover:bg-muted/40",
                rowClassName
              )}
            >
              {columns.map((c, ci) => (
                <td key={c.key} className={cn("px-3 py-1.5", colClasses[ci])}>
                  {c.cell(row, ri)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
