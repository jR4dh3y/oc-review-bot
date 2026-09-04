import { useState } from "react";
import { createRoute } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Route as RootRoute } from "./__root";
import { api } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";

export const Route = createRoute({
  getParentRoute: () => RootRoute,
  path: "/admin/settings",
  component: AdminSettings,
});

function AdminSettings() {
  const qc = useQueryClient();
  const settings = useQuery({ queryKey: ["settings"], queryFn: api.settings });
  const [model, setModel] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const save = useMutation({
    mutationFn: (next: string) => api.saveSettings(next),
    onSuccess: () => {
      setSaved(true);
      void qc.invalidateQueries({ queryKey: ["settings"] });
    },
  });

  const current = model ?? settings.data?.model ?? "";

  return (
    <Card>
      <CardHeader>
        <CardTitle>Settings</CardTitle>
        <CardDescription>Default model for new reviews (free tier default shown).</CardDescription>
      </CardHeader>
      <CardContent className="grid max-w-md gap-3">
        {settings.isError ? (
          <p className="text-sm text-red-600">Admin required to view settings.</p>
        ) : (
          <>
            <Input
              placeholder="opencode/big-pickle"
              value={current}
              disabled={settings.isPending}
              onChange={(e) => {
                setModel(e.target.value);
                setSaved(false);
              }}
            />
            <div className="flex items-center gap-2">
              <Button
                disabled={!current.trim() || save.isPending}
                onClick={() => save.mutate(current.trim())}
              >
                {save.isPending ? "Saving…" : "Save"}
              </Button>
              {saved && <span className="text-sm text-green-700">Saved.</span>}
              {save.isError && (
                <span className="text-sm text-red-600">{(save.error as Error).message}</span>
              )}
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}
