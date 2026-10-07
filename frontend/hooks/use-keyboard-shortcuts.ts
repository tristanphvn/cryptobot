"use client";

import { useEffect } from "react";

export interface Shortcut {
  key: string;
  ctrl?: boolean;
  meta?: boolean;
  shift?: boolean;
  description: string;
  action: () => void;
}

/**
 * Registers global keyboard shortcuts.
 * Ignores events when the user is typing in an input/textarea.
 */
export function useKeyboardShortcuts(shortcuts: Shortcut[]) {
  useEffect(() => {
    function handler(e: KeyboardEvent) {
      // Don't fire shortcuts when user is typing
      const target = e.target as HTMLElement;
      if (
        target.tagName === "INPUT" ||
        target.tagName === "TEXTAREA" ||
        target.isContentEditable
      ) {
        return;
      }

      for (const s of shortcuts) {
        const modifierMatch =
          (s.ctrl ? e.ctrlKey || e.metaKey : true) &&
          (s.meta ? e.metaKey : true) &&
          (s.shift ? e.shiftKey : !e.shiftKey);

        if (e.key.toLowerCase() === s.key.toLowerCase() && modifierMatch) {
          e.preventDefault();
          s.action();
          return;
        }
      }
    }

    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [shortcuts]);
}

/** Pre-defined shortcut map for the command palette hint text */
export const SHORTCUT_MAP = {
  COMMAND_PALETTE: { key: "k", meta: true, label: "\u2318K" },
  KILL_SWITCH:    { key: "Escape", shift: true, label: "Shift+Esc" },
  PAUSE_AI:       { key: "p", meta: true, label: "\u2318P" },
  TOGGLE_CHART:   { key: "c", label: "C" },
  NEXT_SYMBOL:    { key: "]", label: "]" },
  PREV_SYMBOL:    { key: "[", label: "[" },
} as const;
