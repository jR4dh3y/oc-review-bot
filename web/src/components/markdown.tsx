import {
  Children,
  isValidElement,
  useEffect,
  useState,
} from "react";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";
import { cn } from "@/lib/utils";

// ---------------------------------------------------------------------------
// Mermaid (lazy-loaded)
// ---------------------------------------------------------------------------

type MermaidApi = (typeof import("mermaid"))["default"];

let mermaidPromise: Promise<MermaidApi> | null = null;
let mermaidRenderCount = 0;

function loadMermaid(): Promise<MermaidApi> {
  if (mermaidPromise == null) {
    mermaidPromise = import("mermaid")
      .then((module) => {
        module.default.initialize({ startOnLoad: false, theme: "neutral" });
        return module.default;
      })
      .catch((error: unknown) => {
        // Allow a later diagram to retry the import.
        mermaidPromise = null;
        throw error;
      });
  }
  return mermaidPromise;
}

type MermaidRenderState =
  | { status: "loading" }
  | { status: "done"; svg: string }
  | { status: "failed" };

function Mermaid({ code }: { code: string }) {
  const [state, setState] = useState<MermaidRenderState>({ status: "loading" });

  useEffect(() => {
    setState({ status: "loading" });
    let cancelled = false;
    const renderId = `mermaid-render-${++mermaidRenderCount}-${Math.random().toString(36).slice(2)}`;

    void (async () => {
      try {
        const mermaid = await loadMermaid();
        const { svg } = await mermaid.render(renderId, code);
        if (!cancelled) setState({ status: "done", svg });
      } catch {
        // Diagram source can be invalid; never crash the page. Mermaid may
        // leave its temporary measurement element behind on failure.
        try {
          document.getElementById(renderId)?.remove();
          document.getElementById(`d${renderId}`)?.remove();
        } catch {
          // Removal is best effort.
        }
        if (!cancelled) setState({ status: "failed" });
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [code]);

  return (
    <div className="my-4 overflow-hidden rounded-md border border-zinc-200 bg-white">
      {state.status === "loading" ? (
        <p className="p-4 text-xs text-zinc-500" role="status">
          Rendering diagram…
        </p>
      ) : state.status === "failed" ? (
        <div className="p-3">
          <p className="text-xs font-medium text-amber-800">
            This diagram couldn’t be rendered; showing its source instead.
          </p>
          <pre className="mt-2 overflow-x-auto rounded-md bg-zinc-950 p-3 text-xs text-zinc-100">
            {code}
          </pre>
        </div>
      ) : (
        <div
          role="img"
          aria-label="Diagram"
          className="overflow-x-auto p-4 [&_svg]:mx-auto [&_svg]:h-auto [&_svg]:max-w-full"
          dangerouslySetInnerHTML={{ __html: state.svg }}
        />
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Markdown
// ---------------------------------------------------------------------------

const inlineCodeClasses =
  "rounded bg-zinc-100 px-1 py-0.5 text-[0.875em] font-medium text-zinc-900 before:content-none after:content-none";

const markdownComponents: Components = {
  pre({ children }) {
    // In react-markdown v10 a `pre` override receives raw children, so look
    // for the fenced code's language class rather than the rendered element.
    const [first] = Children.toArray(children);
    if (
      isValidElement<{ className?: string }>(first) &&
      /language-mermaid/.test(first.props.className ?? "")
    ) {
      // A fenced mermaid block is rendered by the `code` override below; keep
      // it out of the dark <pre> wrapper.
      return <>{children}</>;
    }
    return (
      <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-md bg-zinc-950 p-4 text-xs text-zinc-100">
        {children}
      </pre>
    );
  },
  code({ className, children }) {
    const language = /language-([\w-]+)/.exec(className ?? "")?.[1];
    if (language === "mermaid") {
      return <Mermaid code={String(children).replace(/\n$/, "")} />;
    }
    // Fenced code carries a language class (or spans multiple lines); anything
    // else is inline code and gets the light chip styling.
    if (language != null || String(children).includes("\n")) {
      return (
        <code
          className={cn("font-normal text-zinc-100 before:content-none after:content-none", className)}
        >
          {children}
        </code>
      );
    }
    return <code className={inlineCodeClasses}>{children}</code>;
  },
};

interface MarkdownProps {
  children: string;
}

export function Markdown({ children }: MarkdownProps) {
  return (
    <div className="prose prose-sm prose-zinc max-w-none prose-headings:font-semibold prose-code:before:content-none prose-code:after:content-none">
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={markdownComponents}>
        {children}
      </ReactMarkdown>
    </div>
  );
}
