"use client";

import { useState } from "react";
import { Check, ChevronsUpDown, X } from "lucide-react";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { AVAILABLE_COINS, type CoinOption } from "@/lib/api";

interface CoinPickerProps {
  selected: string[];
  onChange: (symbols: string[]) => void;
}

export function CoinPicker({ selected, onChange }: CoinPickerProps) {
  const [open, setOpen] = useState(false);

  const toggle = (symbol: string) => {
    if (selected.includes(symbol)) {
      onChange(selected.filter((s) => s !== symbol));
    } else {
      onChange([...selected, symbol]);
    }
  };

  const remove = (symbol: string) => {
    onChange(selected.filter((s) => s !== symbol));
  };

  const selectedCoins = AVAILABLE_COINS.filter((c) =>
    selected.includes(c.symbol)
  );

  return (
    <div className="flex flex-wrap items-center gap-2">
      <Popover open={open} onOpenChange={setOpen}>
        <PopoverTrigger asChild>
          <button className="flex items-center gap-2 rounded-lg border border-border bg-card px-3 py-2 text-data text-muted-foreground transition-colors hover:bg-muted hover:text-foreground">
            <ChevronsUpDown className="h-4 w-4" />
            <span>Select coins</span>
          </button>
        </PopoverTrigger>
        <PopoverContent
          className="w-[220px] border-border bg-card p-1"
          onOpenAutoFocus={(e) => e.preventDefault()}
        >
          <div className="max-h-[250px] overflow-y-auto">
            {AVAILABLE_COINS.map((coin) => {
              const isSelected = selected.includes(coin.symbol);
              return (
                <button
                  key={coin.symbol}
                  onClick={() => toggle(coin.symbol)}
                  className={cn(
                    "flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-data transition-colors",
                    isSelected
                      ? "bg-muted text-foreground"
                      : "text-muted-foreground hover:bg-muted/50 hover:text-foreground"
                  )}
                >
                  <div
                    className="h-2.5 w-2.5 rounded-full shrink-0"
                    style={{ backgroundColor: coin.color }}
                  />
                  <span className="flex-1 text-left">
                    {coin.label}
                    <span className="ml-1 text-micro text-muted-foreground">
                      {coin.symbol}
                    </span>
                  </span>
                  <Check
                    className={cn(
                      "h-3.5 w-3.5 shrink-0 text-primary",
                      isSelected ? "opacity-100" : "opacity-0"
                    )}
                  />
                </button>
              );
            })}
          </div>
        </PopoverContent>
      </Popover>

      {/* Selected coin badges */}
      {selectedCoins.map((coin) => (
        <CoinBadge key={coin.symbol} coin={coin} onRemove={remove} />
      ))}
    </div>
  );
}

function CoinBadge({
  coin,
  onRemove,
}: {
  coin: CoinOption;
  onRemove: (symbol: string) => void;
}) {
  return (
    <Badge
      variant="secondary"
      className="flex items-center gap-1.5 pl-2 pr-1 py-1"
    >
      <div
        className="h-2 w-2 rounded-full"
        style={{ backgroundColor: coin.color }}
      />
      <span className="text-micro font-medium">{coin.label}</span>
      <button
        onClick={() => onRemove(coin.symbol)}
        className="ml-0.5 rounded p-0.5 text-muted-foreground hover:bg-muted hover:text-foreground transition-colors"
      >
        <X className="h-3 w-3" />
      </button>
    </Badge>
  );
}
