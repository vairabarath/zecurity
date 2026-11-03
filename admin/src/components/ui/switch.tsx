import { cn } from "@/lib/utils"

export interface SwitchProps {
  checked: boolean
  onCheckedChange?: (checked: boolean) => void
  disabled?: boolean
  className?: string
  "aria-label"?: string
}

function Switch({
  checked,
  onCheckedChange,
  disabled,
  className,
  ...props
}: SwitchProps) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      disabled={disabled}
      onClick={() => onCheckedChange?.(!checked)}
      className={cn(
        "relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors",
        checked ? "bg-primary" : "bg-secondary",
        disabled ? "cursor-not-allowed opacity-70" : "cursor-pointer",
        className,
      )}
      {...props}
    >
      <span
        className={cn(
          "inline-block h-5 w-5 translate-x-0.5 rounded-full bg-white shadow transition-transform",
          checked && "translate-x-[22px]",
        )}
      />
    </button>
  )
}

export { Switch }
