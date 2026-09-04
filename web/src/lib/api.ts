export interface Me {
  id: number;
  login: string;
  avatar_url: string;
  is_admin: boolean;
}

export interface ReviewListItem {
  id: number;
  repo_full: string;
  pr_number: number;
  head_sha: string;
  requester_login: string;
  status: "queued" | "running" | "done" | "failed" | string;
  model: string;
  summary_md: string;
  error: string;
  summary_comment_id: number;
  created_at: string;
  findings_count: number;
}

export interface Finding {
  id?: number;
  review_id?: number;
  path: string;
  line: number;
  side: string;
  severity: string;
  body: string;
  posted_comment_id?: number;
}

export interface ReviewDetail {
  review: ReviewListItem;
  findings: Finding[];
}

export interface ZenKey {
  id: number;
  label: string;
  last4: string;
  requests_today: number;
  cooldown_until: string | null;
  disabled: boolean;
  created_at: string;
}

async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    credentials: "same-origin",
    headers: { "content-type": "application/json" },
    ...init,
  });
  if (res.status === 401) throw new Error("unauthorized");
  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error((body as { error?: string }).error ?? `request failed: ${res.status}`);
  }
  return (await res.json()) as T;
}

export const api = {
  me: () => req<Me>("/api/me"),
  reviews: () => req<ReviewListItem[]>("/api/reviews"),
  reviewDetail: (id: number | string) => req<ReviewDetail>(`/api/reviews/${id}`),
  keys: () => req<ZenKey[]>("/api/admin/keys"),
  addKey: (label: string, secret: string) =>
    req<{ id: number; label: string; last4: string }>("/api/admin/keys", {
      method: "POST",
      body: JSON.stringify({ label, secret }),
    }),
  patchKey: (id: number, disabled: boolean) =>
    req<{ ok: string }>(`/api/admin/keys/${id}`, {
      method: "PATCH",
      body: JSON.stringify({ disabled }),
    }),
  deleteKey: (id: number) =>
    req<{ ok: string }>(`/api/admin/keys/${id}`, { method: "DELETE" }),
  settings: () => req<{ model: string }>("/api/admin/settings"),
  saveSettings: (model: string) =>
    req<{ ok: string }>("/api/admin/settings", {
      method: "POST",
      body: JSON.stringify({ model }),
    }),
  logout: () => req<{ ok: string }>("/auth/logout", { method: "POST" }),
};
