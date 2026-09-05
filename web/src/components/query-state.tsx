import type { ReactNode } from "react";
import { AlertCircle, LoaderCircle, RefreshCw } from "lucide-react";
import { Button } from "@/components/ui/button";

interface LoadingStateProps {
  children: ReactNode;
}

export function LoadingState({ children }: LoadingStateProps) {
  return (
    <div className="flex items-center gap-2 text-sm text-zinc-500" role="status" aria-live="polite">
      <LoaderCircle className="size-4 animate-spin" aria-hidden="true" />
      <span>{children}</span>
    </div>
  );
}

interface RequestErrorProps {
  title: string;
  description: string;
  onRetry?: () => void;
}

export function RequestError({ title, description, onRetry }: RequestErrorProps) {
  return (
    <div className="flex flex-col gap-3 rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-800 sm:flex-row sm:items-center" role="alert">
      <AlertCircle className="size-4 shrink-0" aria-hidden="true" />
      <div className="min-w-0 flex-1">
        <p className="font-medium">{title}</p>
        <p className="break-words text-red-700">{description}</p>
      </div>
      {onRetry && (
        <Button type="button" variant="outline" size="sm" onClick={onRetry} className="border-red-200 bg-white text-red-800 hover:bg-red-100">
          <RefreshCw className="size-3.5" aria-hidden="true" />
          Try again
        </Button>
      )}
    </div>
  );
}
