import { useState, type FormEvent } from "react";
import { createRoute } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Route as RootRoute } from "./__root";
import { RequireUser } from "@/components/auth-gate";
import { LoadingState, RequestError } from "@/components/query-state";
import { api, getErrorMessage } from "@/lib/api";
import { userQueryKey, useCurrentUser, useUnauthorizedRedirect } from "@/lib/auth";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: "/admin/settings",
  component: AdminSettings,
});

function AdminSettings() {
  return (
    <RequireUser admin>
      <SettingsForm />
    </RequireUser>
  );
}

function SettingsForm() {
  const queryClient = useQueryClient();
  const currentUser = useCurrentUser();
  const userID = currentUser.data?.id;
  const settings = useQuery({
    queryKey: userID == null ? ["settings", "anonymous"] : userQueryKey("settings", userID),
    queryFn: api.settings,
    enabled: userID != null,
	});
	const [model, setModel] = useState<string | null>(null);
	const [saved, setSaved] = useState(false);

	const save = useMutation({
    mutationFn: (next: string) => api.saveSettings(next),
    onMutate: () => setSaved(false),
    onSuccess: (_, next) => {
      setModel(next);
      setSaved(true);
      if (userID != null) {
        void queryClient.invalidateQueries({ queryKey: userQueryKey("settings", userID) });
      }
		},
	});
	const sessionExpired = useUnauthorizedRedirect([settings.error, save.error]);

	const current = model ?? settings.data?.model ?? "";

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const next = current.trim();
    if (next) save.mutate(next);
  };

  return (
    <Card aria-busy={settings.isFetching || save.isPending}>
      <CardHeader>
        <CardTitle as="h1">Review settings</CardTitle>
        <CardDescription>
          Set the model ID applied to new reviews, in provider/model form. Only{" "}
          <code>orcarouter/…</code> models route through OrcaRouter; every other valid provider/model
          ID uses the built-in OpenCode Zen provider. Model availability, pricing, and account limits
          are managed by that gateway.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid max-w-md gap-3">
        {settings.isPending ? (
          <LoadingState>Loading review settings…</LoadingState>
        ) : sessionExpired ? (
          <LoadingState>Your session has ended. Returning you to sign in…</LoadingState>
        ) : settings.isError ? (
          <RequestError
            title="Couldn’t load settings"
            description={getErrorMessage(settings.error, "Try again in a moment.")}
            onRetry={() => void settings.refetch()}
          />
        ) : (
          <form className="grid gap-3" onSubmit={submit}>
            <div className="grid gap-1.5">
              <label htmlFor="default-model" className="text-sm font-medium">
                Default model
              </label>
              <Input
                id="default-model"
                name="model"
                placeholder="Provider model ID"
                value={current}
                required
                disabled={save.isPending}
                aria-describedby="default-model-help"
                onChange={(event) => {
                  setModel(event.target.value);
                  setSaved(false);
                }}
              />
              <p id="default-model-help" className="text-xs text-zinc-500">
                Use a model identifier enabled for your organization on the selected gateway, and
                keep the key pool on that gateway.
              </p>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <Button type="submit" disabled={!current.trim() || save.isPending}>
                {save.isPending ? "Saving…" : "Save"}
              </Button>
              {save.isPending && (
                <span className="text-sm text-zinc-500" role="status" aria-live="polite">
                  Saving changes…
                </span>
              )}
              {saved && !save.isPending && (
                <span className="text-sm text-green-700" role="status">
                  Saved.
                </span>
              )}
            </div>
            {save.isError && (
              <div
                className="flex flex-wrap items-center gap-2 rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-800"
                role="alert"
              >
                <span>Couldn’t save settings: {getErrorMessage(save.error, "Try again.")}</span>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={!current.trim() || save.isPending}
                  onClick={() => save.mutate(current.trim())}
                >
                  Try again
                </Button>
              </div>
            )}
          </form>
        )}
      </CardContent>
    </Card>
  );
}
