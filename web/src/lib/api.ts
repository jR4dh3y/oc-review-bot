export interface Me {
  id: number;
  login: string;
  avatar_url: string;
  is_admin: boolean;
}

export type ReviewStatus = "queued" | "running" | "done" | "failed" | (string & {});

export interface ReviewListItem {
  id: number;
  repo_full: string;
  pr_number: number;
  head_sha: string;
  requester_login: string;
  status: ReviewStatus;
  model: string;
  summary_md: string;
  error: string;
  summary_comment_id: number;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
  findings_count: number;
}

export interface ReviewEvent {
  id: number;
  kind: string;
  message: string;
  created_at: string;
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

interface FindingResponse {
  id?: number;
  ID?: number;
  review_id?: number;
  ReviewID?: number;
  path?: string;
  Path?: string;
  line?: number;
  Line?: number;
  side?: string;
  Side?: string;
  severity?: string;
  Severity?: string;
  body?: string;
  body_md?: string;
  BodyMD?: string;
  posted_comment_id?: number;
  PostedCommentID?: number;
}

export interface ReviewDetail {
  review: ReviewListItem;
  findings: Finding[];
  events: ReviewEvent[];
}

export interface PartnerInfo {
  referral_code: string;
  referral_url: string;
  app_name: string;
  callback_url: string;
  api_base_url: string;
  listing_repo: string;
  connect_script: string;
  connect_script_integrity: string;
}

export interface ConnectURLResponse {
  auth_url: string;
}

interface ReviewDetailResponse {
  review: ReviewListItem;
  findings: FindingResponse[] | null;
  events?: ReviewEvent[] | null;
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

export interface Settings {
  model: string;
}

export interface Meta {
  bot_username: string;
}

export interface KeyCreateResult {
  id: number;
  label: string;
  last4: string;
}

export interface ActionResult {
  ok: string;
}

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export const authenticationRequiredEvent = "samik-bot:authentication-required";

export function notifyAuthenticationRequired() {
	if (typeof window !== "undefined") {
		window.dispatchEvent(new Event(authenticationRequiredEvent));
	}
}

function isErrorResponse(value: unknown): value is { error: string } {
  return (
    typeof value === "object" &&
    value !== null &&
    "error" in value &&
    typeof (value as { error?: unknown }).error === "string"
  );
}

async function responseErrorMessage(res: Response) {
  const body = await res.json().catch(() => null);
  return isErrorResponse(body) ? body.error : res.statusText || `Request failed (${res.status})`;
}

async function req<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body != null && !headers.has("content-type")) {
    headers.set("content-type", "application/json");
  }

  const res = await fetch(path, {
    ...init,
    credentials: "same-origin",
    headers,
	});
	if (!res.ok) {
		const error = new ApiError(await responseErrorMessage(res), res.status);
		if (error.status === 401) notifyAuthenticationRequired();
		throw error;
	}
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export function isApiError(error: unknown): error is ApiError {
  return error instanceof ApiError;
}

export function isUnauthenticatedError(error: unknown) {
  return isApiError(error) && error.status === 401;
}

export function isRetryableError(error: unknown) {
  if (!isApiError(error)) return true;
  return error.status === 408 || error.status === 429 || error.status >= 500;
}

export function getErrorMessage(error: unknown, fallback: string) {
  return error instanceof Error && error.message ? error.message : fallback;
}

function normalizeFinding(finding: FindingResponse): Finding {
  return {
    id: finding.id ?? finding.ID,
    review_id: finding.review_id ?? finding.ReviewID,
    path: finding.path ?? finding.Path ?? "",
    line: finding.line ?? finding.Line ?? 0,
    side: finding.side ?? finding.Side ?? "",
    severity: finding.severity ?? finding.Severity ?? "",
    body: finding.body ?? finding.body_md ?? finding.BodyMD ?? "",
    posted_comment_id: finding.posted_comment_id ?? finding.PostedCommentID,
  };
}

export const api = {
  me: () => req<Me>("/api/me"),
  meta: () => req<Meta>("/api/meta"),
  partner: () => req<PartnerInfo>("/api/admin/partner"),
  orcaConnectURL: () => req<ConnectURLResponse>("/orca/connect-url"),
  reviews: () => req<ReviewListItem[]>("/api/reviews"),
  reviewDetail: async (id: number | string): Promise<ReviewDetail> => {
    const detail = await req<ReviewDetailResponse>(`/api/reviews/${id}`);
    return {
      review: detail.review,
      findings: (detail.findings ?? []).map(normalizeFinding),
      events: detail.events ?? [],
    };
  },
  keys: () => req<ZenKey[]>("/api/admin/keys"),
  addKey: (label: string, secret: string) =>
    req<KeyCreateResult>("/api/admin/keys", {
      method: "POST",
      body: JSON.stringify({ label, secret }),
    }),
  patchKey: (id: number, disabled: boolean) =>
    req<ActionResult>(`/api/admin/keys/${id}`, {
      method: "PATCH",
      body: JSON.stringify({ disabled }),
    }),
  deleteKey: (id: number) =>
    req<ActionResult>(`/api/admin/keys/${id}`, {
      method: "DELETE",
      body: JSON.stringify({}),
    }),
  settings: () => req<Settings>("/api/admin/settings"),
  saveSettings: (model: string) =>
    req<ActionResult>("/api/admin/settings", {
      method: "POST",
      body: JSON.stringify({ model }),
    }),
  logout: () => req<ActionResult>("/auth/logout", { method: "POST" }),
};
