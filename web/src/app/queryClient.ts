import { MutationCache, QueryCache, QueryClient, type QueryClientConfig } from "@tanstack/react-query";
import { isApiError } from "../api/client";
import { keys } from "../features/identity/api";

/** Removes everything a signed-in user could see, keeping only the gate's own state. */
export function resetUserData(qc: QueryClient) {
  qc.removeQueries({ predicate: (q) => q.queryKey[0] !== keys.me[0] && q.queryKey[0] !== keys.setup[0] });
}

/**
 * Query client whose caches react to an expired session anywhere in the app: the user is shown the sign-in page
 * and no data of the previous session survives.
 */
export function createQueryClient(config: QueryClientConfig = {}) {
  const onError = (e: unknown) => {
    if (isApiError(e, "unauthenticated")) {
      resetUserData(qc);
      qc.setQueryData(keys.me, null);
    }
  };
  const qc: QueryClient = new QueryClient({
    ...config,
    queryCache: new QueryCache({ onError }),
    mutationCache: new MutationCache({ onError }),
  });
  return qc;
}
