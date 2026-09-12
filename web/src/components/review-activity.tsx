import {
  CheckCircle2,
  Circle,
  GitBranch,
  LoaderCircle,
  MessageSquare,
  RefreshCw,
  Send,
  ShieldCheck,
  Sparkles,
  Wrench,
  XCircle,
} from "lucide-react";
import type { LucideIcon } from "lucide-react";
import type { ReviewEvent } from "@/lib/api";
import { formatDateTime, formatRelative } from "@/lib/format";
import { cn } from "@/lib/utils";

interface EventPresentation {
  icon: LucideIcon;
  className?: string;
}

function eventPresentation(kind: string): EventPresentation {
  switch (kind) {
    case "request":
      return { icon: MessageSquare, className: "text-zinc-500" };
    case "prepare":
      return { icon: Wrench, className: "text-zinc-500" };
    case "checkout":
      return { icon: GitBranch, className: "text-zinc-500" };
    case "sandbox":
      return { icon: ShieldCheck, className: "text-zinc-500" };
    case "agent_started":
      return { icon: Sparkles, className: "text-zinc-500" };
    case "agent_finished":
      return { icon: Sparkles, className: "text-zinc-400" };
    case "publish":
      return { icon: Send, className: "text-zinc-500" };
    case "retry":
      return { icon: RefreshCw, className: "text-amber-600" };
    case "completed":
      return { icon: CheckCircle2, className: "text-green-600" };
    case "failed":
      return { icon: XCircle, className: "text-red-600" };
    default:
      return { icon: Circle, className: "text-zinc-400" };
  }
}

interface ReviewActivityProps {
  events: ReviewEvent[];
  /** While the review is still working, the newest entry shows a spinner. */
  active?: boolean;
}

export function ReviewActivity({ events, active = false }: ReviewActivityProps) {
  if (!events.length) {
    return (
      <p className="text-sm text-zinc-500" role="status">
        No activity recorded yet.
      </p>
    );
  }

  return (
    <div className="relative">
      <span
        aria-hidden="true"
        className="absolute top-2 bottom-2 left-[15px] w-px bg-zinc-200"
      />
      <ol className="relative space-y-4" aria-label="Review activity timeline">
        {events.map((event, index) => {
          const { icon: Icon, className } = eventPresentation(event.kind);
          const isNewest = index === events.length - 1;
          const absolute = formatDateTime(event.created_at);

          return (
            // Newest entry fades in; older entries already finished animating.
            <li
              key={event.id}
              className={cn("relative flex items-start gap-3", isNewest && "animate-fade-in")}
            >
              <span
                aria-hidden="true"
                className="relative z-10 flex size-8 shrink-0 items-center justify-center rounded-full border border-zinc-200 bg-white"
              >
                <Icon className={cn("size-3.5", className)} />
              </span>
              <div className="min-w-0 flex-1 pt-1">
                <p className="break-words text-sm text-zinc-900">{event.message}</p>
                <p className="mt-0.5 flex items-center gap-1.5 text-xs text-zinc-400">
                  <time dateTime={event.created_at} title={absolute}>
                    {formatRelative(event.created_at)}
                  </time>
                  {isNewest && active && (
                    <LoaderCircle
                      className="animate-spin size-3.5 text-zinc-400"
                      aria-hidden="true"
                    />
                  )}
                </p>
              </div>
            </li>
          );
        })}
      </ol>
    </div>
  );
}
