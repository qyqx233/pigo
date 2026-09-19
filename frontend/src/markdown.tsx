// Markdown rendering for assistant replies.
//
// The model answers in Markdown, and until now the web UI showed it verbatim:
// assistant-ui's default text part is a <p style="white-space:pre-line"> around
// the raw string, so "**bold**", headings, tables and fenced code all arrived as
// literal characters. The two terminal front-ends have rendered Markdown through
// glamour all along (internal/cli/ui/markdown.go, internal/cli/tui/markdown.go);
// this brings the web to parity.
//
// Unlike the terminal — which can only lay out a block once the whole turn is
// known, and so renders once at turn end — the browser re-renders on every
// streamed token, so the Markdown appears as it is written.
import { MarkdownTextPrimitive, type CodeHeaderProps } from "@assistant-ui/react-markdown";
import { MessagePartPrimitive } from "@assistant-ui/react";
import { useState } from "react";
import remarkGfm from "remark-gfm";
import { remarkLiteralBreaks } from "./markdown-breaks";

// CodeHeader labels a fenced block with its language and offers a one-click
// copy — the assistant's code blocks are the part users act on most.
function CodeHeader({ language, code }: CodeHeaderProps) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    if (!code) return;
    try {
      await navigator.clipboard.writeText(code);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // Clipboard access can be denied (insecure origin, permission policy);
      // the code is still selectable, so there is nothing to recover from.
    }
  }

  return (
    <div className="code-header">
      <span className="code-language">{language || "text"}</span>
      <button type="button" className="code-copy" onClick={copy}>
        {copied ? "已复制" : "复制"}
      </button>
    </div>
  );
}

// MarkdownText replaces the default text part for assistant messages.
// remark-gfm is what turns tables, task lists and strikethrough — all of which
// models emit constantly — into real elements rather than literal pipes, and
// remarkLiteralBreaks makes the <br> models write inside table cells behave as
// the line break they mean.
export function MarkdownText() {
  return (
    <>
      <MarkdownTextPrimitive
        className="markdown"
        remarkPlugins={[remarkGfm, remarkLiteralBreaks]}
        components={{ CodeHeader }}
      />
      {/* The default text part appends a cursor while the turn streams; keep it. */}
      <MessagePartPrimitive.InProgress>
        <span className="streaming-cursor"> ●</span>
      </MessagePartPrimitive.InProgress>
    </>
  );
}
