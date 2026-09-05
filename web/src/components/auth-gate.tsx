import { type ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import { getErrorMessage } from "@/lib/api";
import { useCurrentUser, useUnauthorizedRedirect } from "@/lib/auth";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { LoadingState, RequestError } from "@/components/query-state";

interface RequireUserProps {
  children: ReactNode;
  admin?: boolean;
}

export function RequireUser({ children, admin = false }: RequireUserProps) {
	const currentUser = useCurrentUser();
	const unauthenticated = useUnauthorizedRedirect(currentUser.error);

  if (currentUser.isPending) {
    return <AccessCard title="Checking your session" description={<LoadingState>Checking your GitHub session…</LoadingState>} />;
  }

  if (unauthenticated) {
    return (
      <AccessCard
        title="Sign-in required"
        description="Your session has ended. Returning you to the home page…"
        action={
          <Button asChild>
            <a href="/auth/github/login">Sign in with GitHub</a>
          </Button>
        }
      />
    );
  }

  if (currentUser.isError) {
    return (
      <AccessCard
        title="We couldn’t verify your session"
        description={
          <RequestError
            title="Session check failed"
            description={getErrorMessage(currentUser.error, "Try checking your connection, then retry.")}
            onRetry={() => void currentUser.refetch()}
          />
        }
      />
    );
  }

  if (!currentUser.data) {
    return (
      <AccessCard
        title="Sign-in required"
        description="Sign in with GitHub to use this page."
        action={
          <Button asChild>
            <a href="/auth/github/login">Sign in with GitHub</a>
          </Button>
        }
      />
    );
  }

  if (admin && !currentUser.data.is_admin) {
    return (
      <AccessCard
        title="Admin access required"
        description="This page is available only to organization administrators."
        action={
          <Button variant="outline" asChild>
            <Link to="/dashboard">Go to dashboard</Link>
          </Button>
        }
      />
    );
  }

  return <>{children}</>;
}

interface AccessCardProps {
  title: string;
  description: ReactNode;
  action?: ReactNode;
}

function AccessCard({ title, description, action }: AccessCardProps) {
  return (
    <Card className="mx-auto max-w-lg">
      <CardHeader>
        <CardTitle as="h1">{title}</CardTitle>
        {typeof description === "string" ? <CardDescription>{description}</CardDescription> : description}
      </CardHeader>
      {action && <CardContent>{action}</CardContent>}
    </Card>
  );
}
