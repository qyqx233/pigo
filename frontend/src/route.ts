// Client-side routing primitives, shared by the shell and the settings panels.
//
// Real paths rather than a hash: the server serves index.html for every unknown
// non-API path (cmd/pigo-server/web/embed.go), so /settings/models resolves on a
// cold load and can be linked to.
export type Route = { name: "chat" } | { name: "settings"; tab: string };

// navigateEvent is dispatched by navigate() because pushState, unlike a back or
// forward gesture, fires nothing on its own.
export const navigateEvent = "pigo:navigate";

export function parsePath(pathname: string): Route {
  const parts = pathname.split("/").filter(Boolean);
  if (parts[0] === "settings") return { name: "settings", tab: parts[1] || "session" };
  return { name: "chat" };
}

export function navigate(path: string) {
  if (window.location.pathname === path) return;
  window.history.pushState(null, "", path);
  window.dispatchEvent(new Event(navigateEvent));
}
