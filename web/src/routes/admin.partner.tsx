import { useEffect, useState } from "react";
import { createRoute } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { BadgePercent, Check, Copy, ExternalLink } from "lucide-react";
import { Route as RootRoute } from "./__root";
import { RequireUser } from "@/components/auth-gate";
import { LoadingState, RequestError } from "@/components/query-state";
import { api, getErrorMessage } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: "/admin/partner",
  validateSearch: (search: Record<string, unknown>): { connected?: string; error?: string } => ({
    connected: typeof search.connected === "string" ? search.connected : undefined,
    error: typeof search.error === "string" ? search.error : undefined,
  }),
  component: AdminPartner,
});

function AdminPartner() {
  return (
    <RequireUser admin>
      <PartnerDashboard />
    </RequireUser>
  );
}

const ERROR_MESSAGES: Record<string, string> = {
  state: "The connect attempt could not be verified (state mismatch). Start the flow again.",
  exchange: "OrcaRouter could not exchange the authorization code for a key. Try connecting again.",
  store: "The key arrived, but it could not be stored in the pool. Try connecting again.",
  unavailable: "The connect flow is not available on this deployment.",
};

function PartnerDashboard() {
  const { connected, error } = Route.useSearch();
  const partner = useQuery({ queryKey: ["partner"], queryFn: api.partner });
  const [copied, setCopied] = useState(false);
  const [connecting, setConnecting] = useState(false);
  const [connectError, setConnectError] = useState<string | null>(null);

  // The drop-in button loader renders into the data-orca-connect container
  // from a shadow root, so re-inject it on every mount of this page.
  const info = partner.data;
  useEffect(() => {
    if (!info?.connect_script) return;
    const existing = document.querySelector<HTMLScriptElement>(`script[src="${info.connect_script}"]`);
    existing?.remove();
    const script = document.createElement("script");
    script.src = info.connect_script;
    script.async = true;
    script.crossOrigin = "anonymous";
    script.integrity = info.connect_script_integrity;
    document.body.appendChild(script);
  }, [info?.connect_script, info?.connect_script_integrity]);

  const copyReferral = async () => {
    if (!info) return;
    try {
      await navigator.clipboard.writeText(info.referral_url);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      setConnectError("Copying failed — select the referral URL manually.");
    }
  };

  const startConnect = async () => {
    setConnecting(true);
    setConnectError(null);
    try {
      const { auth_url } = await api.orcaConnectURL();
      window.location.assign(auth_url);
    } catch (error) {
      setConnectError(getErrorMessage(error, "Couldn’t start the connect flow. Try again."));
      setConnecting(false);
    }
  };

  return (
    <div className="grid gap-6" aria-busy={partner.isFetching}>
      {connected === "1" && (
        <div className="rounded-md border border-green-200 bg-green-50 p-3 text-sm text-green-800" role="status">
          OrcaRouter key connected and added to the gateway pool.
        </div>
      )}
      {error && (
        <div className="rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-800" role="alert">
          {ERROR_MESSAGES[error] ?? "The connect flow did not complete. Try again."}
        </div>
      )}
      {partner.isPending ? (
        <LoadingState>Loading partner dashboard…</LoadingState>
      ) : partner.isError ? (
        <RequestError
          title="Couldn’t load the partner dashboard"
          description={getErrorMessage(partner.error, "Try again in a moment.")}
          onRetry={() => void partner.refetch()}
        />
      ) : info && (
        <>
          <Card>
            <CardHeader>
              <CardTitle as="h1" className="flex items-center gap-2">
                <BadgePercent className="size-5" aria-hidden="true" /> OrcaRouter partner dashboard
              </CardTitle>
              <CardDescription>
                samik-bot is listed on the public “Built with OrcaRouter” directory. Your referral
                link is live and earns 5% of what referred workspaces go on to spend.
              </CardDescription>
            </CardHeader>
            <CardContent className="grid gap-3">
              <div className="grid gap-2 sm:grid-cols-[minmax(0,1fr)_auto] sm:items-end">
                <div className="grid gap-1.5">
                  <label htmlFor="referral-url" className="text-sm font-medium">
                    Your referral URL
                  </label>
                  <Input
                    id="referral-url"
                    readOnly
                    value={info.referral_url}
                    onFocus={(event) => event.currentTarget.select()}
                  />
                </div>
                <Button type="button" variant="outline" onClick={() => void copyReferral()}>
                  {copied ? (
                    <>
                      <Check className="size-4" aria-hidden="true" /> Copied
                    </>
                  ) : (
                    <>
                      <Copy className="size-4" aria-hidden="true" /> Copy
                    </>
                  )}
                </Button>
              </div>
              <p className="text-xs text-zinc-500">
                Referral code <code>{info.referral_code}</code>. Share the link anywhere; new
                signups in that link’s session are attributed automatically.
              </p>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle as="h2" className="text-base">
                Directory listing
              </CardTitle>
              <CardDescription>
                The listing credits this repository’s work. Verification looks for the OrcaRouter
                gateway entry inside the repository’s configuration code — it ships in{" "}
                <code>internal/runner/provider.go</code> (base URL <code>{info.api_base_url}/v1</code>
                ), which is what both reviewer engines load per run.
              </CardDescription>
            </CardHeader>
            <CardContent className="flex flex-wrap items-center gap-2">
              <Button variant="outline" asChild>
                <a
                  href={`https://github.com/${info.listing_repo}`}
                  target="_blank"
                  rel="noreferrer noopener"
                >
                  <ExternalLink className="size-4" aria-hidden="true" /> {info.listing_repo}
                </a>
              </Button>
              <span className="text-sm text-zinc-500">
                Route reviews through OrcaRouter by setting the default model to an{" "}
                <code>orcarouter/…</code> identifier in Settings.
              </span>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle as="h2" className="text-base">
                Connect a workspace
              </CardTitle>
              <CardDescription>
                Start the authorized connect flow to issue an OrcaRouter API key straight into the
                gateway pool. The key never passes through the browser — the server exchanges the
                authorization code and stores it encrypted.
              </CardDescription>
            </CardHeader>
            <CardContent className="grid gap-3">
              <div className="flex flex-wrap items-center gap-3">
                <Button type="button" onClick={() => void startConnect()} disabled={connecting}>
                  {connecting ? "Opening OrcaRouter…" : "Connect with OrcaRouter"}
                </Button>
                <span className="text-xs text-zinc-500">
                  Consent opens on {new URL(info.referral_url).host}; the key returns to{" "}
                  <code>{info.callback_url}</code>.
                </span>
              </div>
              {connectError && (
                <div className="rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-800" role="alert">
                  {connectError}
                </div>
              )}
              <div data-orca-connect data-endpoint="/orca/connect-url" data-auth-origin="https://www.orcarouter.ai" />
            </CardContent>
          </Card>
        </>
      )}
    </div>
  );
}
