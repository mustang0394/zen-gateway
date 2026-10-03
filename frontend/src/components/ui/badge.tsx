import * as React from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

const badgeVariants = cva(
  // whitespace-nowrap：徽章文字不得折行（列宽不足时由父容器截断，而非把文字拆成两行）
  // [&_svg]:shrink-0：flex 布局下图标保持原始尺寸，不被压缩变形
  "inline-flex items-center gap-1 whitespace-nowrap rounded-full border px-2.5 py-0.5 text-xs font-medium transition-colors [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        default: "border-transparent bg-[var(--color-secondary)] text-[var(--color-secondary-foreground)]",
        outline: "border-[var(--color-border)] text-[var(--color-foreground)]",
        success: "border-transparent bg-[var(--color-success)]/15 text-[var(--color-success)]",
        warning: "border-transparent bg-[var(--color-warning)]/15 text-[var(--color-warning)]",
        destructive:
          "border-transparent bg-[var(--color-destructive)]/15 text-[var(--color-destructive)]",
        muted: "border-transparent bg-[var(--color-muted)] text-[var(--color-muted-foreground)]",
      },
    },
    defaultVariants: { variant: "default" },
  },
);

export interface BadgeProps
  extends React.HTMLAttributes<HTMLSpanElement>,
    VariantProps<typeof badgeVariants> {}

export function Badge({ className, variant, ...props }: BadgeProps) {
  return <span className={cn(badgeVariants({ variant }), className)} {...props} />;
}
