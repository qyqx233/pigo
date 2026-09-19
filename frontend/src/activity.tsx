// A turn's activity, shown inline and in order: the model's narration stays
// where it was written, as ordinary text, and each run of consecutive tool
// calls between two stretches of text is one small collapsible group. A group
// is open while one of its calls runs, following the newest line, and folds to
// a one-line summary when they are done — so nothing moves once it is on the
// page.
//
// The server reports the turn as a list of narration and tool items plus the
// text after the last call (api.ts foldActivity); turnContent turns that into
// assistant-ui message parts, and TurnParts renders them with
// MessagePrimitive.GroupedParts.
import { MessagePrimitive, useAuiState, type ThreadAssistantMessagePart } from "@assistant-ui/react";
import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import type { ActivityItem } from "./api";
import { MarkdownText } from "./markdown";

const toolGlyph: Record<string, string> = {
  bash: "$",
  read: "▤",
  write: "+",
  edit: "✎",
  grep: "⌕",
  find: "⌕",
  webfetch: "↗",
  websearch: "⌕",
  todo: "☐",
};

// ToolArgs and ToolResult are what a tool-call part carries: the call as the
// server described it, and how it ended. A part with no result is still
// running — or, once the turn is over, was cut off with it.
type ToolArgs = { detail?: string; output?: string };
type ToolResult = { summary?: string; elapsedMs?: number };

// turnContent lays a turn out as message parts, in the order it happened.
export function turnContent(items: ActivityItem[], text: string): ThreadAssistantMessagePart[] {
  const parts: ThreadAssistantMessagePart[] = [];
  items.forEach((item, index) => {
    if (item.kind === "text") {
      if (item.text?.trim()) parts.push({ type: "text", text: item.text });
      return;
    }
    const args: ToolArgs = { detail: item.detail, ...(item.output ? { output: item.output } : {}) };
    const done = item.status === "ok" || item.status === "error";
    parts.push({
      type: "tool-call",
      toolCallId: item.id ?? `call-${index}`,
      toolName: item.tool ?? "tool",
      args,
      argsText: item.detail ?? "",
      ...(done ? { result: { summary: item.text, elapsedMs: item.elapsedMs } satisfies ToolResult, isError: item.status === "error" } : {}),
    });
  });
  if (text || parts.length === 0) parts.push({ type: "text", text });
  return parts;
}

// cutOff marks the calls still running when a turn ended as failed: they were
// cut off with it, and will never report.
export function cutOff(items: ActivityItem[]): ActivityItem[] {
  return items.map((item) =>
    item.kind === "tool" && item.status === "running" ? { ...item, status: "error" as const, text: "未完成", output: undefined } : item,
  );
}

function formatDuration(ms?: number): string {
  if (!ms || ms < 1000) return "";
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)}s`;
  return `${Math.floor(s / 60)}m${Math.round(s % 60)}s`;
}

type ToolPart = { type: "tool-call"; toolName: string; result?: unknown; isError?: boolean };

// summarize counts a group's calls by tool, for its folded line.
function summarize(parts: ToolPart[]): string {
  const counts = new Map<string, number>();
  for (const part of parts) counts.set(part.toolName, (counts.get(part.toolName) ?? 0) + 1);
  const byTool = [...counts.entries()].sort((a, b) => b[1] - a[1]).map(([name, n]) => `${name} ${n}`);
  const failed = parts.filter((part) => part.isError).length;
  return [`${parts.length} 步`, ...byTool, ...(failed ? [`失败 ${failed}`] : [])].join(" · ");
}

// ToolGroup is one run of consecutive calls.
function ToolGroup({ indices, running, children }: { indices: readonly number[]; running: boolean; children: ReactNode }) {
  const allParts = useAuiState((state) => state.message.parts);
  const parts = indices.map((i) => allParts[i]).filter((part): part is ToolPart & (typeof allParts)[number] => part?.type === "tool-call");
  const elapsed = parts.reduce((sum, part) => sum + ((part.result as ToolResult | undefined)?.elapsedMs ?? 0), 0);
  const [open, setOpen] = useState(running);
  const boxRef = useRef<HTMLDivElement>(null);
  // follow: keep the newest line in view, until the reader scrolls up.
  const follow = useRef(true);

  // Open while a call runs; fold when they are done.
  useEffect(() => {
    setOpen(running);
    if (running) follow.current = true;
  }, [running]);

  useLayoutEffect(() => {
    const box = boxRef.current;
    if (box && open && follow.current) box.scrollTop = box.scrollHeight;
  });

  return (
    <div className={running ? "activity activity-running" : "activity"}>
      <button type="button" className="activity-head" aria-expanded={open} onClick={() => setOpen(!open)}>
        <span className="activity-caret">{open ? "▾" : "▸"}</span>
        <span>{running ? "执行中" : "已执行"}</span>
        <span className="activity-summary">{summarize(parts)}</span>
        {!running && <span className="activity-time">{formatDuration(elapsed)}</span>}
      </button>
      {open && (
        <div
          className="activity-box"
          ref={boxRef}
          onScroll={(event) => {
            const box = event.currentTarget;
            follow.current = box.scrollHeight - box.scrollTop - box.clientHeight < 24;
          }}
        >
          {children}
        </div>
      )}
    </div>
  );
}

// ToolLine is one call: what it ran, how it ended and how long it took; the
// tail of its output while it runs; the error when it failed.
function ToolLine({
  toolName,
  args,
  result,
  isError,
  running,
}: {
  toolName: string;
  args: ToolArgs;
  result?: ToolResult;
  isError?: boolean;
  running: boolean;
}) {
  const status = result ? (isError ? "error" : "ok") : running ? "running" : "cut";
  const glyph = toolGlyph[toolName] ?? toolName;
  const label = toolName === "todo" ? `待办 ${args.detail ?? ""}` : args.detail || toolName;
  return (
    <div className={`activity-line activity-tool activity-${status}`}>
      <span className="activity-mark activity-status" aria-label={status}>
        {status === "running" ? <i className="activity-spinner" /> : status === "ok" ? "✓" : status === "error" ? "✗" : "–"}
      </span>
      <span className="activity-body">
        <span className="activity-call">
          <b className="activity-glyph">{glyph}</b>
          <span className="activity-detail" title={args.detail}>{label}</span>
          <span className="activity-time">{status === "cut" ? "未完成" : formatDuration(result?.elapsedMs)}</span>
        </span>
        {status === "error" && result?.summary && <span className="activity-error">{result.summary}</span>}
        {status === "running" && args.output && <pre className="activity-output">{args.output}</pre>}
      </span>
    </div>
  );
}

// TurnParts renders an assistant message: its text as Markdown, its tool calls
// in groups.
export function TurnParts() {
  return (
    <MessagePrimitive.GroupedParts
      groupBy={(part) => (part.type === "tool-call" ? ["group-tools"] : null)}
      indicator="never"
    >
      {({ part, children }) => {
        switch (part.type) {
          case "group-tools":
            return (
              <ToolGroup indices={part.indices} running={part.status.type === "running"}>
                {children}
              </ToolGroup>
            );
          case "text":
            return <MarkdownText />;
          case "tool-call":
            return (
              <ToolLine
                toolName={part.toolName}
                args={(part.args ?? {}) as ToolArgs}
                result={part.result as ToolResult | undefined}
                isError={part.isError}
                running={part.status.type === "running"}
              />
            );
          default:
            return null;
        }
      }}
    </MessagePrimitive.GroupedParts>
  );
}
