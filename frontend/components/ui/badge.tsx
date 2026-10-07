import * as React from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

const badgeVariants = cva(
  "inline-flex items-center rounded-md border px-2 py-0.5 text-micro font-semibold uppercase tracking-wider transition-colors",
  {
    variants: {
      variant: {
        default: "border-profit/30 bg-profit/15 text-profit",
        secondary: "border-border bg-muted/50 text-muted-foreground",
        destructive: "border-loss/30 bg-loss/15 text-loss",
        outline: "border-border text-foreground/70",
        warning: "border-warning/30 bg-warning/15 text-warning",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  }
);

export interface BadgeProps
  extends React.HTMLAttributes<HTMLDivElement>,
    VariantProps<typeof badgeVariants> {}

function Badge({ className, variant, ...props }: BadgeProps) {
  return (
    <div className={cn(badgeVariants({ variant }), className)} {...props} />
  );
}

export { Badge, badgeVariants };
