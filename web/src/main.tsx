import React from "react";
import ReactDOM from "react-dom/client";
import { RouterProvider } from "@tanstack/react-router";
import { MutationCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { router } from "./router";
import { isRetryableError, isUnauthenticatedError, notifyAuthenticationRequired } from "./lib/api";
import "./styles.css";

const queryClient = new QueryClient({
	mutationCache: new MutationCache({
		onError: (error) => {
			if (isUnauthenticatedError(error)) notifyAuthenticationRequired();
		},
	}),
	defaultOptions: {
    queries: {
      staleTime: 5_000,
      retry: (failureCount, error) => isRetryableError(error) && failureCount < 1,
    },
  },
});

const rootEl = document.getElementById("root");
if (!rootEl) throw new Error("missing #root");
ReactDOM.createRoot(rootEl).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </React.StrictMode>,
);
