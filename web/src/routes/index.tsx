import { Link, createRoute } from "@tanstack/react-router";
import { Route as RootRoute } from "./__root";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { AtSign, KeyRound, MessagesSquare } from "lucide-react";

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: "/",
  component: Landing,
});

function Landing() {
  return (
    <div className="grid gap-6">
      <Card>
        <CardHeader>
          <CardTitle className="text-2xl">PR reviews from your Zen key pool</CardTitle>
          <CardDescription>
            Mention <code className="rounded bg-zinc-100 px-1">@oc-review-bot</code> on any pull
            request and get a summary plus inline findings.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-wrap gap-2">
          <Button asChild>
            <a href="/auth/github/login">Register with GitHub</a>
          </Button>
          <Button variant="outline" asChild>
            <Link to="/dashboard">View dashboard</Link>
          </Button>
        </CardContent>
      </Card>
      <div className="grid gap-4 sm:grid-cols-3">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <AtSign className="size-4" /> 1. Mention
            </CardTitle>
          </CardHeader>
          <CardContent className="text-sm text-zinc-600">
            Comment <code>@oc-review-bot</code> on a PR. Only registered users trigger reviews.
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <KeyRound className="size-4" /> 2. Pooled keys
            </CardTitle>
          </CardHeader>
          <CardContent className="text-sm text-zinc-600">
            Admins add Zen keys once; the bot round-robins them to stay under daily limits.
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <MessagesSquare className="size-4" /> 3. Review
            </CardTitle>
          </CardHeader>
          <CardContent className="text-sm text-zinc-600">
            One summary comment plus findings pinned to the exact diff lines.
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
