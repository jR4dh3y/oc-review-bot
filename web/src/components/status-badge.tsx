import { CheckCircle2, Circle, Clock, RefreshCw, XCircle } from "lucide-react";
import type { LucideIcon } from "lucide-react";
import type { ReviewStatus } from "@/lib/api";
import { cn } from "@/lib/utils";

interface StatusPresentation {
  label: string;
  icon: LucideIcon;
  /** Text + icon color; statuses read as colored text, not pills. */
  className: string;
  /** Renders a softly pulsing dot instead of a static icon (running). */
  pulsing?: boolean;
}

function statusPresentation(status: string): StatusPresentation {
  switch (status) {
    case "queued":
      return { label: "Queued", icon: Clock, className: "text-zinc-500" };
    case "running":
      return { label: "Running", icon: Clock, className: "text-amber-600", pulsing: true };
    case "done":
      return { label: "Done", icon: CheckCircle2, className: "text-green-700" };
    case "failed":
      return { label: "Failed", icon: XCircle, className: "text-red-700" };
    case "reconciliation_required":
      return { label: "Retry scheduled", icon: RefreshCw, className: "text-amber-600" };
    default:
      // Unknown status from a newer backend: render it defensively as neutral.
      return {
        label: status.replace(/_/g, " ").trim() || "Unknown",
        icon: Circle,
        className: "text-zinc-500",
      };
  }
}

export function StatusBadge({ status, className }: { status: ReviewStatus; className?: string }) {
  const presentation = statusPresentation(String(status));
  const Icon = presentation.icon;

  return (
    <span
      className={cn("inline-flex items-center gap-1.5 text-sm font-medium", presentation.className, className)}
    >
      {presentation.pulsing ? (
        <span
          className="animate-soft-pulse size-2 rounded-full bg-amber-500"
          aria-hidden="true"
        />
      ) : (
        <Icon className="size-4" aria-hidden="true" />
      )}
      {presentation.label}
    </span>
  );
}
