import * as React from "react"
import { cva, type VariantProps } from "class-variance-authority"

import { cn } from "@/lib/utils"

const badgeVariants = cva(
  "inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-medium leading-5 transition-colors",
  {
    variants: {
      // Tinted pills; the text colour carries the meaning together with the
      // label, never the colour alone.
      variant: {
        default: "bg-primary-soft text-primary",
        secondary: "bg-neutral/15 text-neutral-text",
        destructive: "bg-critical/15 text-critical-text",
        outline: "bg-neutral/15 text-neutral-text",
        success: "bg-secure/15 text-secure-text",
        warning: "bg-warning/15 text-warning-text",
        info: "bg-info/15 text-info-text",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  }
)

export interface BadgeProps
  extends React.HTMLAttributes<HTMLDivElement>,
    VariantProps<typeof badgeVariants> {}

function Badge({ className, variant, ...props }: BadgeProps) {
  return (
    <div className={cn(badgeVariants({ variant }), className)} {...props} />
  )
}

// The variants helper is shared with callers (shadcn pattern); fast refresh still works for Badge.
// eslint-disable-next-line react-refresh/only-export-components
export { Badge, badgeVariants }