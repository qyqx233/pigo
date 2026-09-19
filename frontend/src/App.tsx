import {
  ActionBarPrimitive,
  AssistantRuntimeProvider,
  ComposerPrimitive,
  MessagePrimitive,
  ThreadPrimitive,
  unstable_useSlashCommandAdapter,
  useAui,
  useAuiState,
  useLocalRuntime,
  type ChatModelAdapter,
  type ThreadMessage,
  type ThreadMessageLike,
  type Unstable_SlashCommand,
} from "@assistant-ui/react";
import { AdminSettings, AdminUsers } from "./admin";
import { Prices, SessionCost, UsageLine, UsagePanel, totalsOf } from "./billing";
import { navigate, navigateEvent, parsePath, type Route } from "./route";
import { Models } from "./models";
import { Providers } from "./providers";
import { MarkdownText } from "./markdown";
import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type FormEvent,
  type MouseEvent as ReactMouseEvent,
  type ReactNode,
} from "react";
import {
  APIError,
  PigoAPI,
  continuableReasons,
  TurnError,
  type CallUsage,
  type TurnEnd,
  type TurnHandlers,
  type TurnInfo,
  type TurnStatus,
  type TurnUsage,
  type UsageTotals,
  type HistoryMessage,
  type CustomModelInfo,
  type ModelInfo,
  type ProviderInfo,
  type SessionInfo,
  type SessionSettings,
  type SlashCommandInfo,
  type UserInfo,
  type WorkspaceEntry,
} from "./api";

type PigoContextValue = {
  api: PigoAPI;
  user: UserInfo | null;
  authReady: boolean;
  session: SessionInfo | null;
  sessions: SessionInfo[];
  commands: SlashCommandInfo[];
  models: ModelInfo[];
  modelsError: string;
  reloadModels(): Promise<void>;
  // sessionCost is the open session's running total, updated live as each
  // model call reports its cost.
  sessionCost: UsageTotals | null;
  error: string;
  // notice is transient run status (a retry after a rate-limited or
  // overloaded request, a reconnect). Empty when there is nothing to say.
  notice: string;
  // runStatus is what the running turn is doing (from its heartbeat).
  runStatus: TurnStatus | null;
  // lastTurn is how the open session's previous turn ended, when it did not
  // simply finish and the page did not watch it end (a reload, a restart).
  lastTurn: TurnInfo | null;
  dismissLastTurn(): void;
  theme: Theme;
  setTheme(theme: Theme): void;
  commandBrowserOpen: boolean;
  setCommandBrowserOpen(open: boolean): void;
  login(username: string, password: string): Promise<void>;
  register(username: string, password: string, registrationToken?: string): Promise<void>;
  logout(): Promise<void>;
  saveSettings(settings: SessionSettings): Promise<void>;
  newSession(): Promise<void>;
  selectSession(id: string): Promise<void>;
  deleteSession(id: string): Promise<void>;
};

type Theme = "dark" | "light";

const PigoContext = createContext<PigoContextValue | null>(null);

function textOf(message: ThreadMessage): string {
  return message.content
    .filter((part) => part.type === "text")
    .map((part) => part.text)
    .join("\n");
}

function currentSessionStorageKey(userID: string) {
  return `pigo.currentSession.${userID}`;
}

function toThreadMessages(messages: HistoryMessage[]): ThreadMessageLike[] {
  return messages
    .filter(
      (message): message is HistoryMessage & { role: "user" | "assistant"; content: string } =>
        (message.role === "user" || message.role === "assistant") && Boolean(message.content),
    )
    .map((message) => ({
      id: message.id,
      role: message.role,
      content: [{ type: "text" as const, text: message.content }],
      createdAt: new Date(message.createdAt),
      ...(message.role === "assistant"
        ? { status: { type: "complete" as const, reason: "stop" as const } }
        : {}),
      ...(message.usage ? { metadata: { custom: { usage: message.usage } } } : {}),
    }));
}

function RuntimeProvider({ children }: { children: ReactNode }) {
  const apiRef = useRef<PigoAPI | null>(null);
  if (!apiRef.current) apiRef.current = new PigoAPI();
  const api = apiRef.current;
  const [user, setUser] = useState<UserInfo | null>(null);
  const [authReady, setAuthReady] = useState(false);
  const [session, setSession] = useState<SessionInfo | null>(null);
  const [sessions, setSessions] = useState<SessionInfo[]>([]);
  const [commands, setCommands] = useState<SlashCommandInfo[]>([]);
  const [models, setModels] = useState<ModelInfo[]>([]);
  const [modelsError, setModelsError] = useState("");
  const [error, setError] = useState("");
  const [runNotice, setRunNotice] = useState("");
  const [runStatus, setRunStatus] = useState<TurnStatus | null>(null);
  const [lastTurn, setLastTurn] = useState<TurnInfo | null>(null);
  const [sessionCost, setSessionCost] = useState<UsageTotals | null>(null);
  const [commandBrowserOpen, setCommandBrowserOpen] = useState(false);
  const [theme, setThemeState] = useState<Theme>(() => {
    let initial: Theme = "dark";
    try {
      if (window.localStorage.getItem("pigo.theme") === "light") initial = "light";
    } catch {
      // Storage may be unavailable in hardened/private browser contexts.
    }
    document.documentElement.dataset.theme = initial;
    return initial;
  });

  function setTheme(next: Theme) {
    setThemeState(next);
    document.documentElement.dataset.theme = next;
    try {
      window.localStorage.setItem("pigo.theme", next);
    } catch {
      // The active page still switches even when persistence is unavailable.
    }
  }

  // followTurn turns a turn stream into assistant-ui run results: the reply
  // text, and as metadata the turn's usage and, once it ends, how it ended.
  // It serves both a new message and reattaching to a turn already running.
  //
  // fresh is false when reattaching: the session total loaded from the server
  // already includes the calls in the snapshot, so only later ones are added.
  async function* followTurn(open: (handlers: TurnHandlers) => AsyncGenerator<string>, signal: AbortSignal, fresh: boolean) {
    setError("");
    setRunNotice("");
    setLastTurn(null);
    let calls: CallUsage[] = [];
    let counted = fresh ? 0 : -1;
    let end: TurnEnd | null = null;
    const handlers: TurnHandlers = {
      onNotice: setRunNotice,
      onStatus: setRunStatus,
      onUsage(list) {
        if (counted < 0) counted = list.length;
        for (const call of list.slice(counted)) setSessionCost((current) => addCall(current, call));
        counted = Math.max(counted, list.length);
        calls = list;
      },
      onEnd(next) {
        end = next;
      },
    };
    const shape = (text: string) => ({
      content: [{ type: "text" as const, text }],
      metadata: { custom: { ...(calls.length > 0 ? { usage: totalsOf(calls) } : {}), ...(end ? { end } : {}) } },
    });
    let last = "";
    try {
      for await (const text of open(handlers)) {
        last = text;
        yield shape(text);
      }
      // Once more, so usage and the ending that arrived with the final text
      // are on the message.
      yield shape(last);
    } catch (cause) {
      if (end) yield shape(last);
      // A turn that ended short (stopped, a limit, an upstream error) says so
      // on its message; the top bar's error is for the connection itself.
      if (!(cause instanceof TurnError)) setError(cause instanceof Error ? cause.message : String(cause));
      throw cause;
    } finally {
      setRunNotice("");
      setRunStatus(null);
    }
    // Watching was abandoned (another session was opened): the turn goes on,
    // and there is nothing of this session to refresh.
    if (signal.aborted) return;
    void Promise.all([api.sessionInfo(), api.commands(), api.sessions()])
      .then(([nextSession, nextCommands, nextSessions]) => {
        setSession(nextSession);
        setCommands(nextCommands);
        setSessions(nextSessions);
        // The ledger's figure replaces the live sum, which cannot see
        // calls from another tab.
        return api.sessionUsage(nextSession.id).then(setSessionCost);
      })
      .catch(() => undefined);
  }

  const adapter = useMemo<ChatModelAdapter>(
    () => ({
      async *run({ messages, abortSignal }) {
        const prompt = textOf(messages.at(-1)!);
        if (prompt.trim() === "/help") {
          setCommandBrowserOpen(true);
          yield {
            content: [{ type: "text", text: "已打开命令浏览器，可搜索或选择命令。" }],
          };
          return;
        }
        yield* followTurn((handlers) => api.stream(prompt, abortSignal, handlers), abortSignal, true);
      },
    }),
    [api],
  );
  const runtime = useLocalRuntime(adapter);

  async function loadModels() {
    setModelsError("");
    try {
      setModels(await api.models());
    } catch (cause) {
      setModelsError(cause instanceof Error ? cause.message : String(cause));
    }
  }

  async function openSession(next: SessionInfo, currentUser: UserInfo) {
    api.selectSession(next);
    setSession(next);
    try {
      window.localStorage.setItem(currentSessionStorageKey(currentUser.id), next.id);
    } catch {
      // The selected session still works for this page lifetime.
    }
    // Stop watching whatever turn the page was following; on the server it
    // carries on.
    runtime.thread.cancelRun();
    const history = await api.history(next.id);
    const threadMessages = toThreadMessages(history);
    runtime.thread.reset(threadMessages);
    const running = next.turn?.status === "running";
    if (running) {
      // A turn is still running (the page was reloaded, or opened elsewhere):
      // follow it from its snapshot, under the user message that started it.
      runtime.thread.resumeRun({
        parentId: threadMessages.at(-1)?.id ?? null,
        stream: ({ abortSignal }) => followTurn((handlers) => api.attach(abortSignal, handlers), abortSignal, false),
      });
    }
    setLastTurn(!running && next.lastTurn && next.lastTurn.reason !== "done" ? next.lastTurn : null);
    setSessionCost(null);
    void api.sessionUsage(next.id).then(setSessionCost).catch(() => undefined);
    try {
      setCommands(await api.commands());
    } catch {
      setCommands([]);
    }
  }

  async function restoreUser(currentUser: UserInfo) {
    setError("");
    try {
      let savedID = "";
      try {
        savedID = window.localStorage.getItem(currentSessionStorageKey(currentUser.id)) ?? "";
      } catch {
        // Fall back to the newest server-side session.
      }
      let selected: SessionInfo | null = null;
      if (savedID) {
        try {
          selected = await api.getSession(savedID);
        } catch (cause) {
          if (!(cause instanceof APIError) || cause.status !== 404) throw cause;
        }
      }
      let available = await api.sessions();
      if (!selected && available.length > 0) selected = await api.getSession(available[0].id);
      if (!selected) {
        selected = await api.createSession();
        available = [selected, ...available];
      }
      setSessions(available.some((item) => item.id === selected.id) ? available : [selected, ...available]);
      await openSession(selected, currentUser);
      void loadModels();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    }
  }

  useEffect(() => {
    void api
      .me()
      .then(async (currentUser) => {
        setUser(currentUser);
        await restoreUser(currentUser);
      })
      .catch((cause) => {
        if (!(cause instanceof APIError) || cause.status !== 401) {
          setError(cause instanceof Error ? cause.message : String(cause));
        }
      })
      .finally(() => setAuthReady(true));
  }, []);

  async function authenticate(mode: "login" | "register", username: string, password: string, registrationToken = "") {
    setError("");
    try {
      const currentUser =
        mode === "login" ? await api.login(username, password) : await api.register(username, password, registrationToken);
      setUser(currentUser);
      setAuthReady(true);
      await restoreUser(currentUser);
    } catch (cause) {
      const message = cause instanceof Error ? cause.message : String(cause);
      setError(message);
      throw cause;
    }
  }

  async function login(username: string, password: string) {
    await authenticate("login", username, password);
  }

  async function register(username: string, password: string, registrationToken = "") {
    await authenticate("register", username, password, registrationToken);
  }

  async function logout() {
    await api.logout();
    api.selectSession(null);
    runtime.thread.reset();
    setUser(null);
    setSession(null);
    setSessions([]);
    setCommands([]);
    setModels([]);
    setModelsError("");
    setSessionCost(null);
    setLastTurn(null);
    setError("");
  }

  async function saveSettings(settings: SessionSettings) {
    setError("");
    try {
      const updated = await api.updateSession(settings);
      setSession(updated);
      setSessions((current) => current.map((item) => (item.id === updated.id ? updated : item)));
    } catch (cause) {
      const message = cause instanceof Error ? cause.message : String(cause);
      setError(message);
      throw cause;
    }
  }

  // newSession and selectSession switch the visible conversation, so they also
  // show it: called from the settings page, the thread is not rendered and a
  // switch that stays there looks like a button that does nothing — while
  // quietly creating an empty session per click. Navigation lives here rather
  // than at each button so every caller (top bar, history drawer, its "新建"
  // button) gets it. openSession itself does not navigate: it also runs on a
  // cold load of /settings/..., which must stay where the URL says.
  async function newSession() {
    if (!user) return;
    setError("");
    try {
      const created = await api.createSession();
      setSessions((current) => [created, ...current]);
      await openSession(created, user);
      navigate("/");
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    }
  }

  async function selectSession(id: string) {
    if (!user) return;
    // Already open: nothing to load, but still show it.
    if (session?.id === id) {
      navigate("/");
      return;
    }
    setError("");
    try {
      await openSession(await api.getSession(id), user);
      navigate("/");
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
      throw cause;
    }
  }

  async function deleteSession(id: string) {
    if (!user) return;
    setError("");
    await api.deleteSession(id);
    const remaining = sessions.filter((item) => item.id !== id);
    setSessions(remaining);
    if (session?.id !== id) return;
    if (remaining.length > 0) {
      await openSession(await api.getSession(remaining[0].id), user);
    } else {
      const created = await api.createSession();
      setSessions([created]);
      await openSession(created, user);
    }
  }

  return (
    <PigoContext.Provider
      value={{
        api,
        user,
        authReady,
        session,
        sessions,
        commands,
        models,
        modelsError,
        reloadModels: loadModels,
        sessionCost,
        error,
        notice: runNotice,
        runStatus,
        lastTurn,
        dismissLastTurn: () => setLastTurn(null),
        theme,
        setTheme,
        commandBrowserOpen,
        setCommandBrowserOpen,
        login,
        register,
        logout,
        saveSettings,
        newSession,
        selectSession,
        deleteSession,
      }}
    >
      <AssistantRuntimeProvider runtime={runtime}>
        {children}
      </AssistantRuntimeProvider>
    </PigoContext.Provider>
  );
}

// addCall folds one live call into the session total.
function addCall(current: UsageTotals | null, call: CallUsage): UsageTotals {
  const base: UsageTotals = current ?? {
    calls: 0, input: 0, cacheRead: 0, cacheWrite: 0, output: 0, reasoning: 0,
    cost: 0, platformCost: 0, selfCost: 0, unpriced: 0, incomplete: 0,
  };
  const { details: _details, ...one } = totalsOf([call]);
  return {
    calls: base.calls + one.calls,
    input: base.input + one.input,
    cacheRead: base.cacheRead + one.cacheRead,
    cacheWrite: base.cacheWrite + one.cacheWrite,
    output: base.output + one.output,
    reasoning: base.reasoning + one.reasoning,
    cost: base.cost + one.cost,
    platformCost: base.platformCost + one.platformCost,
    selfCost: base.selfCost + one.selfCost,
    unpriced: base.unpriced + one.unpriced,
    incomplete: base.incomplete + one.incomplete,
  };
}

function usePigo() {
  const value = useContext(PigoContext);
  if (!value) throw new Error("PigoContext is missing");
  return value;
}

export function App() {
  return (
    <RuntimeProvider>
      <AuthGate />
    </RuntimeProvider>
  );
}

function AuthGate() {
  const { user, authReady } = usePigo();
  if (!authReady) {
    return <div className="auth-loading"><div className="auth-logo">π</div><span>正在恢复登录状态…</span></div>;
  }
  return user ? <Shell /> : <AuthScreen />;
}

function AuthScreen() {
  const { login, register, error, theme, setTheme } = usePigo();
  const [mode, setMode] = useState<"login" | "register">("login");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [registrationToken, setRegistrationToken] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [localError, setLocalError] = useState("");

  async function submit(event: FormEvent) {
    event.preventDefault();
    setLocalError("");
    if (mode === "register" && password !== confirmPassword) {
      setLocalError("两次输入的密码不一致");
      return;
    }
    setSubmitting(true);
    try {
      if (mode === "login") await login(username, password);
      else await register(username, password, registrationToken);
    } catch {
      // The provider exposes the server error next to the form.
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <main className="auth-page">
      <button
        className="icon-button auth-theme"
        type="button"
        aria-label="切换主题"
        onClick={() => setTheme(theme === "dark" ? "light" : "dark")}
      >
        <ThemeIcon theme={theme} />
      </button>
      <section className="auth-card">
        <div className="auth-brand"><span>π</span><strong>pigo</strong></div>
        <div className="auth-heading">
          <span className="eyebrow">PIGO AGENT WORKSPACE</span>
          <h1>{mode === "login" ? "欢迎回来" : "创建你的账号"}</h1>
          <p>{mode === "login" ? "登录后继续之前的会话与工作区。" : "账号会隔离你的会话、历史和工作区。"}</p>
        </div>
        <div className="auth-tabs" role="tablist">
          <button type="button" className={mode === "login" ? "active" : ""} onClick={() => { setMode("login"); setLocalError(""); }}>登录</button>
          <button type="button" className={mode === "register" ? "active" : ""} onClick={() => { setMode("register"); setLocalError(""); }}>注册</button>
        </div>
        <form className="auth-form" onSubmit={(event) => void submit(event)}>
          <label>用户名
            <input value={username} onChange={(event) => setUsername(event.target.value)} autoComplete="username" placeholder="3-32 位字母、数字或 _ . -" minLength={3} maxLength={32} required autoFocus />
          </label>
          <label>密码
            <input type="password" value={password} onChange={(event) => setPassword(event.target.value)} autoComplete={mode === "login" ? "current-password" : "new-password"} placeholder="至少 8 个字符" minLength={8} maxLength={72} required />
          </label>
          {mode === "register" && (
            <>
              <label>确认密码
                <input type="password" value={confirmPassword} onChange={(event) => setConfirmPassword(event.target.value)} autoComplete="new-password" placeholder="再次输入密码" minLength={8} maxLength={72} required />
              </label>
              <label>服务器注册 Token <small>注册账号时需要，登录后不再使用</small>
                <input type="password" value={registrationToken} onChange={(event) => setRegistrationToken(event.target.value)} autoComplete="off" placeholder="例如启动时配置的 PIGO_SERVER_TOKEN" />
              </label>
            </>
          )}
          {(localError || error) && <div className="auth-error">{localError || error}</div>}
          <button className="primary-button auth-submit" type="submit" disabled={submitting}>
            {submitting ? "请稍候…" : mode === "login" ? "登录并继续" : "注册并开始"}
          </button>
        </form>
      </section>
    </main>
  );
}

function useRoute(): Route {
  const [route, setRoute] = useState<Route>(() => parsePath(window.location.pathname));
  useEffect(() => {
    const sync = () => setRoute(parsePath(window.location.pathname));
    window.addEventListener("popstate", sync);
    window.addEventListener(navigateEvent, sync);
    return () => {
      window.removeEventListener("popstate", sync);
      window.removeEventListener(navigateEvent, sync);
    };
  }, []);
  return route;
}


function Shell() {
  const {
    user,
    session,
    error,
    theme,
    setTheme,
    commandBrowserOpen,
    setCommandBrowserOpen,
    newSession,
    logout,
    sessionCost,
  } = usePigo();
  const [historyOpen, setHistoryOpen] = useState(false);
  const route = useRoute();
  const onSettings = route.name === "settings";

  return (
    <main className="app-shell">
      <header className="topbar">
        <div className="brand-mark" aria-hidden="true">
          π
        </div>
        <div className="brand-copy">
          <strong>pigo</strong>
          <span className="connection-status">
            <i className={error ? "status-dot error-dot" : "status-dot"} />
            {error
              ? error
              : session
                ? `${session.model} · ${session.provider}`
                : "正在连接…"}
          </span>
        </div>
        <div className="topbar-spacer" />
        <SessionCost totals={sessionCost} />
        {session && <ModelPicker />}
        <button className="secondary-button history-button" type="button" onClick={() => setHistoryOpen(true)}>
          历史会话
        </button>
        <button
          className="icon-button"
          type="button"
          aria-label={theme === "dark" ? "切换到浅色主题" : "切换到深色主题"}
          title={theme === "dark" ? "浅色主题" : "深色主题"}
          onClick={() => setTheme(theme === "dark" ? "light" : "dark")}
        >
          <ThemeIcon theme={theme} />
        </button>
        <button
          className={onSettings ? "icon-button icon-button-active" : "icon-button"}
          type="button"
          aria-label={onSettings ? "返回对话" : "打开设置"}
          title={onSettings ? "返回对话" : "设置"}
          aria-pressed={onSettings}
          onClick={() => navigate(onSettings ? "/" : "/settings")}
        >
          <SettingsIcon />
        </button>
        <button className="secondary-button" type="button" onClick={() => void newSession()}>
          新会话
        </button>
        <button className="user-button" type="button" title="退出登录" onClick={() => void logout()}>
          <span>{user?.username.slice(0, 1).toUpperCase()}</span>
          <small>{user?.username}</small>
        </button>
      </header>
      {onSettings ? <SettingsPage tab={route.tab} /> : <Thread />}
      <HistoryDrawer open={historyOpen} onClose={() => setHistoryOpen(false)} />
      <CommandBrowser
        open={commandBrowserOpen}
        onClose={() => setCommandBrowserOpen(false)}
      />
    </main>
  );
}

// modelGroups orders the picker the same way the server orders the list.
const modelGroups = [
  { source: "custom", title: "本服务的模型" },
  { source: "free", title: "OpenRouter 免费" },
  { source: "preset", title: "已配置 Key" },
] as const;

// ModelPicker switches the conversation's model from the top bar, without a
// trip to the settings page. It reads the same /api/models list as the settings
// picker and /models, so the three never disagree about what is available.
function ModelPicker() {
  const { session, models, modelsError, reloadModels, saveSettings } = usePigo();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [busy, setBusy] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    // Close on a click anywhere else, or Escape.
    const onPointer = (event: PointerEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) setOpen(false);
    };
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    window.addEventListener("pointerdown", onPointer);
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("pointerdown", onPointer);
      window.removeEventListener("keydown", onKey);
    };
  }, [open]);

  if (!session) return null;
  const needle = query.trim().toLowerCase();
  const matches = models.filter(
    (item) =>
      !needle ||
      item.id.toLowerCase().includes(needle) ||
      item.label.toLowerCase().includes(needle) ||
      item.provider.toLowerCase().includes(needle),
  );

  async function choose(id: string, providerName: string) {
    if (!session) return;
    setBusy(true);
    try {
      await saveSettings({ model: id, provider: providerName, thinking: session.thinking ?? "medium" });
      setOpen(false);
      setQuery("");
    } catch {
      // saveSettings has already surfaced the error in the status line.
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="model-picker" ref={rootRef}>
      <button
        type="button"
        className="model-pill model-pill-button"
        aria-haspopup="listbox"
        aria-expanded={open}
        title={`${session.model} · ${session.provider}（点击切换模型）`}
        onClick={() => {
          setOpen(!open);
          if (!open && models.length === 0) void reloadModels();
        }}
      >
        {session.model} <span aria-hidden="true">▾</span>
      </button>
      {open && (
        <div className="model-menu" role="listbox" aria-label="切换模型">
          <input
            className="settings-input model-menu-search"
            placeholder="搜索模型 / provider"
            autoFocus
            value={query}
            onChange={(event) => setQuery(event.target.value)}
          />
          <div className="model-menu-list">
            {modelGroups.map((group) => {
              const items = matches.filter((item) => (item.source ?? "free") === group.source);
              if (items.length === 0) return null;
              return (
                <div key={group.source} className="model-menu-group">
                  <span className="eyebrow">{group.title}</span>
                  {items.map((item) => {
                    const current = item.id === session.model;
                    return (
                      <button
                        key={`${item.provider}:${item.id}`}
                        type="button"
                        role="option"
                        aria-selected={current}
                        className={current ? "model-menu-item current" : "model-menu-item"}
                        disabled={busy}
                        onClick={() => void choose(item.id, item.provider)}
                      >
                        <span className="model-menu-label">{item.label}</span>
                        <span className="model-menu-meta">
                          {item.provider}
                          {current ? " · 当前" : ""}
                        </span>
                      </button>
                    );
                  })}
                </div>
              );
            })}
            {matches.length === 0 && (
              <p className="model-menu-empty">
                {modelsError ? `模型列表加载失败：${modelsError}` : models.length === 0 ? "正在加载…" : "没有匹配的模型"}
              </p>
            )}
          </div>
          <button
            type="button"
            className="model-menu-manage"
            onClick={() => {
              setOpen(false);
              navigate("/settings/models");
            }}
          >
            管理模型…
          </button>
        </div>
      )}
    </div>
  );
}

function HistoryDrawer({ open, onClose }: { open: boolean; onClose(): void }) {
  const { session, sessions, selectSession, deleteSession, newSession } = usePigo();
  const [busyID, setBusyID] = useState("");

  useEffect(() => {
    if (!open) return;
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    window.addEventListener("keydown", closeOnEscape);
    return () => window.removeEventListener("keydown", closeOnEscape);
  }, [open, onClose]);

  if (!open) return null;

  async function choose(id: string) {
    setBusyID(id);
    try {
      await selectSession(id);
      onClose();
    } finally {
      setBusyID("");
    }
  }

  async function remove(event: ReactMouseEvent, item: SessionInfo) {
    event.stopPropagation();
    if (!window.confirm(`确定删除“${item.title || "新会话"}”？对话记录和工作区文件都会被删除。`)) return;
    setBusyID(item.id);
    try {
      await deleteSession(item.id);
    } finally {
      setBusyID("");
    }
  }

  return (
    <div className="history-layer">
      <button className="settings-backdrop" type="button" aria-label="关闭历史会话" onClick={onClose} />
      <aside className="history-drawer" role="dialog" aria-modal="true" aria-label="历史会话">
        <header className="settings-header">
          <div><span className="eyebrow">CONVERSATIONS</span><h2>历史会话</h2></div>
          <button className="icon-button close-button" type="button" onClick={onClose} aria-label="关闭">×</button>
        </header>
        <div className="history-actions">
          <button className="primary-button full-button" type="button" onClick={() => void newSession().then(onClose)}>＋ 新建会话</button>
        </div>
        <div className="history-list">
          {sessions.map((item) => (
            <div key={item.id} className={`history-item ${item.id === session?.id ? "active" : ""}`}>
              <button className="history-open" type="button" disabled={Boolean(busyID)} onClick={() => void choose(item.id)}>
                <span className="history-item-copy">
                  <strong>{item.title || "新会话"}</strong>
                  <small>{formatSessionTime(item.lastUsed)} · {item.model}</small>
                </span>
              </button>
              <button className="history-delete" type="button" disabled={Boolean(busyID)} aria-label="删除会话" title="删除会话" onClick={(event) => void remove(event, item)}>×</button>
            </div>
          ))}
          {sessions.length === 0 && <div className="history-empty">还没有历史会话</div>}
        </div>
      </aside>
    </div>
  );
}

function formatSessionTime(value: string) {
  const date = new Date(value);
  const today = new Date();
  if (date.toDateString() === today.toDateString()) {
    return date.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" });
  }
  return date.toLocaleDateString("zh-CN", { month: "short", day: "numeric" });
}

function ThemeIcon({ theme }: { theme: Theme }) {
  if (theme === "dark") {
    return (
      <svg viewBox="0 0 24 24" aria-hidden="true">
        <circle cx="12" cy="12" r="4" />
        <path d="M12 2v2M12 20v2M4.93 4.93l1.42 1.42M17.65 17.65l1.42 1.42M2 12h2M20 12h2M4.93 19.07l1.42-1.42M17.65 6.35l1.42-1.42" />
      </svg>
    );
  }
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true">
      <path d="M20.4 15.2A8.5 8.5 0 0 1 8.8 3.6 8.5 8.5 0 1 0 20.4 15.2Z" />
    </svg>
  );
}

function SettingsIcon() {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true">
      <path d="M12 15.25A3.25 3.25 0 1 0 12 8.75a3.25 3.25 0 0 0 0 6.5Z" />
      <path d="M19.4 13.5a7.8 7.8 0 0 0 .05-1 7.8 7.8 0 0 0-.05-1l2-1.55-2-3.46-2.46 1a7.6 7.6 0 0 0-1.72-1L14.85 4h-4l-.38 2.49a7.6 7.6 0 0 0-1.72 1l-2.45-1-2 3.46 2 1.55a7.8 7.8 0 0 0-.06 1 7.8 7.8 0 0 0 .06 1l-2 1.55 2 3.46 2.45-1a7.6 7.6 0 0 0 1.72 1l.38 2.49h4l.37-2.49a7.6 7.6 0 0 0 1.72-1l2.46 1 2-3.46-2-1.55Z" />
    </svg>
  );
}

// The settings page. Each panel is a hash route (#/settings/<tab>), so a
// specific panel can be linked to and the back button leaves settings rather
// than the browser.
type SettingsTab = { key: string; label: string };
type SettingsGroup = { title: string; adminOnly?: boolean; tabs: SettingsTab[] };

// One panel per route, grouped by who it belongs to. The administrator's four
// concerns are separate panels rather than one console, so nothing here is
// taller than a screen.
// Organised by the object being configured, not by who owns it: a provider is
// one row whoever supplies its key, and a model is one row whoever added it.
// Only the two things that are genuinely deployment-wide get an admin group.
const settingsGroups: SettingsGroup[] = [
  {
    title: "配置",
    tabs: [
      { key: "session", label: "会话" },
      { key: "providers", label: "Provider" },
      { key: "models", label: "模型" },
      { key: "workspace", label: "工作区" },
      { key: "usage", label: "用量" },
      { key: "prices", label: "模型价格" },
      { key: "runtime", label: "运行状态" },
    ],
  },
  {
    title: "管理",
    adminOnly: true,
    tabs: [
      { key: "admin", label: "部署" },
      { key: "admin-users", label: "用户" },
      { key: "admin-usage", label: "全员用量" },
    ],
  },
];

function SettingsPage({ tab }: { tab: string }) {
  const { api, user, session, commands, models, modelsError, reloadModels, error, saveSettings } = usePigo();
  const [model, setModel] = useState("");
  const [thinking, setThinking] = useState("medium");
  const [saving, setSaving] = useState(false);
  const [notice, setNotice] = useState("");
  const [customModels, setCustomModels] = useState<CustomModelInfo[]>([]);
  const [providers, setProviders] = useState<ProviderInfo[]>([]);

  useEffect(() => {
    setModel(session?.model ?? "");
    setThinking(session?.thinking ?? "medium");
    setNotice("");
    void refreshCustomModels();
    void refreshProviders();
  }, [session?.model, session?.thinking]);

  // Escape returns to the conversation, matching what the drawer used to do.
  useEffect(() => {
    const backOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") navigate("/");
    };
    window.addEventListener("keydown", backOnEscape);
    return () => window.removeEventListener("keydown", backOnEscape);
  }, []);
  const skillCount = commands.filter((command) => command.source === "skill").length;
  const unavailableCount = commands.filter((command) => !command.available).length;
  // /api/models 要等 OpenRouter 实时拉取，可能很慢；自定义模型来自本地存储，
  // 直接合并进候选列表，保证添加后立刻可选。
  const datalistModels = [...models];
  {
    const known = new Set(models.map((item) => item.id));
    for (const item of customModels) {
      if (!item.expired && !known.has(item.id)) {
        datalistModels.push({ id: item.id, label: `${item.label} · 自定义`, provider: item.provider, source: "custom" });
      }
    }
  }
  const modelChoiceIds = new Set(datalistModels.map((item) => item.id));

  async function applySessionSettings() {
    const modelID = model.trim();
    if (!modelID) return;
    setSaving(true);
    setNotice("");
    try {
      const selected = datalistModels.find((item) => item.id === modelID);
      await saveSettings({ model: modelID, provider: selected?.provider, thinking });
      setNotice("会话设置已更新，将从下一轮开始生效。");
    } catch {
      setNotice("");
    } finally {
      setSaving(false);
    }
  }

  // The provider list feeds the model panel's dropdown, so it has to be
  // reloaded when the Provider panel adds one — the panels share this component
  // instance and switching tabs does not remount it.
  async function refreshProviders() {
    try {
      setProviders(await api.providers());
    } catch {
      setProviders([]);
    }
  }

  async function refreshCustomModels() {
    try {
      setCustomModels(await api.customModels());
    } catch {
      setCustomModels([]);
    }
  }



  const visibleGroups = settingsGroups.filter((group) => !group.adminOnly || user?.admin);
  const visibleKeys = visibleGroups.flatMap((group) => group.tabs.map((item) => item.key));
  const active = visibleKeys.includes(tab) ? tab : "session";

  return (
    <div className="settings-page">
      <nav className="settings-nav" aria-label="设置分组">
        {visibleGroups.map((group) => (
          <div key={group.title} className="settings-nav-group">
            <span className="eyebrow">{group.title}</span>
            {group.tabs.map((item) => (
              <button
                key={item.key}
                type="button"
                className={item.key === active ? "settings-nav-item active" : "settings-nav-item"}
                aria-current={item.key === active ? "page" : undefined}
                onClick={() => navigate(`/settings/${item.key}`)}
              >
                {item.label}
              </button>
            ))}
          </div>
        ))}
        <button type="button" className="settings-nav-back" onClick={() => navigate("/")}>
          ← 返回对话
        </button>
      </nav>

      <div className="settings-pane">
        {active === "session" && (
          <section className="settings-section">
            <div className="section-heading">
              <div>
                <h3>模型与推理</h3>
                <p>应用后保留当前对话上下文。</p>
              </div>
              <span className="section-icon">AI</span>
            </div>
<label className="field-label" htmlFor="model-setting">模型</label>
            <select
            id="model-setting"
            className="settings-input settings-select"
            value={model}
            onChange={(event) => setModel(event.target.value)}
            >
            {/* Grouped by source, the same way the top-bar picker groups them. */}
            {modelGroups.map((group) => {
              const items = datalistModels.filter((item) => (item.source ?? "free") === group.source);
              if (items.length === 0) return null;
              return (
                <optgroup key={group.source} label={group.title}>
                  {items.map((item) => (
                    <option key={`${item.provider}:${item.id}`} value={item.id}>{`${item.label} · ${item.provider}`}</option>
                  ))}
                </optgroup>
              );
            })}
            {model && !modelChoiceIds.has(model) && (
            <option value={model}>{`${model} · 当前（不在目录中）`}</option>
            )}
            </select>
            {modelsError && <p className="credential-note">OpenRouter 免费目录暂时不可用：{modelsError}。其它模型不受影响，仍可在下拉中选择。</p>}
            <label className="field-label" htmlFor="thinking-setting">推理强度</label>
            <select
            id="thinking-setting"
            className="settings-input settings-select"
            value={thinking}
            onChange={(event) => setThinking(event.target.value)}
            >
            {['off', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'].map((level) => (
            <option key={level} value={level}>{level}</option>
            ))}
            </select>
            <button className="primary-button full-button" type="button" disabled={saving || !session} onClick={() => void applySessionSettings()}>
            {saving ? "正在保存…" : "保存会话设置"}
            </button>
          </section>
        )}

        {active === "models" && (
          <Models
            api={api}
            providers={providers}
            isAdmin={!!user?.admin}
            onChanged={() => void reloadModels()}
          />
        )}

        {active === "providers" && (
          <Providers api={api} isAdmin={!!user?.admin} onChanged={() => void refreshProviders()} />
        )}
        {active === "workspace" && <WorkspaceFiles />}

        {user?.admin && active === "admin" && <AdminSettings api={api} />}
        {user?.admin && active === "admin-users" && <AdminUsers api={api} />}
        {user?.admin && active === "admin-usage" && <UsagePanel api={api} admin />}
        {active === "usage" && <UsagePanel api={api} />}
        {active === "prices" && (
          <Prices api={api} isAdmin={!!user?.admin} providers={providers} models={datalistModels} />
        )}

        {active === "runtime" && (
          <section className="settings-section">
            <div className="section-heading">
              <div>
                <h3>运行能力</h3>
                <p>主机权限只能由服务端启动参数控制。</p>
              </div>
            </div>
            <div className="metric-grid">
            <div className="metric"><strong>{session?.tools?.length ?? 0}</strong><span>工具</span></div>
            <div className="metric"><strong>{skillCount}</strong><span>技能</span></div>
            <div className="metric"><strong>{commands.length}</strong><span>命令</span></div>
            </div>
            <div className="capability-list">
            <div><span>Provider</span><strong>{session?.provider ?? "—"}</strong></div>
            <div><span>Web 暂不可用命令</span><strong>{unavailableCount}</strong></div>
            <div><span>Session</span><code>{session ? session.id.slice(0, 12) : "—"}</code></div>
            <div><span>Sandbox</span><strong>{session?.sandbox ?? "—"}{session?.alive ? " · 运行中" : ""}</strong></div>
            </div>
            <p className="security-note">模型 API Key 留在编排进程，不会进入 bwrap。<code>bash</code> 在沙箱中执行；<code>read</code>/<code>write</code> 只作用于本会话 workspace。用 <code>-tools all</code> 或 <code>-tools read,grep</code> 控制工具。</p>
          </section>
        )}

        {(notice || error) && (
          <div className={error ? "settings-message settings-message-error" : "settings-message"}>{error || notice}</div>
        )}
      </div>
    </div>
  );
}

const commandSourceMeta: Record<string, { label: string; symbol: string }> = {
  builtin: { label: "内置命令", symbol: "⌘" },
  skill: { label: "技能", symbol: "✦" },
  user: { label: "Prompt", symbol: "P" },
  plugin: { label: "插件", symbol: "◇" },
};

function WorkspaceFiles() {
  const { api, session } = usePigo();
  const [entries, setEntries] = useState<WorkspaceEntry[]>([]);
  const [path, setPath] = useState(".");
  const [error, setError] = useState("");

  useEffect(() => {
    if (!session) return;
    setError("");
    void api
      .files(path)
      .then(setEntries)
      .catch((cause) => setError(cause instanceof Error ? cause.message : String(cause)));
  }, [api, session, path]);

  return (
    <section className="settings-section">
      <div className="section-heading">
        <div>
          <h3>工作区文件</h3>
          <p>当前会话 workspace，仅该会话的 bwrap 可见。</p>
        </div>
      </div>
      <div className="workspace-path">
        <code>{path}</code>
        {path !== "." && (
          <button type="button" className="secondary-button" onClick={() => setPath(".")}>
            根目录
          </button>
        )}
      </div>
      {error && <p className="settings-message settings-message-error">{error}</p>}
      <ul className="workspace-list">
        {entries.map((entry) => (
          <li key={entry.name}>
            {entry.dir ? (
              <button
                type="button"
                className="workspace-link"
                onClick={() => setPath(path === "." ? entry.name : `${path}/${entry.name}`)}
              >
                {entry.name}/
              </button>
            ) : (
              <button
                type="button"
                className="workspace-link"
                onClick={() =>
                  void api.download(path === "." ? entry.name : `${path}/${entry.name}`)
                }
              >
                {entry.name}
                <small>{entry.size} B</small>
              </button>
            )}
          </li>
        ))}
        {entries.length === 0 && !error && <li className="workspace-empty">还没有文件</li>}
      </ul>
    </section>
  );
}

function CommandBrowser({ open, onClose }: { open: boolean; onClose(): void }) {
  const { commands } = usePigo();
  const aui = useAui();
  const [query, setQuery] = useState("");

  useEffect(() => {
    if (!open) return;
    setQuery("");
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    window.addEventListener("keydown", closeOnEscape);
    return () => window.removeEventListener("keydown", closeOnEscape);
  }, [open, onClose]);

  const groups = useMemo(() => {
    const needle = query.trim().toLocaleLowerCase();
    const filtered = commands.filter((command) => {
      if (!needle) return true;
      return [command.name, command.description, command.argumentHint, command.source]
        .join(" ")
        .toLocaleLowerCase()
        .includes(needle);
    });
    return ["builtin", "skill", "user", "plugin"]
      .map((source) => ({
        source,
        commands: filtered
          .filter((command) => command.source === source)
          .sort(
            (left, right) =>
              Number(right.available) - Number(left.available) ||
              left.name.localeCompare(right.name),
          ),
      }))
      .filter((group) => group.commands.length > 0);
  }, [commands, query]);

  if (!open) return null;
  const availableCount = commands.filter((command) => command.available).length;

  function chooseCommand(command: SlashCommandInfo) {
    if (!command.available) return;
    aui.composer.setText(`/${command.name} `);
    onClose();
    requestAnimationFrame(() => document.querySelector<HTMLTextAreaElement>(".composer-input")?.focus());
  }

  return (
    <div className="command-browser-layer">
      <button className="command-browser-backdrop" type="button" aria-label="关闭命令浏览器" onClick={onClose} />
      <section className="command-browser" role="dialog" aria-modal="true" aria-label="命令浏览器">
        <header className="command-browser-header">
          <div>
            <span className="eyebrow">PIGO COMMANDS</span>
            <h2>命令浏览器</h2>
            <p>{availableCount} 个可用 · 共 {commands.length} 个命令</p>
          </div>
          <button className="icon-button close-button" type="button" onClick={onClose} aria-label="关闭命令浏览器">×</button>
        </header>
        <div className="command-search-wrap">
          <span aria-hidden="true">⌕</span>
          <input
            className="command-search"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="搜索命令、能力或来源…"
            autoFocus
          />
          {query && <button type="button" onClick={() => setQuery("")} aria-label="清空搜索">×</button>}
        </div>
        <div className="command-browser-content">
          {groups.map((group) => {
            const meta = commandSourceMeta[group.source] ?? { label: group.source, symbol: "·" };
            return (
              <section className="command-group" key={group.source}>
                <header>
                  <span>{meta.symbol}</span>
                  <h3>{meta.label}</h3>
                  <small>{group.commands.length}</small>
                </header>
                <div className="command-card-grid">
                  {group.commands.map((command) => (
                    <button
                      className="command-card"
                      type="button"
                      key={command.name}
                      disabled={!command.available}
                      onClick={() => chooseCommand(command)}
                    >
                      <span className="command-card-topline">
                        <code>/{command.name}</code>
                        {command.argumentHint && <em>{command.argumentHint}</em>}
                      </span>
                      <span className="command-description">{command.description || "暂无说明"}</span>
                      <span className="command-card-meta">
                        <small>{meta.label}</small>
                        <small className={command.available ? "available-label" : "unavailable-label"}>
                          {command.available ? "可用" : "Web 暂不可用"}
                        </small>
                      </span>
                    </button>
                  ))}
                </div>
              </section>
            );
          })}
          {groups.length === 0 && (
            <div className="command-empty">
              <strong>没有匹配的命令</strong>
              <span>换一个关键词试试。</span>
            </div>
          )}
        </div>
      </section>
    </div>
  );
}

function Thread() {
  const { setCommandBrowserOpen } = usePigo();
  return (
    <ThreadPrimitive.Root className="thread-root">
      <ThreadPrimitive.Viewport className="thread-viewport">
        <ThreadPrimitive.Empty>
          <div className="welcome">
            <div className="welcome-copy">
              <span className="welcome-kicker">PIGO AGENT WORKSPACE</span>
              <div className="welcome-icon">π</div>
              <h1>从一个想法开始</h1>
              <p>和 pigo 一起理解代码、推进任务，或键入 / 调用熟悉的命令。</p>
            </div>
            <div className="starter-grid">
              <button
                className="starter-card"
                type="button"
                onClick={() => setCommandBrowserOpen(true)}
              >
                <span className="starter-symbol">/</span>
                <span><strong>浏览命令</strong><small>查看 Web 中可用的 pigo 能力</small></span>
                <i>↗</i>
              </button>
              <ThreadPrimitive.Suggestion className="starter-card" prompt="请先快速了解这个项目，并告诉我它的结构和入口。" send>
                <span className="starter-symbol">⌘</span>
                <span><strong>了解项目</strong><small>快速梳理仓库结构与关键入口</small></span>
                <i>↗</i>
              </ThreadPrimitive.Suggestion>
              <ThreadPrimitive.Suggestion className="starter-card" prompt="/models" send>
                <span className="starter-symbol">AI</span>
                <span><strong>查看模型</strong><small>列出当前支持的模型与 Provider</small></span>
                <i>↗</i>
              </ThreadPrimitive.Suggestion>
            </div>
          </div>
        </ThreadPrimitive.Empty>
        <ThreadPrimitive.Messages
          components={{ UserMessage, AssistantMessage }}
        />
        <ThreadPrimitive.ViewportFooter className="composer-footer">
          <LastTurnBanner />
          <RunNotice />
          <Composer />
        </ThreadPrimitive.ViewportFooter>
      </ThreadPrimitive.Viewport>
    </ThreadPrimitive.Root>
  );
}

// RunNotice shows run status above the composer: a transient notice (a retry
// backoff, a reconnect) when there is one, otherwise what the running turn is
// doing once it has been at it for a while — a long command or a slow model
// looks hung without it.
function RunNotice() {
  const { notice, runStatus } = usePigo();
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!runStatus) return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [runStatus]);

  let text = notice;
  if (!text && runStatus) {
    const elapsed = now - runStatus.since;
    if (elapsed >= 5000) {
      const doing = runStatus.phase === "tool" ? `正在执行 ${runStatus.tool ?? "工具"}` : "等待模型响应";
      text = `${doing}（已 ${formatElapsed(elapsed)}）${runStatus.steps > 0 ? ` · 已完成 ${runStatus.steps} 步` : ""}`;
    }
  }
  if (!text) return null;
  return (
    <div className="run-notice" role="status">
      <span className="run-notice-dot" aria-hidden="true" />
      {text}
    </div>
  );
}

function formatElapsed(ms: number): string {
  const seconds = Math.floor(ms / 1000);
  if (seconds < 60) return `${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分 ${seconds % 60} 秒`;
  return `${Math.floor(minutes / 60)} 小时 ${minutes % 60} 分`;
}

// sendContinue asks the agent to pick up where a cut-short turn stopped.
function useSendContinue() {
  const aui = useAui();
  return () => {
    aui.composer.setText("继续");
    aui.composer.send();
  };
}

// LastTurnBanner explains how the session's previous turn ended when the page
// did not see it end: a turn stopped by a limit or a restart while the page
// was closed.
function LastTurnBanner() {
  const { lastTurn, dismissLastTurn } = usePigo();
  const sendContinue = useSendContinue();
  if (!lastTurn) return null;
  const canContinue = continuableReasons.has(lastTurn.reason ?? "");
  return (
    <div className="run-notice last-turn-banner" role="status">
      <span>{lastTurn.message || "上一轮没有正常结束。"}</span>
      {canContinue && (
        <button
          type="button"
          className="ghost-button"
          onClick={() => {
            dismissLastTurn();
            sendContinue();
          }}
        >
          继续
        </button>
      )}
      <button type="button" className="ghost-button" aria-label="关闭提示" onClick={dismissLastTurn}>×</button>
    </div>
  );
}

function UserMessage() {
  return (
    <MessagePrimitive.Root className="message-row user-row">
      <div className="message user-message">
        <MessagePrimitive.Content />
      </div>
    </MessagePrimitive.Root>
  );
}

function AssistantMessage() {
  const { error } = usePigo();
  const custom = useAuiState((state) => state.message.metadata?.custom as { usage?: TurnUsage; end?: TurnEnd } | undefined);
  const isLast = useAuiState((state) => state.message.isLast);
  const running = useAuiState((state) => state.thread.isRunning);
  const sendContinue = useSendContinue();
  const usage = custom?.usage;
  const end = custom?.end;
  // How the turn ended, when it did not simply finish: a failure shows as the
  // message's error; a step-limit stop finished normally and says so here.
  const note = end && end.reason === "step_limit" ? end.message : "";
  return (
    <MessagePrimitive.Root className="message-row assistant-row">
      <div className="assistant-avatar" aria-hidden="true">
        π
      </div>
      <div className="assistant-body">
        <div className="message assistant-message">
          <MessagePrimitive.Content components={{ Text: MarkdownText }} />
          <MessagePrimitive.Error>
            <span className="message-error">{end?.message || error || "生成失败，请重试。"}</span>
          </MessagePrimitive.Error>
          {note && <p className="turn-note">{note}</p>}
        </div>
        {usage && <UsageLine usage={usage} />}
        {end && isLast && !running && continuableReasons.has(end.reason) && (
          <button type="button" className="ghost-button continue-button" onClick={sendContinue}>
            继续
          </button>
        )}
        <ActionBarPrimitive.Root className="message-actions" hideWhenRunning>
          <ActionBarPrimitive.Copy className="message-action">
            复制
          </ActionBarPrimitive.Copy>
        </ActionBarPrimitive.Root>
      </div>
    </MessagePrimitive.Root>
  );
}

function Composer() {
  const { api, commands } = usePigo();
  const [stopping, setStopping] = useState(false);
  // Stop asks the server to end the turn; the turn's own final event then
  // ends the run here. Merely closing the stream would leave the turn running.
  async function stop() {
    setStopping(true);
    try {
      await api.cancelTurn();
    } catch {
      // The turn may have just ended on its own.
    } finally {
      setStopping(false);
    }
  }
  const aui = useAui();
  const slashCommands = useMemo<Unstable_SlashCommand[]>(
    () =>
      commands.map((command) => ({
        id: command.name,
        label: `/${command.name}${command.argumentHint ? ` ${command.argumentHint}` : ""}`,
        description: `${command.description}${command.available ? "" : "（Web 暂不可用）"}`,
        execute: () => {
          // TriggerPopover removes the typed query after execute; defer our
          // replacement so the selected command remains ready for arguments.
          queueMicrotask(() => aui.composer.setText(`/${command.name} `));
        },
      })),
    [aui, commands],
  );
  const slash = unstable_useSlashCommandAdapter({
    commands: slashCommands,
    removeOnExecute: true,
  });

  return (
    <ComposerPrimitive.Unstable_TriggerPopoverRoot>
      <ComposerPrimitive.Root className="composer">
        <ComposerPrimitive.Unstable_TriggerPopover
          char="/"
          adapter={slash.adapter}
          className="command-menu"
        >
          <ComposerPrimitive.Unstable_TriggerPopover.Action {...slash.action} />
          <ComposerPrimitive.Unstable_TriggerPopoverItems>
            {(items) => (
              <>
                <div className="command-menu-title">pigo commands</div>
                {items.map((item, index) => (
                  <ComposerPrimitive.Unstable_TriggerPopoverItem
                    key={item.id}
                    item={item}
                    index={index}
                    className="command-item"
                  >
                    <strong>{item.label}</strong>
                    {item.description && <span>{item.description}</span>}
                  </ComposerPrimitive.Unstable_TriggerPopoverItem>
                ))}
              </>
            )}
          </ComposerPrimitive.Unstable_TriggerPopoverItems>
        </ComposerPrimitive.Unstable_TriggerPopover>
        <ComposerPrimitive.Input
          className="composer-input"
          placeholder="输入消息，键入 / 查看命令…"
          rows={1}
          autoFocus
        />
        <div className="composer-controls">
          <span>Enter 发送 · Shift+Enter 换行</span>
          <ThreadPrimitive.If running>
            <button type="button" className="send-button stop-button" aria-label="停止" disabled={stopping} onClick={() => void stop()}>
              ■
            </button>
          </ThreadPrimitive.If>
          <ThreadPrimitive.If running={false}>
            <ComposerPrimitive.Send className="send-button" aria-label="发送">
              ↑
            </ComposerPrimitive.Send>
          </ThreadPrimitive.If>
        </div>
      </ComposerPrimitive.Root>
    </ComposerPrimitive.Unstable_TriggerPopoverRoot>
  );
}
