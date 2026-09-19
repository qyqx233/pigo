// Pieces every settings panel shares: the title block and the one-line status
// under the controls. Extracted so the panels cannot drift apart, and so a new
// panel is a list of controls rather than a copy of an old one.
import { useCallback, useState } from "react";

export function errorText(cause: unknown): string {
  return cause instanceof Error ? cause.message : String(cause);
}

// usePanelMessage is the result of the last action, or the reason it failed.
export function usePanelMessage() {
  const [message, setMessage] = useState("");
  const [failed, setFailed] = useState(false);
  const report = useCallback((text: string, isError: boolean) => {
    setFailed(isError);
    setMessage(text);
  }, []);
  const view = message ? (
    <div className={failed ? "settings-message settings-message-error" : "settings-message"}>{message}</div>
  ) : null;
  return { report, view };
}

export function PanelHeading({ title, hint, icon }: { title: string; hint: string; icon: string }) {
  return (
    <div className="section-heading">
      <div>
        <h3>{title}</h3>
        <p>{hint}</p>
      </div>
      <span className="section-icon">{icon}</span>
    </div>
  );
}
