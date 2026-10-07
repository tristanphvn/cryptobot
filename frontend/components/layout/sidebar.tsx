"use client";

import { cn } from "@/lib/utils";
import {
  LayoutDashboard,
  LineChart,
  History,
  Brain,
  Settings,
  Bot,
  Command,
  Wallet,
} from "lucide-react";
import Link from "next/link";
import { usePathname } from "next/navigation";

const navItems = [
  { href: "/", label: "Dashboard", icon: LayoutDashboard, shortcut: "D" },
  { href: "/chart", label: "Chart", icon: LineChart, shortcut: "C" },
  { href: "/portfolio", label: "Portfolio", icon: Wallet, shortcut: "P" },
  { href: "/trades", label: "Trades", icon: History, shortcut: "T" },
  { href: "/ai", label: "AI Brain", icon: Brain, shortcut: "A" },
  { href: "/settings", label: "Settings", icon: Settings, shortcut: "S" },
];

/**
 * Sidebar — icon-only 56px rail. Status (mode, risk, health) lives in
 * the TopStatusBar so the sidebar carries one responsibility only:
 * route navigation. Hover reveals labels via native tooltips.
 */
export function Sidebar() {
  const pathname = usePathname();

  return (
    <aside className="fixed inset-y-0 left-0 z-50 flex w-14 flex-col items-center border-r border-border bg-card">
      {/* Logo mark */}
      <div className="flex h-11 w-full items-center justify-center border-b border-border">
        <div
          className="flex h-7 w-7 items-center justify-center rounded-lg bg-primary/15"
          title="CryptoBot"
        >
          <Bot className="h-4 w-4 text-primary" />
        </div>
      </div>

      {/* Navigation icons */}
      <nav className="flex flex-1 flex-col items-center gap-1 py-3">
        {navItems.map((item) => {
          const isActive = pathname === item.href;
          return (
            <Link
              key={item.href}
              href={item.href}
              title={item.label}
              className={cn(
                "group relative flex h-9 w-9 items-center justify-center rounded-lg transition-colors duration-150",
                isActive
                  ? "bg-ov-4 text-foreground"
                  : "text-muted-foreground hover:bg-ov-2 hover:text-foreground"
              )}
            >
              {isActive && (
                <div className="absolute left-0 top-1/2 h-5 w-[2px] -translate-y-1/2 rounded-r-full bg-primary" />
              )}
              <item.icon
                className={cn(
                  "h-4 w-4 transition-colors",
                  isActive ? "text-primary" : "text-muted-foreground group-hover:text-foreground"
                )}
              />
            </Link>
          );
        })}
      </nav>

      {/* Command palette hint */}
      <div className="mb-3 flex h-9 w-9 items-center justify-center rounded-lg border border-dashed border-border text-muted-foreground" title="Command palette (⌘K)">
        <Command className="h-3.5 w-3.5" />
      </div>
    </aside>
  );
}
