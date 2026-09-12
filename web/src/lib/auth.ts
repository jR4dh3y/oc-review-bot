import { useCallback, useEffect, useRef, useState } from "react";
import { queryOptions, type QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import {
  api,
  authenticationRequiredEvent,
  isUnauthenticatedError,
  notifyAuthenticationRequired,
} from "@/lib/api";

export const meQueryKey = ["me"] as const;
const authChangeStorageKey = "samik-bot-auth-change";

type AuthChange = "logout" | "identity-changed";

interface AuthSessionSync {
  isTransitioning: boolean;
  resetKey: number;
}

interface AuthTransition {
  id: number;
}

export function userQueryKey(scope: string, userID: number) {
  return [scope, userID] as const;
}

export function userReviewQueryKey(userID: number, reviewID: string | number) {
  return ["review", userID, String(reviewID)] as const;
}

function isCurrentUserQuery(queryKey: readonly unknown[]) {
  return queryKey.length === meQueryKey.length && queryKey.every((part, index) => part === meQueryKey[index]);
}

export function clearProtectedQueryCache(queryClient: QueryClient) {
  // Keep the mounted /me observer attached; transitions reset it separately.
  queryClient.removeQueries({ predicate: (query) => !isCurrentUserQuery(query.queryKey) });
  queryClient.getMutationCache().clear();
}

export function publishAuthChange(type: AuthChange) {
  if (typeof window === "undefined") return;
  try {
    // This event contains no session or account data.
    window.localStorage.setItem(
      authChangeStorageKey,
      JSON.stringify({ type, nonce: `${Date.now()}-${Math.random()}` }),
    );
  } catch {
    // Some privacy modes deny storage; the initiating tab still clears itself.
  }
}

export function notifyAuthChange(type: AuthChange) {
  if (typeof window === "undefined") return;
  publishAuthChange(type);
  window.dispatchEvent(new CustomEvent<AuthChange>("samik-bot:auth-change", { detail: type }));
}

export const meQueryOptions = () =>
  queryOptions({
    queryKey: meQueryKey,
    queryFn: api.me,
    retry: false,
  });

export function useCurrentUser() {
  return useQuery(meQueryOptions());
}

export function useAuthSessionSync(userID: number | undefined, authError?: unknown): AuthSessionSync {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [transition, setTransition] = useState<AuthTransition | null>(null);
  const [resetKey, setResetKey] = useState(0);
  const previousUserID = useRef<number | undefined>(undefined);
  const transitionID = useRef(0);
  const handlingUnauthenticated = useRef(false);

  const beginTransition = useCallback(
    (type: AuthChange, options: { broadcast?: boolean; unauthenticated?: boolean } = {}) => {
      if (options.unauthenticated) {
        if (handlingUnauthenticated.current) return;
        handlingUnauthenticated.current = true;
      } else if (type === "identity-changed") {
        handlingUnauthenticated.current = false;
      }

      const id = ++transitionID.current;
      setResetKey((key) => key + 1);
      setTransition({ id });
      if (options.broadcast) publishAuthChange(type);
      if (type === "logout") void navigate({ to: "/", replace: true }).catch(() => undefined);
    },
    [navigate],
  );

  useEffect(() => {
    if (transition == null) return;
    let active = true;
    const { id } = transition;

    void (async () => {
      // This effect runs only after the transition render has unmounted routes
      // that could observe protected cache entries.
      try {
        await queryClient.cancelQueries();
      } catch {
        // Cancellation is best effort; stale protected data must still be removed.
      }
      if (!active || transitionID.current !== id) return;
      clearProtectedQueryCache(queryClient);
      try {
        await queryClient.resetQueries({ queryKey: meQueryKey, exact: true });
      } catch {
        // fetchQuery below still makes one explicit verification attempt.
      }
      if (!active || transitionID.current !== id) return;
      try {
        await queryClient.fetchQuery({ ...meQueryOptions(), staleTime: 0 });
      } catch {
        // A failed verification remains in the /me query for the auth gate.
      }
    })().finally(() => {
      if (active && transitionID.current === id) setTransition(null);
    });

    return () => {
      active = false;
    };
  }, [queryClient, transition]);

  const revalidateCurrentUser = useCallback(() => {
    void queryClient.fetchQuery({ ...meQueryOptions(), staleTime: 0 }).catch(() => undefined);
  }, [queryClient]);

  useEffect(() => {
    const onAuthenticationRequired = () => {
      beginTransition("logout", { broadcast: true, unauthenticated: true });
    };
    const onStorage = (event: StorageEvent) => {
      if (event.key !== authChangeStorageKey || !event.newValue) return;
      try {
        const message = JSON.parse(event.newValue) as { type?: unknown };
        if (message.type === "logout") beginTransition(message.type, { unauthenticated: true });
        if (message.type === "identity-changed") beginTransition(message.type);
      } catch {
        // Ignore unrelated or malformed local-storage values.
      }
    };
    const onLocalAuthChange = (event: Event) => {
      const type = (event as CustomEvent<unknown>).detail;
      if (type === "logout") beginTransition(type, { unauthenticated: true });
      if (type === "identity-changed") beginTransition(type);
    };

    window.addEventListener(authenticationRequiredEvent, onAuthenticationRequired);
    window.addEventListener("storage", onStorage);
    window.addEventListener("samik-bot:auth-change", onLocalAuthChange);
    window.addEventListener("focus", revalidateCurrentUser);
    window.addEventListener("pageshow", revalidateCurrentUser);
    window.addEventListener("online", revalidateCurrentUser);
    return () => {
      window.removeEventListener(authenticationRequiredEvent, onAuthenticationRequired);
      window.removeEventListener("storage", onStorage);
      window.removeEventListener("samik-bot:auth-change", onLocalAuthChange);
      window.removeEventListener("focus", revalidateCurrentUser);
      window.removeEventListener("pageshow", revalidateCurrentUser);
      window.removeEventListener("online", revalidateCurrentUser);
    };
  }, [beginTransition, revalidateCurrentUser]);

  const rootSessionExpired = isUnauthenticatedError(authError);
  useEffect(() => {
    if (rootSessionExpired) {
      beginTransition("logout", { broadcast: true, unauthenticated: true });
    }
  }, [beginTransition, rootSessionExpired]);

  const identityChanged =
    previousUserID.current != null && userID != null && previousUserID.current !== userID;
  useEffect(() => {
    if (userID == null) return;
    const previous = previousUserID.current;
    previousUserID.current = userID;
    if (previous != null && previous !== userID) {
      beginTransition("identity-changed", { broadcast: true });
    }
  }, [beginTransition, userID]);

  return { isTransitioning: transition != null || identityChanged, resetKey };
}

export function useUnauthorizedRedirect(error: unknown | readonly unknown[]) {
  const unauthenticated = (Array.isArray(error) ? error : [error]).some(isUnauthenticatedError);

  useEffect(() => {
    if (unauthenticated) notifyAuthenticationRequired();
  }, [unauthenticated]);

  return unauthenticated;
}
