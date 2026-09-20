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
  // toolProfile is the naming profile the session's model gets, and toolNames
  // what it renames (canonical → model-facing).
  toolProfile?: string;
  toolNames?: Record<string, string>;
  // context: the window and compaction threshold its current model gets.
  context?: ContextInfo;
  // turn is the running (or just-finished) turn; lastTurn how the last one
  // ended, kept across restarts.
  turn?: TurnInfo;
  lastTurn?: TurnInfo;
  // scene: the scene the session was created from.
  scene?: SessionScene;
  // draft: nothing sent in it yet — the server has not stored it, and it is
  // not in the session list.
  draft?: boolean;
};

export type SessionScene = { slug: string; name: string; icon?: string };

// SceneInfo is a scene: a prepared prompt for one kind of task, with the
// tools and default model it runs on (see spec/preset-scenes.md).
export type SceneInfo = {
  slug: string;
  name: string;
  icon?: string;
  description?: string;
  prompt: string;
  examples?: string[];
  tools?: string[];
  model?: string;
  provider?: string;
  thinking?: string;
  disabled?: boolean;
  order?: number;
  updatedBy?: string;
  updatedAt?: string;
  // missingTools: listed tools the tools config no longer has.
  missingTools?: string[];
};

// SceneToolInfo is a tool a scene can list; scene: only scenes get it.
export type SceneToolInfo = { name: string; description?: string; builtin?: boolean; scene?: boolean };

export type SceneList = { scenes: SceneInfo[]; tools: SceneToolInfo[] };

// TurnInfo describes one turn: a user message and everything the agent did
// about it. A turn runs on the server independently of the page.
export type TurnInfo = {
  id: string;
  startedAt: string;
  endedAt?: string;
  status: "running" | "done" | "stopped" | "error";
  // Why it ended: "done", "step_limit", "stalled", "time_limit", "canceled",
  // "shutdown", "interrupted", "session_closed" or "error".
  reason?: string;
  steps: number;
  message?: string;
};

// TurnStatus is what a running turn is doing, from its heartbeat and tool
// events. since is a local timestamp (ms) the phase began.
export type TurnStatus = { phase: "tool" | "model"; tool?: string; since: number; steps: number };

// TurnEnd is how a turn the page watched ended.
export type TurnEnd = { reason: string; steps: number; message?: string };

// The reasons after which "继续" makes sense: the work was cut short, not done.
export const continuableReasons = new Set(["step_limit", "stalled", "time_limit", "canceled", "shutdown", "interrupted"]);

// TurnError is a turn that ended without finishing; its message is the
// server's explanation.
export class TurnError extends Error {
  constructor(message: string, readonly end: TurnEnd) {
    super(message);
  }
}

// TurnHandlers receive what a turn stream carries besides the reply text.
export type TurnHandlers = {
  // onNotice: transient status to show (a retry, a reconnect); "" clears it.
  onNotice?: (text: string) => void;
  // onUsage: every model call of the turn so far (the full list each time).
  onUsage?: (calls: CallUsage[]) => void;
  onStatus?: (status: TurnStatus | null) => void;
  onEnd?: (end: TurnEnd) => void;
  // onActivity: the turn's activity log so far (the full list each time).
  onActivity?: (items: ActivityItem[]) => void;
};

export type HistoryMessage = {
  id: string;
  role: "user" | "assistant" | "toolCall" | "toolResult" | "compaction";
  content?: string;
  createdAt: string;
  toolName?: string;
  toolCallId?: string;
  arguments?: unknown;
  isError?: boolean;
  // detail: a tool call's one-line description; summary: a failed tool
  // result's one line (shown in the activity log).
  detail?: string;
  summary?: string;
  // The turn's model usage, on the turn's last assistant message.
  usage?: TurnUsage;
  // A "compaction" message's figures; its content is the summary.
  compaction?: CompactionReport;
  // scene: a user message sent as a scene command; content is the command.
  scene?: { slug: string; name: string };
};

// CompactionReport is a context compaction's figures, in estimated tokens.
export type CompactionReport = {
  // "threshold" (automatic) or "manual" (/compact).
  reason?: string;
  before?: number;
  after?: number;
  summarized?: number;
};

// ActivityItem is one line of a turn's activity log: the model's narration
// between tool calls, a tool call with its status, or a context compaction.
export type ActivityItem = {
  kind: "text" | "tool" | "compaction";
  // compaction: its figures; summary is the text that replaced the earlier
  // conversation (history only).
  compaction?: CompactionReport;
  summary?: string;
  text?: string;
  tool?: string;
  id?: string;
  detail?: string;
  status?: "running" | "ok" | "error";
  elapsedMs?: number;
  // output: the tail of a running command's output.
  output?: string;
};

// foldActivity records a tool event in the activity log, as the server does:
// the text written before a call is its narration and moves into the log.
// It returns the new log and the text that remains as the answer so far.
export function foldActivity(
  items: ActivityItem[],
  text: string,
  event: {
    type?: string;
    phase?: string;
    tool?: string;
    id?: string;
    detail?: string;
    text?: string;
    error?: string;
    isError?: boolean;
    elapsedMs?: number;
    compaction?: CompactionReport;
  },
): { items: ActivityItem[]; text: string } {
  const next = [...items];
  const find = () => {
    for (let i = next.length - 1; i >= 0; i--) {
      if (next[i].kind === "tool" && next[i].id === event.id) return i;
    }
    return -1;
  };
  const moveNarration = () => {
    if (text.trim()) next.push({ kind: "text", text: text.trim() });
    text = "";
  };
  if (event.type === "compaction") {
    // Compaction runs after the turn's last call: the answer written so far
    // moves into the log first, so the note follows it.
    if (event.phase === "start") {
      moveNarration();
      next.push({ kind: "compaction", status: "running", compaction: event.compaction });
    } else if (event.phase === "end") {
      const done: ActivityItem = { kind: "compaction", status: event.isError ? "error" : "ok", text: event.error, compaction: event.compaction };
      const i = next.map((item) => item.kind === "compaction" && item.status === "running").lastIndexOf(true);
      // An automatic compaction that found nothing old enough to summarize
      // leaves no trace, as on the server.
      const skipped = !event.isError && event.compaction?.reason !== "manual" && !event.compaction?.summarized;
      if (skipped) {
        if (i >= 0) next.splice(i, 1);
      } else if (i >= 0) next[i] = done;
      else next.push(done);
    }
    return { items: next, text };
  }
  if (event.phase === "start") {
    moveNarration();
    next.push({ kind: "tool", tool: event.tool, id: event.id, detail: event.detail, status: "running" });
  } else if (event.phase === "output") {
    const i = find();
    if (i >= 0) next[i] = { ...next[i], output: event.text };
  } else if (event.phase === "end") {
    let i = find();
    if (i < 0) {
      moveNarration();
      next.push({ kind: "tool", tool: event.tool, id: event.id });
      i = next.length - 1;
    }
    next[i] = { ...next[i], status: event.isError ? "error" : "ok", text: event.text, elapsedMs: event.elapsedMs, output: undefined };
  }
  return { items: next, text };
}

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
};

// CallsPage is one page of a period's calls, newest first.
export type CallsPage = { total: number; page: number; size: number; items: UsageEntry[] };

// CallsFilter narrows the call list: modelKey is a by-model row's key
// (provider/model), kind "chat" or "compaction".
export type CallsFilter = { modelKey?: string; kind?: string; page: number; size: number };

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

// SandboxTool is a toolchain mounted read-only into every sandbox.
export type SandboxTool = { name: string; mount: string; python?: boolean };

// ExtensionTool is a tool from the tools config: source is "command" or "go",
// with "-override" when it replaces the built-in of the same name.
export type ExtensionTool = { name: string; source: string };

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
  // contextWindow: the model's window when its catalog says (OpenRouter).
  contextWindow?: number;
};

// ContextInfo is the context window and compaction threshold (a percentage
// of the window) a model gets, and where each came from.
export type ContextInfo = {
  window: number;
  compactPct: number;
  windowSource: "override" | "openrouter" | "default";
  pctSource: "override" | "default";
};

// ModelParam overrides the window and/or threshold for one model; 0 or absent
// keeps the default.
export type ModelParam = {
  provider: string;
  model: string;
  contextWindow?: number;
  compactPct?: number;
  updatedBy?: string;
  updatedAt?: string;
};

export type ModelParamList = { defaultWindow: number; defaultCompactPct: number; params: ModelParam[] };

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
  // Which tier supplies the key a run would actually use.
  source?: "user" | "public" | "none";
  // Set for an administrator-defined endpoint.
  custom?: boolean;
  protocol?: string;
  baseUrl?: string;
  conversationId?: boolean;
};

// conversationId sends the session id as conversation_id in each request, for
// CodeBuddy gateways (workbuddy2api) whose upstream cache needs it.
export type CustomProvider = { name: string; protocol: string; baseUrl: string; conversationId?: boolean };

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
  // "snapshot" opens every turn stream: the turn so far (text, calls, steps),
  //   so attaching mid-turn loses nothing.
  // "notice" carries transient run status (a retry after a rate-limited or
  //   overloaded request): no conversation content.
  // "usage" reports one model call's tokens and cost as the call finishes.
  // "heartbeat" says what the turn is doing while nothing else happens.
  // "done" / "error" end the turn, with the reason.
  // "compaction" reports a context compaction starting and ending.
  type: "snapshot" | "delta" | "done" | "error" | "tool" | "notice" | "usage" | "heartbeat" | "compaction";
  compaction?: CompactionReport;
  usage?: CallUsage;
  usages?: CallUsage[];
  activity?: ActivityItem[];
  id?: string;
  isError?: boolean;
  text?: string;
  error?: string;
  tool?: string;
  notice?: string;
  phase?: string;
  steps?: number;
  elapsedMs?: number;
  reason?: string;
  // detail: on a tool's start, what the call does; on "done", a note about
  // how the turn ended (the step limit).
  detail?: string;
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

  async createSession(scene?: string): Promise<SessionInfo> {
    const session = await this.request<SessionInfo>("/api/sessions", {
      method: "POST",
      headers: this.headers(true),
      body: JSON.stringify(scene ? { scene } : {}),
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

  async scenes(): Promise<SceneList> {
    return this.request<SceneList>("/api/scenes", { headers: this.headers() });
  }

  async adminScenes(): Promise<SceneList> {
    return this.request<SceneList>("/api/admin/scenes", { headers: this.headers() });
  }

  // putScene creates a scene (from "") or saves over one; a different slug
  // renames it.
  async putScene(from: string, scene: SceneInfo): Promise<SceneList> {
    const { missingTools: _missing, updatedAt: _at, updatedBy: _by, ...body } = scene;
    return this.request<SceneList>(`/api/admin/scenes/${encodeURIComponent(from || "new")}`, {
      method: "PUT",
      headers: this.headers(true),
      body: JSON.stringify(body),
    });
  }

  async deleteScene(slug: string): Promise<SceneList> {
    return this.request<SceneList>(`/api/admin/scenes/${encodeURIComponent(slug)}`, {
      method: "DELETE",
      headers: this.headers(),
    });
  }

  async subagent(): Promise<SubagentView> {
    return this.request<SandboxEnvList>("/api/admin/sandbox-env", { headers: this.headers() });
  }

  async putSandboxEnv(name: string, value: string): Promise<SandboxEnvList> {
    return this.request<SandboxEnvList>("/api/admin/sandbox-env", {
      method: "PUT",
      headers: this.headers(true),
      body: JSON.stringify({ name, value }),
    });
  }

  async deleteSandboxEnv(name: string): Promise<SandboxEnvList> {
    return this.request<SandboxEnvList>(`/api/admin/sandbox-env/${encodeURIComponent(name)}`, {
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

  // patchAdminProvider edits a custom provider in place; a different name
  // renames it, carrying along its keys, models, prices and sessions.
  async patchAdminProvider(name: string, body: CustomProvider): Promise<CustomProvider[]> {
    return this.request<CustomProvider[]>(`/api/admin/providers/${encodeURIComponent(name)}`, {
      method: "PATCH",
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

  // runtimeTools reads what /healthz reports of the tool setup: the
  // toolchains mounted into the sandbox and the configured extension tools.
  async runtimeTools(): Promise<{ sandbox: SandboxTool[]; extensions: ExtensionTool[] }> {
    const health = await this.request<{ sandbox?: { tools?: SandboxTool[] }; extensions?: { tools?: ExtensionTool[] } }>(
      "/healthz",
      { headers: this.headers() },
    );
    return { sandbox: health.sandbox?.tools ?? [], extensions: health.extensions?.tools ?? [] };
  }

  async modelParams(): Promise<ModelParamList> {
    return this.request<ModelParamList>("/api/model-params", { headers: this.headers() });
  }

  async putModelParam(param: ModelParam): Promise<ModelParamList> {
    return this.request<ModelParamList>("/api/admin/model-params", {
      method: "PUT",
      headers: this.headers(true),
      body: JSON.stringify(param),
    });
  }

  // The row is named in the query string, as for prices.
  async deleteModelParam(provider: string, model: string): Promise<ModelParamList> {
    return this.request<ModelParamList>(`/api/admin/model-params?${new URLSearchParams({ provider, model })}`, {
      method: "DELETE",
      headers: this.headers(),
    });
  }

  async putContextDefaults(defaultWindow: number, defaultCompactPct: number): Promise<ModelParamList> {
    return this.request<ModelParamList>("/api/admin/model-params/defaults", {
      method: "PUT",
      headers: this.headers(true),
      body: JSON.stringify({ defaultWindow, defaultCompactPct }),
    });
  }

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

  async usageCalls(query: UsageQuery, filter: CallsFilter, admin = false): Promise<CallsPage> {
    const params = usageParams({ ...query, modelKey: filter.modelKey, kind: filter.kind, page: String(filter.page), size: String(filter.size) });
    return this.request<CallsPage>(`${admin ? "/api/admin/usage/calls" : "/api/usage/calls"}?${params}`, {
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

  // stream starts a turn for prompt and yields the reply text as it grows.
  //
  // The turn runs on the server: if the connection drops, this reattaches and
  // carries on; aborting signal only stops watching (use cancelTurn to stop
  // the turn itself).
  stream(prompt: string, signal: AbortSignal, handlers: TurnHandlers = {}): AsyncGenerator<string> {
    const session = this.requireSession();
    return this.follow(
      session,
      () =>
        fetch(`/api/sessions/${encodeURIComponent(session.id)}/messages?stream=true`, {
          method: "POST",
          headers: this.headers(true),
          body: JSON.stringify({ prompt }),
          signal,
          credentials: "same-origin",
        }),
      signal,
      handlers,
    );
  }

  // attach follows the session's running turn, starting from its snapshot.
  attach(signal: AbortSignal, handlers: TurnHandlers = {}): AsyncGenerator<string> {
    const session = this.requireSession();
    return this.follow(session, () => this.attachRequest(session, signal), signal, handlers);
  }

  async cancelTurn(): Promise<void> {
    const session = this.requireSession();
    await this.request<unknown>(`/api/sessions/${encodeURIComponent(session.id)}/turn/cancel`, {
      method: "POST",
      headers: this.headers(),
    });
  }

  private attachRequest(session: SessionInfo, signal: AbortSignal): Promise<Response> {
    return fetch(`/api/sessions/${encodeURIComponent(session.id)}/turn`, {
      headers: this.headers(),
      signal,
      credentials: "same-origin",
    });
  }

  // follow reads a turn stream to its end, reattaching when the connection
  // drops before it (a network blip, a proxy timeout, a suspended laptop).
  private async *follow(
    session: SessionInfo,
    open: () => Promise<Response>,
    signal: AbortSignal,
    handlers: TurnHandlers,
  ): AsyncGenerator<string> {
    let response = await open();
    if (!response.ok) throw new APIError(await errorMessage(response), response.status);
    for (let attempt = 0; ; ) {
      const ended = yield* this.readTurn(response, signal, handlers);
      if (ended || signal.aborted) return;
      // Dropped mid-turn. The turn is still running on the server.
      handlers.onNotice?.("连接中断，正在重连…");
      for (;;) {
        attempt++;
        if (attempt > 40) throw new Error("连接中断，多次重连失败。刷新页面可以重新接上仍在运行的任务。");
        await sleep(Math.min(500 * 2 ** attempt, 5000), signal);
        if (signal.aborted) return;
        try {
          response = await this.attachRequest(session, signal);
        } catch {
          continue;
        }
        if (response.status === 404) throw new Error("连接中断期间本轮已结束，刷新页面查看结果。");
        if (response.ok) break;
      }
      handlers.onNotice?.("");
      attempt = 0;
    }
  }

  // readTurn reads one connection's worth of a turn stream. It returns true when
  // the turn's end arrived, false when the connection ended first.
  private async *readTurn(response: Response, signal: AbortSignal, handlers: TurnHandlers): AsyncGenerator<string, boolean> {
    if (!response.body) throw new Error("当前浏览器不支持流式响应");
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let pending = "";
    let text = "";
    let calls: CallUsage[] = [];
    let activity: ActivityItem[] = [];
    for (;;) {
      let chunk: ReadableStreamReadResult<Uint8Array>;
      try {
        chunk = await reader.read();
      } catch (cause) {
        if (signal.aborted) return false;
        // A network error mid-stream is a dropped connection, not a failure.
        void cause;
        return false;
      }
      pending += decoder.decode(chunk.value ?? new Uint8Array(), { stream: !chunk.done });
      const lines = pending.split("\n");
      pending = lines.pop() ?? "";
      for (const line of lines) {
        if (!line.trim()) continue;
        const event = JSON.parse(line) as StreamEvent;
        switch (event.type) {
          case "snapshot":
            text = event.text ?? "";
            calls = event.usages ?? [];
            activity = event.activity ?? [];
            handlers.onUsage?.([...calls]);
            handlers.onActivity?.(activity);
            yield text;
            break;
          case "delta":
            // Output is flowing, so any pending notice is over.
            handlers.onNotice?.("");
            text += event.text ?? "";
            yield text;
            break;
          case "usage":
            if (event.usage) {
              calls = [...calls, event.usage];
              handlers.onUsage?.(calls);
            }
            break;
          case "notice":
            handlers.onNotice?.(event.text ?? "");
            break;
          case "compaction":
            ({ items: activity, text } = foldActivity(activity, text, event));
            handlers.onActivity?.(activity);
            yield text;
            break;
          case "tool": {
            ({ items: activity, text } = foldActivity(activity, text, event));
            handlers.onActivity?.(activity);
            if (event.phase === "start") {
              handlers.onStatus?.({ phase: "tool", tool: event.tool, since: Date.now(), steps: event.steps ?? 0 });
            } else if (event.phase === "end") {
              handlers.onStatus?.({ phase: "model", since: Date.now(), steps: event.steps ?? 0 });
            }
            // Re-render: the log changed, and narration may have left the text.
            yield text;
            break;
          }
          case "heartbeat":
            handlers.onStatus?.({
              phase: event.phase === "tool" ? "tool" : "model",
              tool: event.tool,
              since: Date.now() - (event.elapsedMs ?? 0),
              steps: event.steps ?? 0,
            });
            break;
          case "done": {
            handlers.onNotice?.("");
            handlers.onStatus?.(null);
            const end = { reason: event.reason ?? "done", steps: event.steps ?? 0, message: event.detail };
            handlers.onEnd?.(end);
            yield event.text ?? text;
            return true;
          }
          case "error": {
            handlers.onNotice?.("");
            handlers.onStatus?.(null);
            const end = { reason: event.reason ?? "error", steps: event.steps ?? 0, message: event.error };
            handlers.onEnd?.(end);
            throw new TurnError(event.error || "请求失败", end);
          }
        }
      }
      if (chunk.done) return false;
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

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, ms);
    signal.addEventListener("abort", () => {
      clearTimeout(timer);
      resolve();
    }, { once: true });
  });
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
