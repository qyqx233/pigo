export type SessionInfo = {
  id: string;
  model: string;
  provider: string;
  thinking: string;
  tools: string[];
  createdAt: string;
  lastUsed: string;
  pigoSessionId?: string;
  sandbox?: string;
  alive?: boolean;
};

export type WorkspaceEntry = {
  name: string;
  dir: boolean;
  size: number;
};

export type SlashCommandInfo = {
  name: string;
  description: string;
  argumentHint?: string;
  source: string;
  available: boolean;
};

export type ModelInfo = {
  id: string;
  label: string;
  provider: string;
};

export type SessionSettings = {
  model: string;
  thinking: string;
};

type StreamEvent = {
  type: "delta" | "done" | "error" | "tool";
  text?: string;
  error?: string;
  tool?: string;
  phase?: string;
};

const serverTokenStorageKey = "pigo.serverToken";

export class PigoAPI {
  private session: SessionInfo | null = null;
  private sessionPromise: Promise<SessionInfo> | null = null;
  private token = "";

  constructor() {
    try {
      this.token = window.localStorage.getItem(serverTokenStorageKey)?.trim() ?? "";
    } catch {
      // Storage can be unavailable in hardened/private browser contexts.
    }
  }

  setToken(token: string) {
    this.token = token.trim();
    try {
      if (this.token) {
        window.localStorage.setItem(serverTokenStorageKey, this.token);
      } else {
        window.localStorage.removeItem(serverTokenStorageKey);
      }
    } catch {
      // Keep the token for this page lifetime when persistence is unavailable.
    }
  }

  private headers(json = false): HeadersInit {
    const headers: Record<string, string> = {};
    if (json) headers["Content-Type"] = "application/json";
    if (this.token) headers.Authorization = `Bearer ${this.token}`;
    return headers;
  }

  async ensureSession(): Promise<SessionInfo> {
    if (this.session) return this.session;
    if (!this.sessionPromise) {
      this.sessionPromise = this.request<SessionInfo>("/api/sessions", {
        method: "POST",
        headers: this.headers(true),
        body: "{}",
      })
        .then((session) => {
          this.session = session;
          return session;
        })
        .finally(() => {
          this.sessionPromise = null;
        });
    }
    return this.sessionPromise;
  }

  async commands(): Promise<SlashCommandInfo[]> {
    const session = await this.ensureSession();
    return this.request<SlashCommandInfo[]>(
      `/api/sessions/${encodeURIComponent(session.id)}/commands`,
      { headers: this.headers() },
    );
  }

  async sessionInfo(): Promise<SessionInfo> {
    const session = await this.ensureSession();
    const current = await this.request<SessionInfo>(
      `/api/sessions/${encodeURIComponent(session.id)}`,
      { headers: this.headers() },
    );
    this.session = current;
    return current;
  }

  async models(): Promise<ModelInfo[]> {
    return this.request<ModelInfo[]>("/api/models", {
      headers: this.headers(),
    });
  }

  async updateSession(settings: SessionSettings): Promise<SessionInfo> {
    const session = await this.ensureSession();
    const current = await this.request<SessionInfo>(
      `/api/sessions/${encodeURIComponent(session.id)}`,
      {
        method: "PATCH",
        headers: this.headers(true),
        body: JSON.stringify(settings),
      },
    );
    this.session = current;
    return current;
  }

  async reset(): Promise<void> {
    const session = this.session;
    this.session = null;
    this.sessionPromise = null;
    if (!session) return;
    await this.request<void>(`/api/sessions/${encodeURIComponent(session.id)}`, {
      method: "DELETE",
      headers: this.headers(),
    });
  }

  async files(path = "."): Promise<WorkspaceEntry[]> {
    const session = await this.ensureSession();
    const payload = await this.request<{ entries?: WorkspaceEntry[] }>(
      `/api/sessions/${encodeURIComponent(session.id)}/files?path=${encodeURIComponent(path)}`,
      { headers: this.headers() },
    );
    return payload.entries ?? [];
  }

  async download(path: string): Promise<void> {
    const session = await this.ensureSession();
    const response = await fetch(
      `/api/sessions/${encodeURIComponent(session.id)}/files/raw?path=${encodeURIComponent(path)}`,
      { headers: this.headers() },
    );
    if (!response.ok) throw new Error(await errorMessage(response));
    const blob = await response.blob();
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = path.split("/").pop() || "file";
    link.click();
    URL.revokeObjectURL(url);
  }

  async *stream(prompt: string, signal: AbortSignal): AsyncGenerator<string> {
    const session = await this.ensureSession();
    const response = await fetch(
      `/api/sessions/${encodeURIComponent(session.id)}/messages?stream=true`,
      {
        method: "POST",
        headers: this.headers(true),
        body: JSON.stringify({ prompt }),
        signal,
      },
    );
    if (!response.ok) throw new Error(await errorMessage(response));
    if (!response.body) throw new Error("当前浏览器不支持流式响应");

    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let pending = "";
    let accumulated = "";
    for (;;) {
      const chunk = await reader.read();
      pending += decoder.decode(chunk.value ?? new Uint8Array(), {
        stream: !chunk.done,
      });
      const lines = pending.split("\n");
      pending = lines.pop() ?? "";
      for (const line of lines) {
        if (!line.trim()) continue;
        const event = JSON.parse(line) as StreamEvent;
        if (event.type === "error") throw new Error(event.error || "请求失败");
        if (event.type === "delta") {
          accumulated += event.text ?? "";
          yield accumulated;
        }
        if (event.type === "done") {
          accumulated = event.text ?? accumulated;
          yield accumulated;
        }
      }
      if (chunk.done) break;
    }
  }

  private async request<T>(url: string, init?: RequestInit): Promise<T> {
    const response = await fetch(url, init);
    if (!response.ok) throw new Error(await errorMessage(response));
    if (response.status === 204) return undefined as T;
    return response.json() as Promise<T>;
  }
}

async function errorMessage(response: Response): Promise<string> {
  const body = (await response.json().catch(() => null)) as
    | { error?: string }
    | null;
  return body?.error || `${response.status} ${response.statusText}`;
}
