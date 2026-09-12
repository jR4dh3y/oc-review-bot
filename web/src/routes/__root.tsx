import { useEffect } from "react";
import { Link, Outlet, createRootRoute } from "@tanstack/react-router";
import { useMutation } from "@tanstack/react-query";
import { BadgePercent, BotMessageSquare, KeyRound, LayoutDashboard, LogOut, Settings2 } from "lucide-react";
import { api, getErrorMessage, isUnauthenticatedError } from "@/lib/api";
import { notifyAuthChange, useAuthSessionSync, useCurrentUser } from "@/lib/auth";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

export const Route = createRootRoute({
  component: RootLayout,
});

function RootLayout() {
	const currentUser = useCurrentUser();
	const authSession = useAuthSessionSync(currentUser.data?.id, currentUser.error);
	const me = authSession.isTransitioning || currentUser.isError ? undefined : currentUser.data;

	useEffect(() => {
		const url = new URL(window.location.href);
		if (url.searchParams.get("auth") !== "oauth") return;
		url.searchParams.delete("auth");
		window.history.replaceState(window.history.state, "", `${url.pathname}${url.search}${url.hash}`);
		notifyAuthChange("identity-changed");
	}, []);

	const logout = useMutation({
		mutationFn: api.logout,
		onSuccess: () => {
			notifyAuthChange("logout");
		},
  });

  return (
    <div className="min-h-screen bg-zinc-50 text-zinc-950">
      <a
        href="#main-content"
        className="absolute -top-12 left-4 z-20 rounded-md bg-zinc-900 px-3 py-2 text-sm font-medium text-white focus:top-3"
      >
        Skip to content
      </a>
      <header className="sticky top-0 z-20 border-b border-zinc-200 bg-white">
        <div className="mx-auto flex min-h-14 max-w-5xl flex-wrap items-center gap-x-2 gap-y-2 px-4 py-2 sm:flex-nowrap sm:py-0">
          <Link
            to="/"
            className="order-1 flex shrink-0 items-center gap-2 rounded-sm font-semibold focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-950 focus-visible:ring-offset-2"
          >
            <BotMessageSquare className="size-5" aria-hidden="true" />
            samik-bot
          </Link>
          {me && (
            <nav
              aria-label="Primary"
              className="order-3 -mx-1 flex w-full items-center gap-1 overflow-x-auto px-1 pb-0.5 text-sm sm:order-2 sm:ml-6 sm:w-auto sm:pb-0"
            >
              <Link
                to="/dashboard"
                className="shrink-0 rounded-md px-3 py-1.5 hover:bg-zinc-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-950 focus-visible:ring-offset-2 [&.active]:bg-zinc-900 [&.active]:text-white"
              >
                <span className="inline-flex items-center gap-1.5">
                  <LayoutDashboard className="size-4" aria-hidden="true" /> Dashboard
                </span>
              </Link>
              {me.is_admin && (
                <>
                  <Link
                    to="/admin/keys"
                    className="shrink-0 rounded-md px-3 py-1.5 hover:bg-zinc-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-950 focus-visible:ring-offset-2 [&.active]:bg-zinc-900 [&.active]:text-white"
                  >
                    <span className="inline-flex items-center gap-1.5">
                      <KeyRound className="size-4" aria-hidden="true" /> Keys
                    </span>
                  </Link>
                  <Link
                    to="/admin/settings"
                    className="shrink-0 rounded-md px-3 py-1.5 hover:bg-zinc-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-950 focus-visible:ring-offset-2 [&.active]:bg-zinc-900 [&.active]:text-white"
                  >
                    <span className="inline-flex items-center gap-1.5">
                      <Settings2 className="size-4" aria-hidden="true" /> Settings
                    </span>
                  </Link>
                  <Link
                    to="/admin/partner"
                    className="shrink-0 rounded-md px-3 py-1.5 hover:bg-zinc-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-950 focus-visible:ring-offset-2 [&.active]:bg-zinc-900 [&.active]:text-white"
                  >
                    <span className="inline-flex items-center gap-1.5">
                      <BadgePercent className="size-4" aria-hidden="true" /> Partner
                    </span>
                  </Link>
                </>
              )}
            </nav>
          )}
          <div className="order-2 ml-auto flex flex-col items-end gap-1 sm:order-3">
            <div className="flex items-center gap-2">
              {me ? (
                <>
                  <span className={cn("hidden text-sm text-zinc-500 sm:inline")}>{me.login}</span>
                  {me.avatar_url && (
                    <img src={me.avatar_url} alt="" className="size-7 rounded-full border" />
                  )}
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    onClick={() => logout.mutate()}
                    disabled={logout.isPending}
                  >
                    <LogOut aria-hidden="true" /> Logout
                  </Button>
                </>
              ) : currentUser.isPending ? (
                <span className="text-xs text-zinc-500" role="status" aria-live="polite">
                  Checking session…
                </span>
              ) : currentUser.isError && !isUnauthenticatedError(currentUser.error) ? (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => void currentUser.refetch()}
                  disabled={currentUser.isFetching}
                >
                  Retry session
                </Button>
              ) : (
                <Button size="sm" asChild>
                  <a href="/auth/github/login">Login with GitHub</a>
                </Button>
              )}
            </div>
            {logout.isError && (
              <p className="max-w-52 text-right text-xs text-red-700" role="alert">
                Couldn’t sign out. {getErrorMessage(logout.error, "Try again.")}
              </p>
            )}
          </div>
        </div>
      </header>
      <main
        id="main-content"
        tabIndex={-1}
        className="mx-auto max-w-5xl px-4 py-6 focus:outline-none sm:py-8"
      >
		{authSession.isTransitioning ? (
			<div className="py-8 text-sm text-zinc-500" role="status" aria-live="polite">
				Refreshing your session…
			</div>
		) : (
			<Outlet key={authSession.resetKey} />
		)}
		</main>
    </div>
  );
}
