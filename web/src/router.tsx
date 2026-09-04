import { createRouter } from "@tanstack/react-router";
import { Route as RootRoute } from "./routes/__root";
import { Route as LandingRoute } from "./routes/index";
import { Route as DashboardRoute, ReviewDetailRoute } from "./routes/dashboard";
import { Route as AdminKeysRoute } from "./routes/admin.keys";
import { Route as AdminSettingsRoute } from "./routes/admin.settings";

const routeTree = RootRoute.addChildren([
  LandingRoute,
  DashboardRoute,
  ReviewDetailRoute,
  AdminKeysRoute,
  AdminSettingsRoute,
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
