import { Link, createRoute, useParams } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { Route as RootRoute } from "./__root";
import { api } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: "/dashboard",
  component: Dashboard,
});

function statusVariant(status: string) {
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
  const reviews = useQuery({
    queryKey: ["reviews"],
    queryFn: api.reviews,
    refetchInterval: 5000,
  });

  return (
    <div className="grid gap-4">
      <Card>
        <CardHeader>
          <CardTitle>Reviews</CardTitle>
          <CardDescription>Latest 50 reviews, auto-refreshing every 5s.</CardDescription>
        </CardHeader>
        <CardContent>
          {reviews.isPending && <p className="text-sm text-zinc-500">Loading…</p>}
          {reviews.isError && (
            <p className="text-sm">
              Login required.{" "}
              <a className="underline" href="/auth/github/login">
                Login with GitHub
              </a>
            </p>
          )}
          <ul className="divide-y divide-zinc-100">
            {reviews.data?.map((r) => (
              <li key={r.id} className="flex items-center gap-3 py-2.5 text-sm">
                <Badge variant={statusVariant(r.status)}>{r.status}</Badge>
                <Link to="/dashboard/$reviewId" params={{ reviewId: String(r.id) }} className="font-medium hover:underline">
                  {r.repo_full}#{r.pr_number}
                </Link>
                <span className="hidden text-zinc-500 sm:inline">
                  by {r.requester_login} · {r.findings_count} findings · {r.model}
                </span>
                <span className="ml-auto shrink-0 text-xs text-zinc-400">
                  {new Date(r.created_at).toLocaleString()}
                </span>
              </li>
            ))}
          </ul>
          {reviews.data?.length === 0 && (
            <p className="text-sm text-zinc-500">
              No reviews yet. Comment <code>@oc-review-bot</code> on a PR to trigger one.
            </p>
          )}
        </CardContent>
      </Card>
      <ReviewDetailPanel />
    </div>
  );
}

function ReviewDetailPanel() {
  const params = useParams({ strict: false }) as { reviewId?: string };
  const reviewId = params.reviewId;
  const detail = useQuery({
    queryKey: ["review", reviewId],
    queryFn: () => api.reviewDetail(reviewId as string),
    enabled: Boolean(reviewId),
  });
  if (!reviewId) return null;
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Review #{reviewId}</CardTitle>
        <CardDescription>{detail.data?.review.summary_md?.slice(0, 160)}</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm">
        {detail.isPending && <p className="text-zinc-500">Loading detail…</p>}
        {detail.data && (
          <>
            <pre className="whitespace-pre-wrap rounded-md bg-zinc-950 p-4 text-xs text-zinc-100">
              {detail.data.review.summary_md || "(no summary)"}
            </pre>
            {detail.data.review.error && (
              <p className="rounded-md bg-red-50 p-3 text-red-700">{detail.data.review.error}</p>
            )}
            <ul className="grid gap-2">
              {detail.data.findings.map((f, i) => (
                <li key={i} className="rounded-md border border-zinc-200 p-3">
                  <div className="flex items-center gap-2 text-xs text-zinc-500">
                    <Badge variant="outline">
                      {f.path}:{f.line}
                    </Badge>
                    <Badge variant="secondary">{f.severity}</Badge>
                    <span>{f.side}</span>
                  </div>
                  <p className="mt-1.5">{f.body}</p>
                </li>
              ))}
            </ul>
          </>
        )}
      </CardContent>
    </Card>
  );
}

export const ReviewDetailRoute = createRoute({
  getParentRoute: () => RootRoute,
  path: "/dashboard/$reviewId",
  component: Dashboard,
});
