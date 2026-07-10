// The query primitives: a JSON fetch wrapper, a QueryClient factory with the
// daemon-tuned defaults, the scoped-key helper, and the optimistic-mutation
// choreography. Domain hooks (useSession, useSubmit, …) are built on these by
// the consumer; none ship here.

import { QueryClient, useMutation, useQueryClient } from '@tanstack/react-query';
import type { QueryKey, UseMutationResult } from '@tanstack/react-query';

export function createQueryClient(overrides?: ConstructorParameters<typeof QueryClient>[0]) {
  return new QueryClient({
    defaultOptions: {
      queries: { staleTime: 5_000, retry: 1, refetchOnWindowFocus: false },
      ...overrides?.defaultOptions,
    },
    ...overrides,
  });
}

// A subject's cache key: a stable string prefix, the subject id, then an opaque
// scope (the displayed slice — e.g. a version). Consumers invalidate the whole
// family with `[prefix, subject]` and exact:false.
export const scopedKey = <Subject, Scope>(prefix: string, subject: Subject, scope: Scope) =>
  [prefix, subject, scope] as const;

export async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { 'content-type': 'application/json', ...init?.headers },
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`${init?.method ?? 'GET'} ${path} failed (${res.status}): ${text}`);
  }
  return res.json() as Promise<T>;
}

export interface OptimisticMutationConfig<Vars, Data, Cache> {
  mutationFn: (vars: Vars) => Promise<Data>;
  // The exact key whose cache the optimistic patch lands on.
  queryKey: (vars: Vars) => QueryKey;
  applyOptimistic: (cache: Cache, vars: Vars) => Cache;
  // On error, the key family to invalidate so the server (or stream echo)
  // re-establishes truth. Typically the broader `[prefix, subject]` with
  // exact:false.
  invalidate?: (vars: Vars) => { queryKey: QueryKey; exact?: boolean };
  // Fired on mutation error, once the invalidation is dispatched (its refetch is
  // not awaited), so a consumer can surface the failure promptly (e.g. an error
  // toast). The optimistic patch is deliberately left in place — the daemon
  // redelivers absolute state, so there is nothing to roll back.
  onError?: (err: Error, vars: Vars) => void;
  // Mutations sharing a scope run serially in dispatch order instead of
  // concurrently. Set it (e.g. to the subject id) when the daemon's append
  // order must match the user's action order — a blur-commit racing the
  // submit that triggered it, say.
  scope?: string;
}

// cancel → snapshot → optimistic patch → invalidate-on-error. The patch is not
// rolled back from a snapshot: the daemon redelivers absolute state over the
// stream, so an error invalidates the family and lets the refetch / echo
// converge.
export function useOptimisticMutation<Vars, Data, Cache>(
  config: OptimisticMutationConfig<Vars, Data, Cache>,
): UseMutationResult<Data, Error, Vars> {
  const qc = useQueryClient();
  return useMutation<Data, Error, Vars>({
    mutationFn: config.mutationFn,
    ...(config.scope !== undefined && { scope: { id: config.scope } }),
    onMutate: async (vars) => {
      const key = config.queryKey(vars);
      // An in-flight refetch (kicked off by another mutation's invalidate) would
      // resolve with a pre-mutation snapshot and clobber both this patch and the
      // stream echo.
      await qc.cancelQueries({ queryKey: key });
      const current = qc.getQueryData<Cache>(key);
      if (current === undefined) return;
      qc.setQueryData<Cache>(key, config.applyOptimistic(current, vars));
    },
    onError: (err, vars) => {
      const inv = config.invalidate?.(vars);
      if (inv) void qc.invalidateQueries(inv);
      config.onError?.(err, vars);
    },
  });
}
