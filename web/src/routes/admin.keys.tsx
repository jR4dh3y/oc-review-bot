import { useState, type FormEvent } from "react";
import { createRoute } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Route as RootRoute } from "./__root";
import { RequireUser } from "@/components/auth-gate";
import { LoadingState, RequestError } from "@/components/query-state";
import { api, getErrorMessage } from "@/lib/api";
import { userQueryKey, useCurrentUser, useUnauthorizedRedirect } from "@/lib/auth";
import { formatDateTime } from "@/lib/format";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: "/admin/keys",
  component: AdminKeys,
});

interface KeyPatchVariables {
  id: number;
  disabled: boolean;
  label: string;
}

interface KeyDeleteVariables {
  id: number;
  label: string;
}

type FailedAction =
  | { kind: "add" }
  | { kind: "patch"; variables: KeyPatchVariables }
  | { kind: "delete"; variables: KeyDeleteVariables };

function AdminKeys() {
  return (
    <RequireUser admin>
      <ZenKeyManager />
    </RequireUser>
  );
}

function ZenKeyManager() {
  const queryClient = useQueryClient();
  const currentUser = useCurrentUser();
  const userID = currentUser.data?.id;
  const keys = useQuery({
    queryKey: userID == null ? ["keys", "anonymous"] : userQueryKey("keys", userID),
    queryFn: api.keys,
    enabled: userID != null,
  });
  const [label, setLabel] = useState("");
  const [secret, setSecret] = useState("");
  const [notice, setNotice] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [confirmDeleteId, setConfirmDeleteId] = useState<number | null>(null);
  const [failedAction, setFailedAction] = useState<FailedAction | null>(null);

  const refreshKeys = () =>
    userID == null ? Promise.resolve() : queryClient.invalidateQueries({ queryKey: userQueryKey("keys", userID) });

  const add = useMutation({
    // Keep the secret out of React Query's mutation cache while it is being submitted.
    mutationFn: () => api.addKey(label.trim() || "zen-key", secret.trim()),
    onMutate: () => {
      setNotice(null);
      setActionError(null);
      setFailedAction(null);
    },
    onSuccess: (key) => {
      setLabel("");
      setSecret("");
      setNotice(`Credential “${key.label}” was added.`);
      void refreshKeys();
    },
    onError: (error) => {
      setActionError(getErrorMessage(error, "Couldn’t add the credential. Try again."));
      setFailedAction({ kind: "add" });
    },
  });
  const patch = useMutation({
    mutationFn: ({ id, disabled }: KeyPatchVariables) =>
      api.patchKey(id, disabled),
    onMutate: () => {
      setNotice(null);
      setActionError(null);
      setFailedAction(null);
    },
    onSuccess: (_, variables) => {
      setNotice(
        `Credential “${variables.label}” was ${variables.disabled ? "disabled" : "enabled"}.`,
      );
      void refreshKeys();
    },
    onError: (error, variables) => {
      setActionError(getErrorMessage(error, "Couldn’t update the credential. Try again."));
      setFailedAction({ kind: "patch", variables });
    },
  });
  const del = useMutation({
    mutationFn: ({ id }: KeyDeleteVariables) => api.deleteKey(id),
    onMutate: () => {
      setNotice(null);
      setActionError(null);
      setFailedAction(null);
    },
    onSuccess: (_, variables) => {
      setConfirmDeleteId(null);
      setNotice(`Credential “${variables.label}” was deleted.`);
      void refreshKeys();
    },
    onError: (error, variables) => {
      setActionError(getErrorMessage(error, "Couldn’t delete the credential. Try again."));
      setFailedAction({ kind: "delete", variables });
    },
  });
	const sessionExpired = useUnauthorizedRedirect([keys.error, add.error, patch.error, del.error]);

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const nextSecret = secret.trim();
    if (!nextSecret) return;
    add.mutate();
  };

  const retryAction = () => {
    if (!failedAction) return;

    if (failedAction.kind === "add") {
      const nextSecret = secret.trim();
      if (!nextSecret) {
        setActionError("Enter an API key before retrying.");
        return;
      }
      add.mutate();
      return;
    }
    if (failedAction.kind === "patch") {
      patch.mutate(failedAction.variables);
      return;
    }
    del.mutate(failedAction.variables);
  };

  const clearAddError = () => {
    if (failedAction?.kind !== "add") return;
    add.reset();
    setActionError(null);
    setFailedAction(null);
  };

  return (
    <Card aria-busy={keys.isFetching || add.isPending || patch.isPending || del.isPending}>
      <CardHeader>
        <CardTitle as="h1">Gateway credentials</CardTitle>
        <CardDescription>
          API keys for the configured review gateway — OpenCode Zen for <code>opencode/…</code>{" "}
          models, OrcaRouter for <code>orcarouter/…</code> models. Credential values are encrypted
          at rest and never returned to the browser; only masked suffixes appear here.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4">
        <aside
          className="rounded-md border border-amber-200 bg-amber-50 p-3 text-sm text-amber-950"
          aria-label="Credential usage notice"
        >
          Use only credentials your organization is authorized to manage. Tracking usage or rotating
          credentials does not increase provider capacity or bypass provider quotas, billing, or plan
          limits.
        </aside>
        <form className="grid gap-3" onSubmit={submit}>
          <div className="grid gap-2 sm:grid-cols-[minmax(0,12rem)_minmax(0,1fr)_auto] sm:items-end">
            <div className="grid gap-1.5">
              <label htmlFor="zen-key-label" className="text-sm font-medium">
                Label <span className="font-normal text-zinc-500">(optional)</span>
              </label>
              <Input
                id="zen-key-label"
                name="label"
                placeholder="e.g. production account"
                value={label}
                disabled={add.isPending}
                onChange={(event) => {
                  setLabel(event.target.value);
                  clearAddError();
                }}
              />
            </div>
            <div className="grid gap-1.5">
              <label htmlFor="zen-key-secret" className="text-sm font-medium">
                Gateway API key
              </label>
              <Input
                id="zen-key-secret"
                name="secret"
                placeholder="Paste an API key"
                type="password"
                autoComplete="off"
                required
                aria-invalid={Boolean(add.isError)}
                value={secret}
                disabled={add.isPending}
                onChange={(event) => {
                  setSecret(event.target.value);
                  clearAddError();
                }}
              />
            </div>
            <Button type="submit" disabled={!secret.trim() || add.isPending}>
              {add.isPending ? "Adding…" : "Add key"}
            </Button>
          </div>
        </form>
        {notice && (
          <p className="text-sm text-green-700" role="status">
            {notice}
          </p>
        )}
        {actionError && (
          <div
            className="flex flex-wrap items-center gap-2 rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-800"
            role="alert"
          >
            <span>{actionError}</span>
            <Button type="button" variant="outline" size="sm" onClick={retryAction} disabled={!failedAction}>
              Try again
            </Button>
          </div>
        )}
        {keys.isPending ? (
          <LoadingState>Loading credentials…</LoadingState>
        ) : sessionExpired ? (
          <LoadingState>Your session has ended. Returning you to sign in…</LoadingState>
        ) : keys.isError ? (
          <RequestError
            title="Couldn’t load credentials"
            description={getErrorMessage(keys.error, "Try again in a moment.")}
            onRetry={() => void keys.refetch()}
          />
        ) : keys.data?.length ? (
          <ul className="divide-y divide-zinc-100" aria-label="Saved gateway credentials">
            {keys.data.map((key) => {
              const cooling =
                key.cooldown_until != null && new Date(key.cooldown_until) > new Date();
              const cooldownLabel = key.cooldown_until ? formatDateTime(key.cooldown_until) : null;
              const updating = patch.isPending && patch.variables?.id === key.id;
              const deleting = del.isPending && del.variables?.id === key.id;
              const actionsDisabled = patch.isPending || del.isPending;

              return (
                <li key={key.id} className="flex flex-col gap-3 py-3 text-sm sm:flex-row sm:items-center">
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="break-words font-medium">{key.label}</span>
                      <span className="text-zinc-500" aria-label={`Credential ending in ${key.last4}`}>
                        ••••{key.last4}
                      </span>
                      <Badge variant="secondary">
                        {key.requests_today} {key.requests_today === 1 ? "request" : "requests"} today
                        {" "}(UTC)
                      </Badge>
                      {cooling && <Badge variant="warning">Temporarily unavailable</Badge>}
                      {key.disabled && <Badge variant="destructive">Disabled</Badge>}
                    </div>
                    {cooling && cooldownLabel && (
                      <p className="mt-1 text-xs text-amber-800">Available after {cooldownLabel}</p>
                    )}
                  </div>
                  {confirmDeleteId === key.id ? (
                    <div
                      className="flex flex-wrap items-center gap-2 rounded-md border border-red-200 bg-red-50 p-2 text-red-800"
                      role="alert"
                    >
                      <span>Delete “{key.label}”? This cannot be undone.</span>
                      <Button
                        type="button"
                        variant="destructive"
                        size="sm"
                        disabled={deleting}
                        onClick={() => del.mutate({ id: key.id, label: key.label })}
                      >
                        {deleting ? "Deleting…" : "Confirm delete"}
                      </Button>
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        disabled={deleting}
                        onClick={() => setConfirmDeleteId(null)}
                      >
                        Cancel
                      </Button>
                    </div>
                  ) : (
                    <div
                      className="flex flex-wrap gap-1 sm:justify-end"
                      role="group"
                      aria-label={`Actions for ${key.label}`}
                    >
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        disabled={actionsDisabled}
                        onClick={() =>
                          patch.mutate({
                            id: key.id,
                            disabled: !key.disabled,
                            label: key.label,
                          })
                        }
                      >
                        {updating ? "Saving…" : key.disabled ? "Enable" : "Disable"}
                      </Button>
                      <Button
                        type="button"
                        variant="ghost"
                        size="sm"
                        disabled={actionsDisabled}
                        onClick={() => setConfirmDeleteId(key.id)}
                      >
                        Delete
                      </Button>
                    </div>
                  )}
                </li>
              );
            })}
          </ul>
        ) : (
          <div className="rounded-md border border-dashed border-zinc-300 p-4 text-sm text-zinc-600">
            <p className="font-medium text-zinc-900">No credentials saved.</p>
            <p className="mt-1">
              Add an organization-authorized gateway API key that matches the configured review
              model to enable reviews.
            </p>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
