import { useEffect, useState, type ReactNode } from "react";
import { Link, createRoute } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft, ExternalLink } from "lucide-react";
import { Route as RootRoute } from "./__root";
import { RequireUser } from "@/components/auth-gate";
import { BotMention } from "@/components/bot-mention";
import { FindingCard } from "@/components/finding-card";
import { Markdown } from "@/components/markdown";
import { LoadingState, RequestError } from "@/components/query-state";
import { ReviewActivity } from "@/components/review-activity";
import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { api, getErrorMessage, isApiError } from "@/lib/api";
import type { Finding, ReviewEvent, ReviewListItem } from "@/lib/api";
import { formatDateTime, formatElapsed, formatFindingCount, formatRelative } from "@/lib/format";
import { userQueryKey, userReviewQueryKey, useCurrentUser, useUnauthorizedRedirect } from "@/lib/auth";

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: "/dashboard",
  component: Dashboard,
});

export const ReviewDetailRoute = createRoute({
  getParentRoute: () => RootRoute,
  path: "/dashboard/$reviewId",
  component: ReviewDetailPage,
});

const ACTIVE_STATUSES = new Set(["queued", "running"]);

// The published summary comment repeats its findings as a markdown list and
// signs off inside a <sub> tag. The dashboard renders findings as cards of
// its own, so keep the narrative, diagram, and sign-off line only.
function publishedSummaryBody(summaryMd: string): string {
  const clean = summaryMd.replace(/<\/?sub>/g, "");
  const marker = "### Findings";
  const at = clean.indexOf(marker);
  if (at === -1) return clean;
  const before = clean.slice(0, at);
  const footer = clean
    .slice(at + marker.length)
    .split("\n")
    .find((line) => line.startsWith("Reviewed by "));
  return footer ? `${before}\n\n${footer}` : before;
}

/** Ticks once per second while `enabled`; frozen (no interval) otherwise. */
function useNow(enabled: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!enabled) return;
    setNow(Date.now());
    const timer = window.setInterval(() => setNow(Date.now()), 1_000);
    return () => window.clearInterval(timer);
  }, [enabled]);
  return now;
}

function Dashboard() {
  return (
    <RequireUser>
      <ReviewList />
    </RequireUser>
  );
}

function ReviewList() {
  const currentUser = useCurrentUser();
  const userID = currentUser.data?.id;
  const reviews = useQuery({
    queryKey: userID == null ? ["reviews", "anonymous"] : userQueryKey("reviews", userID),
    queryFn: api.reviews,
    enabled: userID != null,
    refetchInterval: (query) =>
      query.state.data?.some((review) => ACTIVE_STATUSES.has(review.status)) ? 3_000 : false,
  });
  const sessionExpired = useUnauthorizedRedirect(reviews.error);
  const hasActiveReview = reviews.data?.some((review) => ACTIVE_STATUSES.has(review.status)) ?? false;
  const now = useNow(hasActiveReview);

  return (
    <Card aria-busy={reviews.isFetching}>
      <CardHeader>
        <CardTitle as="h1">Reviews</CardTitle>
        <CardDescription>
          Latest 50 review requests. Queued and running reviews refresh every three seconds.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {reviews.isPending ? (
          <LoadingState>Loading review requests…</LoadingState>
        ) : sessionExpired ? (
          <LoadingState>Your session has ended. Returning you to sign in…</LoadingState>
        ) : reviews.isError ? (
          <RequestError
            title="Couldn’t load reviews"
            description={getErrorMessage(reviews.error, "Try again in a moment.")}
            onRetry={() => void reviews.refetch()}
          />
        ) : reviews.data?.length ? (
          <ul className="divide-y divide-zinc-100" aria-label="Recent review requests">
            {reviews.data.map((review) => (
              <ReviewRow key={review.id} review={review} now={now} />
            ))}
          </ul>
        ) : (
          <div className="rounded-md border border-dashed border-zinc-300 p-4 text-sm text-zinc-600">
            <p className="font-medium text-zinc-900">No review requests yet.</p>
            <p className="mt-1">
              After registering, mention <BotMention /> on a pull request in an installed repository.
            </p>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function ReviewRow({ review, now }: { review: ReviewListItem; now: number }) {
  const active = ACTIVE_STATUSES.has(review.status);
  const elapsed = active
    ? formatElapsed(review.status === "running" ? (review.started_at ?? review.created_at) : review.created_at, now)
    : null;

  return (
    <li className="flex items-start gap-3 py-3 text-sm">
      <StatusBadge status={review.status} className="mt-0.5 shrink-0" />
      <div className="min-w-0 flex-1">
        <Link
          to="/dashboard/$reviewId"
          params={{ reviewId: String(review.id) }}
          className="break-words font-medium hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-950 focus-visible:ring-offset-2"
        >
          {review.repo_full}#{review.pr_number}
        </Link>
        <p className="mt-1 break-words text-xs text-zinc-500">
          Requested by {review.requester_login} ·{" "}
          <time dateTime={review.created_at} title={formatDateTime(review.created_at)}>
            {formatRelative(review.created_at)}
          </time>
        </p>
        {review.status === "failed" && review.error && (
          <p className="mt-1 truncate text-xs text-red-600" title={review.error}>
            {review.error}
          </p>
        )}
      </div>
      <div className="flex shrink-0 flex-col items-end gap-1 text-right">
        {review.findings_count > 0 && (
          <span className="text-xs font-medium text-zinc-700">
            {formatFindingCount(review.findings_count)}
          </span>
        )}
        {review.model && (
          <span className="max-w-48 truncate text-xs text-zinc-400" title={review.model}>
            {review.model}
          </span>
        )}
        {elapsed != null && (
          <span className="text-xs text-zinc-500" role="status">
            {review.status === "running" ? "running for" : "queued for"} {elapsed}
          </span>
        )}
      </div>
    </li>
  );
}

function ReviewDetailPage() {
  return (
    <RequireUser>
      <ReviewDetail />
    </RequireUser>
  );
}

function ReviewDetail() {
  const { reviewId } = ReviewDetailRoute.useParams();
  const currentUser = useCurrentUser();
  const userID = currentUser.data?.id;
  const detail = useQuery({
    queryKey:
      userID == null ? ["review", "anonymous", reviewId] : userReviewQueryKey(userID, reviewId),
    queryFn: () => api.reviewDetail(reviewId),
    enabled: userID != null,
    refetchInterval: (query) => {
      const status = query.state.data?.review.status;
      return status === "queued" || status === "running" || status === "reconciliation_required"
        ? 3_000
        : false;
    },
  });
  const sessionExpired = useUnauthorizedRedirect(detail.error);
  const status = detail.data?.review.status;
  const detailActive = status != null && ACTIVE_STATUSES.has(status);
  const now = useNow(status === "running");

  return (
    <Card aria-busy={detail.isFetching}>
      <CardHeader className="gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
          <CardTitle as="h1" className="break-words">
            {detail.data
              ? `${detail.data.review.repo_full}#${detail.data.review.pr_number}`
              : `Review #${reviewId}`}
          </CardTitle>
          {detail.data && <StatusBadge status={detail.data.review.status} />}
        </div>
        <div className="flex shrink-0 items-center gap-2">
          {detail.data && (
            <Button variant="outline" size="icon" asChild>
              <a
                href={`https://github.com/${detail.data.review.repo_full}/pull/${detail.data.review.pr_number}`}
                target="_blank"
                rel="noreferrer"
                aria-label="Open pull request on GitHub"
              >
                <ExternalLink aria-hidden="true" />
              </a>
            </Button>
          )}
          <Button variant="outline" size="sm" asChild>
            <Link to="/dashboard">
              <ArrowLeft aria-hidden="true" /> Back to reviews
            </Link>
          </Button>
        </div>
      </CardHeader>
      <CardContent className="grid gap-6 text-sm">
        {detail.isPending ? (
          <LoadingState>Loading review details…</LoadingState>
        ) : sessionExpired ? (
          <LoadingState>Your session has ended. Returning you to sign in…</LoadingState>
        ) : detail.isError ? (
          <RequestError
            title={
              isApiError(detail.error) && detail.error.status === 404
                ? "Review not found"
                : "Couldn’t load this review"
            }
            description={
              isApiError(detail.error) && detail.error.status === 404
                ? "This review may have been removed."
                : getErrorMessage(detail.error, "Try again in a moment.")
            }
            onRetry={() => void detail.refetch()}
          />
        ) : detail.data ? (
          <ReviewDetailContent detail={detail.data} now={now} active={detailActive} />
        ) : null}
      </CardContent>
    </Card>
  );
}

function ReviewDetailContent({
  detail,
  now,
  active,
}: {
  detail: {
    review: ReviewListItem;
    findings: Finding[];
    events: ReviewEvent[];
  };
  now: number;
  active: boolean;
}) {
  const { review } = detail;
  const finishedMs = review.finished_at == null ? null : Date.parse(review.finished_at);
  const startedAt = review.started_at ?? (review.status === "running" ? review.created_at : null);

  let duration: string | null = null;
  if (startedAt != null && finishedMs != null && Number.isFinite(finishedMs)) {
    duration = formatElapsed(startedAt, finishedMs);
  } else if (startedAt != null && review.status === "running") {
    duration = formatElapsed(startedAt, now);
  }

  return (
    <>
      <dl className="grid gap-x-6 gap-y-3 text-sm sm:grid-cols-2 lg:grid-cols-3">
        <MetaItem label="Requester">{review.requester_login}</MetaItem>
        <MetaItem label="Created">
          <time dateTime={review.created_at}>{formatDateTime(review.created_at)}</time>
        </MetaItem>
        <MetaItem label="Started">
          {review.started_at ? (
            <time dateTime={review.started_at}>{formatDateTime(review.started_at)}</time>
          ) : (
            "—"
          )}
        </MetaItem>
        <MetaItem label="Finished">
          {review.finished_at ? (
            <time dateTime={review.finished_at}>{formatDateTime(review.finished_at)}</time>
          ) : (
            "—"
          )}
        </MetaItem>
        <MetaItem label="Duration">{duration ?? "—"}</MetaItem>
        <MetaItem label="Model">{review.model || "—"}</MetaItem>
      </dl>

      <section aria-labelledby="review-activity" className="grid gap-3">
        <h2 id="review-activity" className="text-sm font-semibold">
          Activity
        </h2>
        <ReviewActivity events={detail.events} active={active} />
      </section>

      {review.error ? (
        <div className="rounded-md border border-red-200 bg-red-50 p-3 text-red-700" role="alert">
          {review.error}
        </div>
      ) : review.summary_md ? (
        <section aria-labelledby="review-summary" className="grid gap-3">
          <h2 id="review-summary" className="text-sm font-semibold">
            Summary
          </h2>
          <Markdown>{publishedSummaryBody(review.summary_md)}</Markdown>
        </section>
      ) : null}

      <section aria-labelledby="review-findings" className="grid gap-3">
        <h2 id="review-findings" className="text-sm font-semibold">
          Findings
        </h2>
        {detail.findings.length ? (
          <ul className="grid gap-3">
            {detail.findings.map((finding, index) => (
              <li
                key={finding.id ?? `${finding.path}-${finding.line}-${finding.side}-${index}`}
              >
                <FindingCard
                  finding={finding}
                  repoFull={review.repo_full}
                  headSha={review.head_sha}
                />
              </li>
            ))}
          </ul>
        ) : (
          <p className="rounded-md border border-dashed border-zinc-300 p-3 text-zinc-600">
            No inline findings were returned for this review.
          </p>
        )}
      </section>
    </>
  );
}

function MetaItem({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs text-zinc-500">{label}</dt>
      <dd className="mt-0.5 break-words text-sm">{children}</dd>
    </div>
  );
}
