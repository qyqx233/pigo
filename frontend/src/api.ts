export type UserInfo = {
  id: string;
  username: string;
  createdAt?: string;
  // admin is resolved per request from the server's PIGO_ADMIN_USERS roster.
  // The console is hidden without it, but every admin route is also enforced
  // server-side — hiding is convenience, not access control.
  admin?: boolean;
};

export type SessionInfo = {
  id: string;
  title?: string;
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

export type HistoryMessage = {
  id: string;
  role: "user" | "assistant" | "toolCall" | "toolResult";
  content?: string;
  createdAt: string;
  toolName?: string;
  toolCallId?: string;
  arguments?: unknown;
  isError?: boolean;
  // The turn's model usage, on the turn's last assistant message.
  usage?: TurnUsage;
};

// --- billing ------------------------------------------------------------------

// CallUsage is one model call: the "usage" stream event, and one detail row of a
// turn. Token counts never overlap: input excludes the cache. Cost is in yuan.
export type CallUsage = {
  kind: "chat" | "compaction";
  status: "ok" | "error" | "aborted" | "unreported";
  provider: string;
  model: string;
  responseId?: string;
  input: number;
  cacheRead: number;
  cacheWrite: number;
  output: number;
  reasoning?: number;
  priced: boolean;
  cost: number;
  billedTo: "platform" | "self";
};

export type UsageTotals = {
  calls: number;
  input: number;
  cacheRead: number;
  cacheWrite: number;
  output: number;
  reasoning: number;
  cost: number;
  platformCost: number;
  selfCost: number;
  // unpriced: calls of models without a price (counted at zero).
  // incomplete: calls whose usage is unknown (aborted or never reported).
  unpriced: number;
  incomplete: number;
};

export type TurnUsage = UsageTotals & { details: CallUsage[] };

export type UsageGroup = UsageTotals & { key: string; label?: string; deleted?: boolean };

export type UsageEntry = {
  id: string;
  at: string;
  userId: string;
  username: string;
  sessionId: string;
  turnId: string;
  provider: string;
  model: string;
  responseModel?: string;
  kind: string;
  status: string;
  billedTo: string;
  keySource: string;
  input: number;
  cacheRead: number;
  cacheWrite: number;
  output: number;
  priced: boolean;
  costYuan: number;
  usageAnomaly?: boolean;
};

export type UsageReport = {
  from: string;
  to: string;
  currency: string;
  totals: UsageTotals;
  byDay: UsageGroup[];
  byModel: UsageGroup[];
  byUser?: UsageGroup[];
  unpriced: { provider: string; model: string; calls: number }[];
  recent: UsageEntry[];
};

export type UsageQuery = { from?: string; to?: string; user?: string; provider?: string; model?: string; session?: string };

// ModelPrice is one row of the price table, in yuan per million tokens. Unset
// cache prices are charged at the input price.
export type ModelPrice = {
  provider: string;
  model: string;
  input: number;
  cacheRead?: number;
  cacheWrite?: number;
  output: number;
  updatedBy?: string;
  updatedAt?: string;
};

export type PriceList = { currency: string; unit: string; prices: ModelPrice[] };

export type WorkspaceEntry = { name: string; dir: boolean; size: number };

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
  // "custom" (added on this server), "free" (OpenRouter's free catalog) or
  // "preset" (a built-in model of a provider that has a key).
  source?: "custom" | "free" | "preset";
};

export type CustomModelInfo = {
  id: string;
  label: string;
  provider: string;
  expiresAt: string;
  expired: boolean;
  createdAt: string;
  // "user" (personal, expires) or "public" (the deployment catalog).
  scope?: string;
};

export type ProviderInfo = {
  name: string;
  hasKey: boolean;
  keyHint: string;
  // Which tier supplies the key a run would actually use.
  source?: "user" | "public" | "env" | "none";
  // Set for an administrator-defined endpoint.
  custom?: boolean;
  protocol?: string;
  baseUrl?: string;
};

export type CustomProvider = { name: string; protocol: string; baseUrl: string };

// CredentialInfo never carries the key itself — only a masked hint.
export type CredentialInfo = { provider: string; configured: boolean; hint?: string };

export type CredentialList = {
  enabled: boolean;
  reason?: string;
  credentials: CredentialInfo[];
};

export type ServerSettings = {
  defaultModel: string;
  defaultThinking: string;
  allowRegistration: boolean;
  allowUserKeys: boolean;
};

export type AdminUser = {
  id: string;
  username: string;
  createdAt?: string;
  admin: boolean;
  disabled: boolean;
  sessions: number;
  credentials?: string[];
};

export type DeleteUserResult = {
  username: string;
  sessions: number;
  problems?: string[];
};

export type SessionSettings = {
  model: string;
  provider?: string;
  thinking: string;
};

type StreamEvent = {
  // "notice" carries transient run status (currently only a retry after a
  // rate-limited or overloaded request): no conversation content, safe to
  // ignore, and worth surfacing because the run pauses for several seconds.
  // "usage" reports one model call's tokens and cost as the call finishes; a
  // turn with tool use or compaction sends several.
  type: "delta" | "done" | "error" | "tool" | "notice" | "usage";
  usage?: CallUsage;
  text?: string;
  error?: string;
  tool?: string;
  notice?: string;
  phase?: string;
};

const serverTokenStorageKey = "pigo.serverToken";

export class APIError extends Error {
  constructor(message: string, readonly status: number) {
    super(message);
  }
}

export class PigoAPI {
  private session: SessionInfo | null = null;
  private token = "";

  constructor() {
    try {
      // Retained for compatibility with older deployments. Normal browser
      // requests authenticate with the HttpOnly account cookie.
      this.token = window.localStorage.getItem(serverTokenStorageKey)?.trim() ?? "";
    } catch {
      // Storage can be unavailable in hardened/private browser contexts.
    }
  }

  selectSession(session: SessionInfo | null) {
    this.session = session;
  }

  async register(username: string, password: string, registrationToken = ""): Promise<UserInfo> {
    const headers = new Headers(this.headers(true));
    if (registrationToken.trim()) headers.set("Authorization", `Bearer ${registrationToken.trim()}`);
    const user = await this.request<UserInfo>("/api/auth/register", {
      method: "POST",
      headers,
      body: JSON.stringify({ username, password }),
    });
    this.clearLegacyToken();
    return user;
  }

  async login(username: string, password: string): Promise<UserInfo> {
    const user = await this.request<UserInfo>("/api/auth/login", {
      method: "POST",
      headers: this.headers(true),
      body: JSON.stringify({ username, password }),
    });
    this.clearLegacyToken();
    return user;
  }

  async logout(): Promise<void> {
    await this.request<void>("/api/auth/logout", { method: "POST", headers: this.headers() });
    this.session = null;
  }

  async me(): Promise<UserInfo> {
    return this.request<UserInfo>("/api/auth/me", { headers: this.headers() });
  }

  async sessions(): Promise<SessionInfo[]> {
    return this.request<SessionInfo[]>("/api/sessions", { headers: this.headers() });
  }

  async createSession(): Promise<SessionInfo> {
    const session = await this.request<SessionInfo>("/api/sessions", {
      method: "POST",
      headers: this.headers(true),
      body: "{}",
    });
    this.session = session;
    return session;
  }

  async getSession(id: string): Promise<SessionInfo> {
    const session = await this.request<SessionInfo>(`/api/sessions/${encodeURIComponent(id)}`, {
      headers: this.headers(),
    });
    this.session = session;
    return session;
  }

  async deleteSession(id: string): Promise<void> {
    await this.request<void>(`/api/sessions/${encodeURIComponent(id)}`, {
      method: "DELETE",
      headers: this.headers(),
    });
    if (this.session?.id === id) this.session = null;
  }

  async history(id: string): Promise<HistoryMessage[]> {
    return this.request<HistoryMessage[]>(`/api/sessions/${encodeURIComponent(id)}/messages`, {
      headers: this.headers(),
    });
  }

  async commands(): Promise<SlashCommandInfo[]> {
    const session = this.requireSession();
    return this.request<SlashCommandInfo[]>(
      `/api/sessions/${encodeURIComponent(session.id)}/commands`,
      { headers: this.headers() },
    );
  }

  async sessionInfo(): Promise<SessionInfo> {
    return this.getSession(this.requireSession().id);
  }

  async models(): Promise<ModelInfo[]> {
    return this.request<ModelInfo[]>("/api/models", { headers: this.headers() });
  }

  async customModels(): Promise<CustomModelInfo[]> {
    return this.request<CustomModelInfo[]>("/api/custom-models", { headers: this.headers() });
  }

  async addCustomModel(body: {
    id: string;
    label?: string;
    provider?: string;
    expiresAt: string;
  }): Promise<CustomModelInfo> {
    return this.request<CustomModelInfo>("/api/custom-models", {
      method: "POST",
      headers: this.headers(true),
      body: JSON.stringify(body),
    });
  }

  async deleteCustomModel(id: string): Promise<void> {
    await this.request<void>(`/api/custom-models/${encodeURIComponent(id)}`, {
      method: "DELETE",
      headers: this.headers(),
    });
  }

  async providers(): Promise<ProviderInfo[]> {
    return this.request<ProviderInfo[]>("/api/providers", { headers: this.headers() });
  }

  // --- a user's own provider keys -------------------------------------------

  async credentials(): Promise<CredentialList> {
    return this.request<CredentialList>("/api/credentials", { headers: this.headers() });
  }

  async setCredential(provider: string, key: string): Promise<CredentialList> {
    return this.request<CredentialList>(`/api/credentials/${encodeURIComponent(provider)}`, {
      method: "PUT",
      headers: this.headers(true),
      body: JSON.stringify({ key }),
    });
  }

  async deleteCredential(provider: string): Promise<CredentialList> {
    return this.request<CredentialList>(`/api/credentials/${encodeURIComponent(provider)}`, {
      method: "DELETE",
      headers: this.headers(),
    });
  }

  // --- the admin console ----------------------------------------------------

  async adminSettings(): Promise<ServerSettings> {
    return this.request<ServerSettings>("/api/admin/settings", { headers: this.headers() });
  }

  async updateAdminSettings(settings: ServerSettings): Promise<ServerSettings> {
    return this.request<ServerSettings>("/api/admin/settings", {
      method: "PUT",
      headers: this.headers(true),
      body: JSON.stringify(settings),
    });
  }

  async adminCredentials(): Promise<CredentialList> {
    return this.request<CredentialList>("/api/admin/credentials", { headers: this.headers() });
  }

  async setAdminCredential(provider: string, key: string): Promise<CredentialList> {
    return this.request<CredentialList>(`/api/admin/credentials/${encodeURIComponent(provider)}`, {
      method: "PUT",
      headers: this.headers(true),
      body: JSON.stringify({ key }),
    });
  }

  async deleteAdminCredential(provider: string): Promise<CredentialList> {
    return this.request<CredentialList>(`/api/admin/credentials/${encodeURIComponent(provider)}`, {
      method: "DELETE",
      headers: this.headers(),
    });
  }

  async adminModels(): Promise<CustomModelInfo[]> {
    return this.request<CustomModelInfo[]>("/api/admin/models", { headers: this.headers() });
  }

  async addAdminModel(body: { id: string; label?: string; provider?: string }): Promise<CustomModelInfo> {
    return this.request<CustomModelInfo>("/api/admin/models", {
      method: "POST",
      headers: this.headers(true),
      body: JSON.stringify(body),
    });
  }

  async deleteAdminModel(id: string): Promise<void> {
    await this.request<void>(`/api/admin/models/${encodeURIComponent(id)}`, {
      method: "DELETE",
      headers: this.headers(),
    });
  }

  async adminProviders(): Promise<CustomProvider[]> {
    return this.request<CustomProvider[]>("/api/admin/providers", { headers: this.headers() });
  }

  async putAdminProvider(body: CustomProvider): Promise<CustomProvider[]> {
    return this.request<CustomProvider[]>("/api/admin/providers", {
      method: "PUT",
      headers: this.headers(true),
      body: JSON.stringify(body),
    });
  }

  async deleteAdminProvider(name: string): Promise<CustomProvider[]> {
    return this.request<CustomProvider[]>(`/api/admin/providers/${encodeURIComponent(name)}`, {
      method: "DELETE",
      headers: this.headers(),
    });
  }

  async adminUsers(): Promise<AdminUser[]> {
    return this.request<AdminUser[]>("/api/admin/users", { headers: this.headers() });
  }

  async setUserDisabled(id: string, disabled: boolean): Promise<AdminUser> {
    return this.request<AdminUser>(`/api/admin/users/${encodeURIComponent(id)}/disable`, {
      method: "POST",
      headers: this.headers(true),
      body: JSON.stringify({ disabled }),
    });
  }

  async resetUserPassword(id: string): Promise<{ username: string; password: string }> {
    return this.request<{ username: string; password: string }>(
      `/api/admin/users/${encodeURIComponent(id)}/password`,
      { method: "POST", headers: this.headers() },
    );
  }

  async deleteUser(id: string): Promise<DeleteUserResult> {
    return this.request<DeleteUserResult>(`/api/admin/users/${encodeURIComponent(id)}`, {
      method: "DELETE",
      headers: this.headers(),
    });
  }

  // --- billing ----------------------------------------------------------------

  async prices(): Promise<PriceList> {
    return this.request<PriceList>("/api/prices", { headers: this.headers() });
  }

  async putPrice(price: ModelPrice): Promise<PriceList> {
    return this.request<PriceList>("/api/admin/prices", {
      method: "PUT",
      headers: this.headers(true),
      body: JSON.stringify(price),
    });
  }

  // The row is named in the query string: model ids contain slashes.
  async deletePrice(provider: string, model: string): Promise<PriceList> {
    return this.request<PriceList>(`/api/admin/prices?${new URLSearchParams({ provider, model })}`, {
      method: "DELETE",
      headers: this.headers(),
    });
  }

  async usage(query: UsageQuery, admin = false): Promise<UsageReport> {
    return this.request<UsageReport>(`${admin ? "/api/admin/usage" : "/api/usage"}?${usageParams(query)}`, {
      headers: this.headers(),
    });
  }

  async sessionUsage(id: string): Promise<UsageTotals> {
    return this.request<UsageTotals>(`/api/sessions/${encodeURIComponent(id)}/usage`, { headers: this.headers() });
  }

  async downloadUsage(query: UsageQuery, admin = false): Promise<void> {
    const response = await fetch(
      `${admin ? "/api/admin/usage" : "/api/usage"}?${usageParams({ ...query, format: "csv" })}`,
      { headers: this.headers(), credentials: "same-origin" },
    );
    if (!response.ok) throw new APIError(await errorMessage(response), response.status);
    const disposition = response.headers.get("Content-Disposition") ?? "";
    const name = /filename="([^"]+)"/.exec(disposition)?.[1] ?? "usage.csv";
    saveBlob(await response.blob(), name);
  }

  async updateSession(settings: SessionSettings): Promise<SessionInfo> {
    const session = this.requireSession();
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

  async files(path = "."): Promise<WorkspaceEntry[]> {
    const session = this.requireSession();
    const payload = await this.request<{ entries?: WorkspaceEntry[] }>(
      `/api/sessions/${encodeURIComponent(session.id)}/files?path=${encodeURIComponent(path)}`,
      { headers: this.headers() },
    );
    return payload.entries ?? [];
  }

  async download(path: string): Promise<void> {
    const session = this.requireSession();
    const response = await fetch(
      `/api/sessions/${encodeURIComponent(session.id)}/files/raw?path=${encodeURIComponent(path)}`,
      { headers: this.headers(), credentials: "same-origin" },
    );
    if (!response.ok) throw new APIError(await errorMessage(response), response.status);
    saveBlob(await response.blob(), path.split("/").pop() || "file");
  }

  // onNotice receives transient run status (a retry after a rate-limited or
  // overloaded request). It is a callback rather than a yielded value because a
  // notice is not part of the reply: the run pauses for seconds, and the UI
  // needs to say so without writing anything into the assistant's message.
  //
  // onUsage receives each model call's usage as it finishes, for the same
  // reason: it describes the reply rather than being part of it.
  async *stream(
    prompt: string,
    signal: AbortSignal,
    onNotice?: (text: string) => void,
    onUsage?: (usage: CallUsage) => void,
  ): AsyncGenerator<string> {
    const session = this.requireSession();
    const response = await fetch(
      `/api/sessions/${encodeURIComponent(session.id)}/messages?stream=true`,
      {
        method: "POST",
        headers: this.headers(true),
        body: JSON.stringify({ prompt }),
        signal,
        credentials: "same-origin",
      },
    );
    if (!response.ok) throw new APIError(await errorMessage(response), response.status);
    if (!response.body) throw new Error("当前浏览器不支持流式响应");

    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let pending = "";
    let accumulated = "";
    for (;;) {
      const chunk = await reader.read();
      pending += decoder.decode(chunk.value ?? new Uint8Array(), { stream: !chunk.done });
      const lines = pending.split("\n");
      pending = lines.pop() ?? "";
      for (const line of lines) {
        if (!line.trim()) continue;
        const event = JSON.parse(line) as StreamEvent;
        if (event.type === "error") throw new Error(event.error || "请求失败");
        if (event.type === "notice") onNotice?.(event.text ?? "");
        if (event.type === "usage" && event.usage) onUsage?.(event.usage);
        if (event.type === "delta") {
          // Output is flowing again, so any pending notice is over. Sent on
          // every delta (not just the first) because one run streams several
          // turns and a retry can follow text that already arrived; React bails
          // out when the value is unchanged.
          onNotice?.("");
          accumulated += event.text ?? "";
          yield accumulated;
        }
        if (event.type === "done") {
          onNotice?.("");
          accumulated = event.text ?? accumulated;
          yield accumulated;
        }
      }
      if (chunk.done) break;
    }
  }

  private requireSession(): SessionInfo {
    if (!this.session) throw new Error("尚未选择会话");
    return this.session;
  }

  private clearLegacyToken() {
    this.token = "";
    try {
      window.localStorage.removeItem(serverTokenStorageKey);
    } catch {
      // The account cookie is already active even if storage is unavailable.
    }
  }

  private headers(json = false): HeadersInit {
    const headers: Record<string, string> = {};
    if (json) headers["Content-Type"] = "application/json";
    if (this.token) headers.Authorization = `Bearer ${this.token}`;
    return headers;
  }

  private async request<T>(url: string, init: RequestInit = {}): Promise<T> {
    const response = await fetch(url, { ...init, credentials: "same-origin" });
    if (!response.ok) throw new APIError(await errorMessage(response), response.status);
    if (response.status === 204) return undefined as T;
    return response.json() as Promise<T>;
  }
}

function saveBlob(blob: Blob, name: string) {
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  URL.revokeObjectURL(url);
}

function usageParams(query: Record<string, string | undefined>): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value) params.set(key, value);
  }
  return params.toString();
}

async function errorMessage(response: Response): Promise<string> {
  const body = (await response.json().catch(() => null)) as { error?: string } | null;
  return body?.error || `${response.status} ${response.statusText}`;
}
