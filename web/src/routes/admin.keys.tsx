import { useState } from "react";
import { createRoute } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Route as RootRoute } from "./__root";
import { api } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: "/admin/keys",
  component: AdminKeys,
});

function AdminKeys() {
  const qc = useQueryClient();
  const keys = useQuery({ queryKey: ["keys"], queryFn: api.keys });
  const [label, setLabel] = useState("");
  const [secret, setSecret] = useState("");
  const [error, setError] = useState<string | null>(null);

  const add = useMutation({
    mutationFn: () => api.addKey(label.trim() || "zen-key", secret.trim()),
    onSuccess: () => {
      setLabel("");
      setSecret("");
      setError(null);
      void qc.invalidateQueries({ queryKey: ["keys"] });
    },
    onError: (e: Error) => setError(e.message),
  });
  const patch = useMutation({
    mutationFn: ({ id, disabled }: { id: number; disabled: boolean }) =>
      api.patchKey(id, disabled),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["keys"] }),
  });
  const del = useMutation({
    mutationFn: (id: number) => api.deleteKey(id),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["keys"] }),
  });

  return (
    <div className="grid gap-4">
      <Card>
        <CardHeader>
          <CardTitle>Zen keys</CardTitle>
          <CardDescription>
            Secrets are stored encrypted and never returned — only the last 4 show here.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-3">
          <div className="flex flex-col gap-2 sm:flex-row">
            <Input
              placeholder="Label (e.g. key-1)"
              value={label}
              onChange={(e) => setLabel(e.target.value)}
              className="sm:max-w-48"
            />
            <Input
              placeholder="sk-…"
              type="password"
              value={secret}
              onChange={(e) => setSecret(e.target.value)}
              className="flex-1"
            />
            <Button disabled={!secret.trim() || add.isPending} onClick={() => add.mutate()}>
              {add.isPending ? "Adding…" : "Add key"}
            </Button>
          </div>
          {error && <p className="text-sm text-red-600">{error}</p>}
          {keys.isError && <p className="text-sm text-red-600">Admin required to manage keys.</p>}
          <ul className="divide-y divide-zinc-100">
            {keys.data?.map((k) => {
              const cooling =
                k.cooldown_until != null && new Date(k.cooldown_until) > new Date();
              return (
                <li key={k.id} className="flex items-center gap-2 py-2.5 text-sm">
                  <span className="font-medium">{k.label}</span>
                  <span className="text-zinc-400">•••{k.last4}</span>
                  <Badge variant="secondary">{k.requests_today} today</Badge>
                  {cooling && <Badge variant="warning">cooling down</Badge>}
                  {k.disabled && <Badge variant="destructive">disabled</Badge>}
                  <span className="ml-auto flex gap-1">
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => patch.mutate({ id: k.id, disabled: !k.disabled })}
                    >
                      {k.disabled ? "Enable" : "Disable"}
                    </Button>
                    <Button variant="ghost" size="sm" onClick={() => del.mutate(k.id)}>
                      Delete
                    </Button>
                  </span>
                </li>
              );
            })}
          </ul>
        </CardContent>
      </Card>
    </div>
  );
}
