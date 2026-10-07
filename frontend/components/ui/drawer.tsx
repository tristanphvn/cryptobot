"use client";

import { ReactNode, useEffect } from "react";
import { createPortal } from "react-dom";
import { X } from "lucide-react";
import { cn } from "@/lib/utils";

interface DrawerProps {
  open: boolean;
  onClose: () => void;
  /** Drawer header. Use a short string or a custom node. */
  title?: ReactNode;
  /** Tailwind width class. Default `w-[480px]`. */
  widthClass?: string;
  children: ReactNode;
}

/**
 * Right-side overlay primitive — backdrop + sliding panel + ESC/click
 * close. Portaled to <body> so it escapes the dashboard's grid clipping
 * once phase 6 lands. Kept small on purpose: phases 3/5 only need a
 * drilldown surface, not full Sheet/Dialog ergonomics.
 *
 * Body scroll is locked while open via a className on <html>, so nested
 * drawers and command palettes don't fight over `overflow`.
 */
export function Drawer({ open, onClose, title, widthClass = "w-[480px]", children }: DrawerProps) {
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    const root = document.documentElement;
    const prev = root.style.overflow;
    root.style.overflow = "hidden";
    return () => {
      window.removeEventListener("keydown", onKey);
      root.style.overflow = prev;
    };
  }, [open, onClose]);

  if (typeof document === "undefined") return null;
  if (!open) return null;

  return createPortal(
    <div className="fixed inset-0 z-[60]">
      <button
        type="button"
        aria-label="Close drawer"
        onClick={onClose}
        className="absolute inset-0 bg-background/70 backdrop-blur-sm"
      />
      <aside
        role="dialog"
        aria-modal="true"
        className={cn(
          "absolute right-0 top-0 flex h-full flex-col border-l border-border bg-card shadow-2xl",
          widthClass
        )}
      >
        <header className="flex h-11 shrink-0 items-center justify-between border-b border-border px-4">
          <div className="text-data font-semibold tracking-tight">{title}</div>
          <button
            type="button"
            onClick={onClose}
            className="flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground hover:bg-muted/60 hover:text-foreground"
            aria-label="Close"
          >
            <X className="h-4 w-4" />
          </button>
        </header>
        <div className="flex-1 overflow-y-auto p-4">{children}</div>
      </aside>
    </div>,
    document.body
  );
}
