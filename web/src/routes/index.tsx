import { Link, createRoute } from "@tanstack/react-router";
import { AtSign, KeyRound, MessagesSquare } from "lucide-react";
import { Route as RootRoute } from "./__root";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { useCurrentUser } from "@/lib/auth";
import { BotMention } from "@/components/bot-mention";

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: "/",
  component: Landing,
});

function Landing() {
  const currentUser = useCurrentUser();

  return (
    <div className="grid gap-6">
      <Card>
        <CardHeader>
          <CardTitle as="h1" className="text-2xl">
            GitHub pull-request reviews with OpenCode Zen
          </CardTitle>
          <CardDescription>
            Sign in once with GitHub, then mention <BotMention /> on a pull request in
            an installed repository to receive a summary and inline findings.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-wrap gap-2">
          {currentUser.data ? (
            <Button asChild>
              <Link to="/dashboard">Go to dashboard</Link>
            </Button>
          ) : (
            <Button asChild>
              <a href="/auth/github/login">Register with GitHub</a>
            </Button>
          )}
          <Button variant="outline" asChild>
            <a href="#how-it-works">How it works</a>
          </Button>
        </CardContent>
      </Card>
      <section
        id="how-it-works"
        aria-labelledby="how-it-works-title"
        className="grid gap-4 sm:grid-cols-3"
      >
        <h2 id="how-it-works-title" className="sr-only">
          How it works
        </h2>
        <Card>
          <CardHeader>
            <CardTitle as="h2" className="flex items-center gap-2 text-base">
              <AtSign className="size-4" aria-hidden="true" /> 1. Mention
            </CardTitle>
          </CardHeader>
          <CardContent className="text-sm text-zinc-600">
            Comment <BotMention /> on a PR. Only GitHub accounts that have registered
            through this site can request a review.
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle as="h2" className="flex items-center gap-2 text-base">
              <KeyRound className="size-4" aria-hidden="true" /> 2. Authorized credentials
            </CardTitle>
          </CardHeader>
          <CardContent className="text-sm text-zinc-600">
            Administrators manage organization-authorized OpenCode Zen credentials. Provider pricing,
            capacity, and account limits still apply.
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle as="h2" className="flex items-center gap-2 text-base">
              <MessagesSquare className="size-4" aria-hidden="true" /> 3. Review
            </CardTitle>
          </CardHeader>
          <CardContent className="text-sm text-zinc-600">
            One summary comment plus findings pinned to the exact diff lines.
          </CardContent>
        </Card>
      </section>
    </div>
  );
}
