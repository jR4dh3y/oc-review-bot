import { CircleHelp, Info, OctagonAlert, ThumbsUp, TriangleAlert } from "lucide-react";
import type { LucideIcon } from "lucide-react";
import type { Finding } from "@/lib/api";
import { Markdown } from "@/components/markdown";
import { cn } from "@/lib/utils";

interface SeverityPresentation {
  label: string;
  icon: LucideIcon;
  /** Text + icon color; severities read as colored text, not pills. */
  className: string;
}

function severityPresentation(severity: string): SeverityPresentation {
  switch (severity.trim().toLowerCase()) {
    case "critical":
      return { label: "Critical", icon: OctagonAlert, className: "text-red-700" };
    case "warning":
      return { label: "Warning", icon: TriangleAlert, className: "text-amber-700" };
    case "info":
      return { label: "Info", icon: Info, className: "text-blue-700" };
    case "praise":
      return { label: "Praise", icon: ThumbsUp, className: "text-green-700" };
    default:
      return {
        label: severity.trim() || "Unknown",
        icon: CircleHelp,
        className: "text-zinc-500",
      };
  }
}

interface FindingCardProps {
  finding: Finding;
  repoFull: string;
  headSha: string;
}

function findingLocationHref(finding: Finding, repoFull: string, headSha: string): string | null {
  // Deep links point at the reviewed head revision. Line anchors only make
  // sense on the RIGHT side; LEFT-side lines have no stable anchor there.
  if (!headSha || !finding.path || !repoFull) return null;
  const anchor = finding.side === "RIGHT" && finding.line > 0 ? `#L${finding.line}` : "";
  return `https://github.com/${repoFull}/blob/${headSha}/${finding.path}${anchor}`;
}

export function FindingCard({ finding, repoFull, headSha }: FindingCardProps) {
  const presentation = severityPresentation(finding.severity);
  const Icon = presentation.icon;
  const href = findingLocationHref(finding, repoFull, headSha);
  const location = `${finding.path}:${finding.line}`;

  return (
    <article className="rounded-md border border-zinc-200 p-4 transition-colors">
      <div className="flex flex-wrap items-center gap-2">
        <span
          className={cn(
            "inline-flex items-center gap-1.5 text-sm font-medium",
            presentation.className,
          )}
        >
          <Icon className="size-4 shrink-0" aria-hidden="true" />
          {presentation.label}
        </span>
        {href ? (
          <a
            href={href}
            target="_blank"
            rel="noreferrer"
            className="break-all font-mono text-xs text-zinc-900 underline decoration-zinc-500 underline-offset-2 transition-colors hover:text-zinc-700 hover:decoration-zinc-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-950 focus-visible:ring-offset-2"
          >
            {location}
          </a>
        ) : (
          <code className="break-all rounded bg-zinc-100 px-1 py-0.5 font-mono text-xs text-zinc-900">
            {location}
          </code>
        )}
      </div>
      {finding.body && (
        <div className="mt-2">
          <Markdown>{finding.body}</Markdown>
        </div>
      )}
    </article>
  );
}
