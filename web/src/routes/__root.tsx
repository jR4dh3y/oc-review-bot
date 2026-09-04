import { Link, Outlet, createRootRoute, useNavigate } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { BotMessageSquare, LayoutDashboard, KeyRound, Settings2, LogOut } from "lucide-react";
import { api } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

export const Route = createRootRoute({
  component: RootLayout,
});

function RootLayout() {
  const qc = useQueryClient();
  const meQuery = useQuery({ queryKey: ["me"], queryFn: api.me, retry: false });
  const me = meQuery.data;
  const navigate = useNavigate();

  const logout = async () => {
    await api.logout().catch(() => undefined);
    qc.removeQueries({ queryKey: ["me"] });
    void navigate({ to: "/" });
  };

  return (
    <div className="min-h-screen bg-zinc-50 text-zinc-950">
      <header className="sticky top-0 z-10 border-b border-zinc-200 bg-white/80 backdrop-blur">
        <div className="mx-auto flex h-14 max-w-5xl items-center gap-2 px-4">
          <Link to="/" className="flex items-center gap-2 font-semibold">
            <BotMessageSquare className="size-5" />
            oc-review-bot
          </Link>
          <nav className="ml-6 flex items-center gap-1 text-sm">
            {me && (
              <Link
                to="/dashboard"
                className="rounded-md px-3 py-1.5 hover:bg-zinc-100 [&.active]:bg-zinc-900 [&.active]:text-white"
              >
                <span className="inline-flex items-center gap-1.5">
                  <LayoutDashboard className="size-4" /> Dashboard
                </span>
              </Link>
            )}
            {me?.is_admin && (
              <>
                <Link
                  to="/admin/keys"
                  className="rounded-md px-3 py-1.5 hover:bg-zinc-100 [&.active]:bg-zinc-900 [&.active]:text-white"
                >
                  <span className="inline-flex items-center gap-1.5">
                    <KeyRound className="size-4" /> Keys
                  </span>
                </Link>
                <Link
                  to="/admin/settings"
                  className="rounded-md px-3 py-1.5 hover:bg-zinc-100 [&.active]:bg-zinc-900 [&.active]:text-white"
                >
                  <span className="inline-flex items-center gap-1.5">
                    <Settings2 className="size-4" /> Settings
                  </span>
                </Link>
              </>
            )}
          </nav>
          <div className="ml-auto flex items-center gap-2">
            {me ? (
              <>
                <span className={cn("hidden text-sm text-zinc-500 sm:inline")}>{me.login}</span>
                {me.avatar_url && (
                  <img src={me.avatar_url} alt={me.login} className="size-7 rounded-full border" />
                )}
                <Button variant="ghost" size="sm" onClick={logout}>
                  <LogOut /> Logout
                </Button>
              </>
            ) : (
              <Button size="sm" asChild>
                <a href="/auth/github/login">Login with GitHub</a>
              </Button>
            )}
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-5xl px-4 py-8">
        <Outlet />
      </main>
    </div>
  );
}
