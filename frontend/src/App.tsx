import {
  ActionBarPrimitive,
  AssistantRuntimeProvider,
  ComposerPrimitive,
  MessagePrimitive,
  ThreadPrimitive,
  unstable_useSlashCommandAdapter,
  useAui,
  useLocalRuntime,
  type ChatModelAdapter,
  type ThreadMessage,
  type Unstable_SlashCommand,
} from "@assistant-ui/react";
import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  PigoAPI,
  type ModelInfo,
  type SessionInfo,
  type SessionSettings,
  type SlashCommandInfo,
  type WorkspaceEntry,
} from "./api";

type PigoContextValue = {
  api: PigoAPI;
  session: SessionInfo | null;
  commands: SlashCommandInfo[];
  models: ModelInfo[];
  error: string;
  theme: Theme;
  setTheme(theme: Theme): void;
  commandBrowserOpen: boolean;
  setCommandBrowserOpen(open: boolean): void;
  connect(token: string): Promise<void>;
  saveSettings(settings: SessionSettings): Promise<void>;
  newSession(): Promise<void>;
};

type Theme = "dark" | "light";

const PigoContext = createContext<PigoContextValue | null>(null);

function textOf(message: ThreadMessage): string {
  return message.content
    .filter((part) => part.type === "text")
    .map((part) => part.text)
    .join("\n");
}

function RuntimeProvider({ children }: { children: ReactNode }) {
  const apiRef = useRef<PigoAPI | null>(null);
  if (!apiRef.current) apiRef.current = new PigoAPI();
  const api = apiRef.current;
  const [session, setSession] = useState<SessionInfo | null>(null);
  const [commands, setCommands] = useState<SlashCommandInfo[]>([]);
  const [models, setModels] = useState<ModelInfo[]>([]);
  const [error, setError] = useState("");
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

  const adapter = useMemo<ChatModelAdapter>(
    () => ({
      async *run({ messages, abortSignal }) {
        setError("");
        try {
          const prompt = textOf(messages.at(-1)!);
          if (prompt.trim() === "/help") {
            setCommandBrowserOpen(true);
            yield {
              content: [{ type: "text", text: "已打开命令浏览器，可搜索或选择命令。" }],
            };
            return;
          }
          let emitted = false;
          for await (const text of api.stream(prompt, abortSignal)) {
            emitted = true;
            yield { content: [{ type: "text", text }] };
          }
          if (!emitted) yield { content: [{ type: "text", text: "" }] };
        } catch (cause) {
          setError(cause instanceof Error ? cause.message : String(cause));
          throw cause;
        }
        void Promise.all([api.sessionInfo(), api.commands()])
          .then(([nextSession, nextCommands]) => {
            setSession(nextSession);
            setCommands(nextCommands);
          })
          .catch(() => undefined);
      },
    }),
    [api],
  );
  const runtime = useLocalRuntime(adapter);

  async function loadSession() {
    setError("");
    try {
      const [next, nextCommands, nextModels] = await Promise.all([
        api.ensureSession(),
        api.commands(),
        api.models(),
      ]);
      setSession(next);
      setCommands(nextCommands);
      setModels(nextModels);
      return true;
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
      return false;
    }
  }

  useEffect(() => {
    void loadSession();
  }, []);

  async function connect(token: string) {
    api.setToken(token);
    if (!(await loadSession())) throw new Error("连接失败");
  }

  async function saveSettings(settings: SessionSettings) {
    setError("");
    try {
      setSession(await api.updateSession(settings));
    } catch (cause) {
      const message = cause instanceof Error ? cause.message : String(cause);
      setError(message);
      throw cause;
    }
  }

  async function newSession() {
    runtime.thread.reset();
    setCommands([]);
    setSession(null);
    setError("");
    try {
      await api.reset();
    } catch {
      // The local reset is still useful if the old server session expired.
    }
    await loadSession();
  }

  return (
    <PigoContext.Provider
      value={{
        api,
        session,
        commands,
        models,
        error,
        theme,
        setTheme,
        commandBrowserOpen,
        setCommandBrowserOpen,
        connect,
        saveSettings,
        newSession,
      }}
    >
      <AssistantRuntimeProvider runtime={runtime}>
        {children}
      </AssistantRuntimeProvider>
    </PigoContext.Provider>
  );
}

function usePigo() {
  const value = useContext(PigoContext);
  if (!value) throw new Error("PigoContext is missing");
  return value;
}

export function App() {
  return (
    <RuntimeProvider>
      <Shell />
    </RuntimeProvider>
  );
}

function Shell() {
  const {
    session,
    error,
    theme,
    setTheme,
    commandBrowserOpen,
    setCommandBrowserOpen,
    newSession,
  } = usePigo();
  const [settingsOpen, setSettingsOpen] = useState(false);

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
        {session && <span className="model-pill">{session.model}</span>}
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
          className="icon-button"
          type="button"
          aria-label="打开设置"
          title="设置"
          onClick={() => setSettingsOpen(true)}
        >
          <SettingsIcon />
        </button>
        <button className="secondary-button" type="button" onClick={() => void newSession()}>
          新会话
        </button>
      </header>
      <Thread />
      <SettingsDrawer open={settingsOpen} onClose={() => setSettingsOpen(false)} />
      <CommandBrowser
        open={commandBrowserOpen}
        onClose={() => setCommandBrowserOpen(false)}
      />
    </main>
  );
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

function SettingsDrawer({ open, onClose }: { open: boolean; onClose(): void }) {
  const { session, commands, models, error, connect, saveSettings } = usePigo();
  const [model, setModel] = useState("");
  const [thinking, setThinking] = useState("medium");
  const [token, setToken] = useState("");
  const [saving, setSaving] = useState(false);
  const [notice, setNotice] = useState("");

  useEffect(() => {
    if (!open) return;
    setModel(session?.model ?? "");
    setThinking(session?.thinking ?? "medium");
    setNotice("");
  }, [open, session?.model, session?.thinking]);

  useEffect(() => {
    if (!open) return;
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    window.addEventListener("keydown", closeOnEscape);
    return () => window.removeEventListener("keydown", closeOnEscape);
  }, [open, onClose]);

  if (!open) return null;
  const skillCount = commands.filter((command) => command.source === "skill").length;
  const unavailableCount = commands.filter((command) => !command.available).length;

  async function applySessionSettings() {
    if (!model.trim()) return;
    setSaving(true);
    setNotice("");
    try {
      await saveSettings({ model: model.trim(), thinking });
      setNotice("会话设置已更新，将从下一轮开始生效。");
    } catch {
      setNotice("");
    } finally {
      setSaving(false);
    }
  }

  async function applyToken() {
    setSaving(true);
    setNotice("");
    try {
      await connect(token);
      setNotice(
        token.trim()
          ? "连接凭据已保存到当前浏览器。"
          : "已清除浏览器中保存的连接凭据。",
      );
    } catch {
      setNotice("");
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="settings-layer">
      <button className="settings-backdrop" type="button" aria-label="关闭设置" onClick={onClose} />
      <aside className="settings-drawer" role="dialog" aria-modal="true" aria-label="设置">
        <header className="settings-header">
          <div>
            <span className="eyebrow">PIGO WEB</span>
            <h2>设置</h2>
          </div>
          <button className="icon-button close-button" type="button" onClick={onClose} aria-label="关闭设置">
            ×
          </button>
        </header>

        <div className="settings-content">
          <section className="settings-section">
            <div className="section-heading">
              <div>
                <h3>模型与推理</h3>
                <p>应用后保留当前对话上下文。</p>
              </div>
              <span className="section-icon">AI</span>
            </div>
            <label className="field-label" htmlFor="model-setting">模型</label>
            <input
              id="model-setting"
              className="settings-input"
              value={model}
              onChange={(event) => setModel(event.target.value)}
              list="pigo-models"
              placeholder="输入模型 ID"
            />
            <datalist id="pigo-models">
              {models.map((item) => (
                <option key={`${item.provider}:${item.id}`} value={item.id}>{`${item.label} · ${item.provider}`}</option>
              ))}
            </datalist>
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

          <section className="settings-section">
            <div className="section-heading">
              <div>
                <h3>连接</h3>
                <p>这是 pigo-server 的访问 Token，不是模型 API Key。</p>
              </div>
              <span className={`connection-badge ${error ? "connection-badge-error" : ""}`}>
                {error ? "未连接" : "已连接"}
              </span>
            </div>
            <label className="field-label" htmlFor="token-setting">Bearer Token</label>
            <input
              id="token-setting"
              className="settings-input"
              type="password"
              value={token}
              onChange={(event) => setToken(event.target.value)}
              placeholder="输入新 Token；留空应用将清除"
              autoComplete="off"
            />
            <button className="secondary-button full-button" type="button" disabled={saving} onClick={() => void applyToken()}>
              应用连接凭据
            </button>
            <p className="credential-note">Token 会保存在当前浏览器的 localStorage 中。不要在公共或不受信任的设备上保存。</p>
          </section>

          <section className="settings-section">
            <div className="section-heading">
              <div>
                <h3>运行能力</h3>
                <p>主机权限只能由服务端启动参数控制。</p>
              </div>
            </div>
            <div className="metric-grid">
              <div className="metric"><strong>{session?.tools.length ?? 0}</strong><span>工具</span></div>
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
          <WorkspaceFiles />

          {(notice || error) && <div className={error ? "settings-message settings-message-error" : "settings-message"}>{error || notice}</div>}
        </div>
      </aside>
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
          <Composer />
        </ThreadPrimitive.ViewportFooter>
      </ThreadPrimitive.Viewport>
    </ThreadPrimitive.Root>
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
  return (
    <MessagePrimitive.Root className="message-row assistant-row">
      <div className="assistant-avatar" aria-hidden="true">
        π
      </div>
      <div className="assistant-body">
        <div className="message assistant-message">
          <MessagePrimitive.Content />
          <MessagePrimitive.Error>
            <span className="message-error">{error || "生成失败，请重试。"}</span>
          </MessagePrimitive.Error>
        </div>
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
  const { commands } = usePigo();
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
            <ComposerPrimitive.Cancel className="send-button stop-button" aria-label="停止">
              ■
            </ComposerPrimitive.Cancel>
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
