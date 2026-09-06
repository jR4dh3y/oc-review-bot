import { Link, createRoute } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft } from "lucide-react";
import { Route as RootRoute } from "./__root";
import { RequireUser } from "@/components/auth-gate";
import { BotMention } from "@/components/bot-mention";
import { LoadingState, RequestError } from "@/components/query-state";
import { api, getErrorMessage, isApiError, type ReviewStatus } from "@/lib/api";
import { userQueryKey, userReviewQueryKey, useCurrentUser, useUnauthorizedRedirect } from "@/lib/auth";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

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

function statusVariant(status: ReviewStatus) {
  switch (status) {
    case "done":
      return "success" as const;
    case "failed":
      return "destructive" as const;
    case "running":
      return "warning" as const;
    default:
      return "secondary" as const;
  }
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
      query.state.data?.some(
        (review) => review.status === "queued" || review.status === "running",
      )
        ? 5_000
        : false,
  });
  const sessionExpired = useUnauthorizedRedirect(reviews.error);

  return (
    <Card aria-busy={reviews.isFetching}>
      <CardHeader>
        <CardTitle as="h1">Reviews</CardTitle>
        <CardDescription>
          Latest 50 review requests. Queued and running reviews refresh every five seconds.
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
            {reviews.data.map((review) => {
              const createdAt = formatDateTime(review.created_at);
              return (
                <li key={review.id} className="flex items-start gap-3 py-3 text-sm">
                  <Badge variant={statusVariant(review.status)} className="mt-0.5 shrink-0">
                    {review.status}
                  </Badge>
                  <div className="min-w-0 flex-1">
                    <Link
                      to="/dashboard/$reviewId"
                      params={{ reviewId: String(review.id) }}
                      className="break-words font-medium hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-950 focus-visible:ring-offset-2"
                    >
                      {review.repo_full}#{review.pr_number}
                    </Link>
                    <p className="mt-1 break-words text-xs text-zinc-500">
                      Requested by {review.requester_login} · {formatFindingCount(review.findings_count)} · {review.model}
                    </p>
                  </div>
                  <time dateTime={createdAt.dateTime} className="shrink-0 text-right text-xs text-zinc-400">
                    {createdAt.label}
                  </time>
                </li>
              );
            })}
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
  });
  const sessionExpired = useUnauthorizedRedirect(detail.error);

  return (
    <Card aria-busy={detail.isFetching}>
      <CardHeader className="gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="grid gap-1.5">
          <CardTitle as="h1">Review #{reviewId}</CardTitle>
          <CardDescription>
            {detail.data
              ? `${detail.data.review.repo_full}#${detail.data.review.pr_number}`
              : "Review details and inline findings."}
          </CardDescription>
        </div>
        <Button variant="outline" size="sm" asChild>
          <Link to="/dashboard">
            <ArrowLeft aria-hidden="true" /> Back to reviews
          </Link>
        </Button>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
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
          <>
            <div className="flex flex-wrap items-center gap-2">
              <Badge variant={statusVariant(detail.data.review.status)}>
                {detail.data.review.status}
              </Badge>
              <span className="break-words text-xs text-zinc-500">{detail.data.review.model}</span>
            </div>
            {detail.data.review.error && (
              <p className="rounded-md border border-red-200 bg-red-50 p-3 text-red-700" role="alert">
                {detail.data.review.error}
              </p>
            )}
            <section aria-labelledby="review-summary">
              <h2 id="review-summary" className="mb-2 text-sm font-semibold">
                Summary
              </h2>
              <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-md bg-zinc-950 p-4 text-xs text-zinc-100">
                {detail.data.review.summary_md || "(No summary was returned.)"}
              </pre>
            </section>
            <section aria-labelledby="review-findings">
              <h2 id="review-findings" className="mb-2 text-sm font-semibold">
                Inline findings
              </h2>
              {detail.data.findings.length ? (
                <ul className="grid gap-2">
                  {detail.data.findings.map((finding, index) => (
                    <li
                      key={finding.id ?? `${finding.path}-${finding.line}-${finding.side}-${index}`}
                      className="rounded-md border border-zinc-200 p-3"
                    >
                      <div className="flex flex-wrap items-center gap-2 text-xs text-zinc-500">
                        <Badge variant="outline" className="break-all">
                          {finding.path}:{finding.line}
                        </Badge>
                        <Badge variant="secondary">{finding.severity || "unknown"}</Badge>
                        <span>{finding.side}</span>
                      </div>
                      <p className="mt-1.5 break-words">{finding.body}</p>
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
        ) : null}
      </CardContent>
    </Card>
  );
}

function formatDateTime(value: string) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return { label: "Unknown time" };
  return { label: date.toLocaleString(), dateTime: date.toISOString() };
}

function formatFindingCount(count: number) {
  return `${count} ${count === 1 ? "finding" : "findings"}`;
}
