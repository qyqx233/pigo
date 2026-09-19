// The activity log: what a turn did, one line per step, in a scrolling area
// above the answer — the model's narration between tool calls, and each call
// with what it ran, whether it worked and how long it took. While the turn
// runs the log is open and follows the newest line (unless the reader has
// scrolled up); once it ends it folds to a one-line summary.
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { ActivityItem } from "./api";

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

function formatDuration(ms?: number): string {
  if (!ms || ms < 1000) return "";
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)}s`;
  return `${Math.floor(s / 60)}m${Math.round(s % 60)}s`;
}

// summarize counts the turn's tool calls by tool, for the folded line.
function summarize(items: ActivityItem[]): string {
  const tools = items.filter((item) => item.kind === "tool");
  const counts = new Map<string, number>();
  for (const item of tools) counts.set(item.tool ?? "工具", (counts.get(item.tool ?? "工具") ?? 0) + 1);
  const parts = [...counts.entries()].sort((a, b) => b[1] - a[1]).map(([name, n]) => `${name} ${n}`);
  const failed = tools.filter((item) => item.status === "error").length;
  return [`${tools.length} 步`, ...parts, ...(failed ? [`失败 ${failed}`] : [])].join(" · ");
}

function ActivityLine({ item }: { item: ActivityItem }) {
  if (item.kind === "text") {
    return (
      <div className="activity-line activity-text">
        <span className="activity-mark">•</span>
        <span className="activity-body">{item.text}</span>
      </div>
    );
  }
  const status = item.status ?? "running";
  const glyph = toolGlyph[item.tool ?? ""] ?? item.tool ?? "·";
  const label = item.tool === "todo" ? `待办 ${item.detail ?? ""}` : item.detail || item.tool;
  return (
    <div className={`activity-line activity-tool activity-${status}`}>
      <span className="activity-mark activity-status" aria-label={status}>
        {status === "running" ? <i className="activity-spinner" /> : status === "ok" ? "✓" : "✗"}
      </span>
      <span className="activity-body">
        <span className="activity-call">
          <b className="activity-glyph">{glyph}</b>
          <span className="activity-detail" title={item.detail}>{label}</span>
          <span className="activity-time">{formatDuration(item.elapsedMs)}</span>
        </span>
        {status === "error" && item.text && <span className="activity-error">{item.text}</span>}
        {status === "running" && item.output && <pre className="activity-output">{item.output}</pre>}
      </span>
    </div>
  );
}

export function ActivityLog({ items, running }: { items: ActivityItem[]; running: boolean }) {
  const [open, setOpen] = useState(running);
  const boxRef = useRef<HTMLDivElement>(null);
  // follow: keep the newest line in view, until the reader scrolls up.
  const follow = useRef(true);

  // Open while running; fold when the turn ends.
  useEffect(() => {
    setOpen(running);
    if (running) follow.current = true;
  }, [running]);

  useLayoutEffect(() => {
    const box = boxRef.current;
    if (box && open && follow.current) box.scrollTop = box.scrollHeight;
  }, [items, open]);

  if (items.length === 0) return null;
  return (
    <div className={running ? "activity activity-running" : "activity"}>
      <button type="button" className="activity-head" aria-expanded={open} onClick={() => setOpen(!open)}>
        <span className="activity-caret">{open ? "▾" : "▸"}</span>
        <span>{running ? "执行中" : "过程"}</span>
        <span className="activity-summary">{summarize(items)}</span>
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
          {items.map((item, index) => (
            <ActivityLine key={item.id ?? `t${index}`} item={item} />
          ))}
        </div>
      )}
    </div>
  );
}
