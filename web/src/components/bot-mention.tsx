import { queryOptions, useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";

export const metaQueryKey = ["meta"] as const;

export const metaQueryOptions = () =>
  queryOptions({
    queryKey: metaQueryKey,
    queryFn: api.meta,
    retry: false,
    staleTime: Infinity,
  });
/** Mention handle for the configured bot login. Falls back to plain words
 *  while the public meta endpoint is unreachable, never to a stale login. */
export function BotMention() {
  const meta = useQuery(metaQueryOptions());
  const username = meta.data?.bot_username?.trim();
  if (!username) {
    return <>the review bot</>;
  }
  return <code className="rounded bg-zinc-100 px-1">@{username}</code>;
}
