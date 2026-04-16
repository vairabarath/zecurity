import * as React from "react"
import { cn } from "@/lib/utils"

interface ShimmerButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  shimmerColor?: string
  shimmerSize?: string
  borderRadius?: string
  shimmerDuration?: string
  background?: string
}

const ShimmerButton = React.forwardRef<HTMLButtonElement, ShimmerButtonProps>(
  (
    {
      shimmerColor = "oklch(0.68 0.19 250)",
      shimmerSize = "0.1em",
      borderRadius = "0.5rem",
      shimmerDuration = "2.5s",
      background = "oklch(0.17 0.04 250)",
      className,
      children,
      ...props
    },
    ref
  ) => {
    return (
      <button
        ref={ref}
        className={cn(
          "group relative z-0 flex cursor-pointer items-center justify-center overflow-hidden whitespace-nowrap px-6 py-3 font-body text-sm font-medium text-white transition-all",
          "hover:scale-[1.02] active:scale-[0.98]",
          "disabled:pointer-events-none disabled:opacity-50",
          className
        )}
        style={
          {
            "--shimmer-color": shimmerColor,
            "--radius": borderRadius,
            "--speed": shimmerDuration,
            "--cut": shimmerSize,
            "--bg": background,
            borderRadius,
          } as React.CSSProperties
        }
        {...props}
      >
        {/* Shimmer effect */}
        <div
          className="absolute inset-0 overflow-hidden"
          style={{ borderRadius }}
        >
          <div
            className="absolute inset-[-100%] animate-[spin_var(--speed)_linear_infinite]"
            style={{
              background: `conic-gradient(from 0deg, transparent 0 340deg, var(--shimmer-color) 360deg)`,
            }}
          />
        </div>

        {/* Background */}
        <div
          className="absolute inset-[1px]"
          style={{
            borderRadius: `calc(${borderRadius} - 1px)`,
            background: `var(--bg)`,
          }}
        />

        {/* Inner glow */}
        <div
          className="absolute inset-[1px] opacity-0 transition-opacity duration-300 group-hover:opacity-100"
          style={{
            borderRadius: `calc(${borderRadius} - 1px)`,
            background: `radial-gradient(ellipse at center, var(--shimmer-color) 0%, transparent 70%)`,
            opacity: 0,
          }}
        />
        <div className="absolute inset-[1px] opacity-0 transition-opacity duration-300 group-hover:opacity-[0.08]"
          style={{
            borderRadius: `calc(${borderRadius} - 1px)`,
            background: `radial-gradient(ellipse at center, var(--shimmer-color) 0%, transparent 70%)`,
          }}
        />

        <span className="relative z-10 flex items-center gap-2">{children}</span>
      </button>
    )
  }
)
ShimmerButton.displayName = "ShimmerButton"

export { ShimmerButton }
