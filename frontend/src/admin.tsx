// The two administrator panels that are genuinely about the deployment rather
// than about a provider or a model: what new sessions default to, and who has
// an account. Everything else an administrator configures now lives on the
// Provider and 模型 tables next to its personal counterpart.
//
// They take `api` as a prop rather than reaching for the context in App.tsx, so
// this module has no import cycle with it.
//
// They render only for administrators, but that is presentation only — every
// route they call is enforced by requireAdmin on the server.
import { useCallback, useEffect, useMemo, useState } from "react";
import type { AdminUser, CustomModelInfo, ModelInfo, ModelParamList, PigoAPI, SandboxEnvList, ServerSettings, SubagentSettings, SubagentView } from "./api";
import { PanelHeading, errorText, usePanelMessage } from "./panel";

// --- administrator: deployment settings -------------------------------------

const thinkingLevels = [
  { value: "off", label: "off · 不推理" },
  { value: "minimal", label: "minimal" },
  { value: "low", label: "low" },
  { value: "medium", label: "medium · 推荐" },
  { value: "high", label: "high" },
  { value: "xhigh", label: "xhigh" },
  { value: "max", label: "max" },
];

// defaultChoices lists the models every user can resolve: the public catalog,
// built-in models of providers with a key, and OpenRouter's free ones. A
// personal model is left out — it resolves only for the user who added it.
function defaultChoices(models: ModelInfo[], shared: CustomModelInfo[]) {
  const groups: { title: string; items: { id: string; label: string }[] }[] = [
    {
      title: "公共模型",
      items: shared.filter((item) => !item.expired).map((item) => ({ id: item.id, label: `${item.label} · ${item.provider}` })),
    },
    {
      title: "已配置 Key 的内置模型",
      items: models.filter((item) => item.source === "preset").map((item) => ({ id: item.id, label: `${item.label} · ${item.provider}` })),
    },
    {
      title: "OpenRouter 免费",
      items: models.filter((item) => item.source === "free").map((item) => ({ id: item.id, label: item.label })),
    },
  ];
  return groups.filter((group) => group.items.length > 0);
}

export function AdminSettings({ api, models }: { api: PigoAPI; models: ModelInfo[] }) {
  const [saved, setSaved] = useState<ServerSettings | null>(null);
  const [settings, setSettings] = useState<ServerSettings | null>(null);
  const [shared, setShared] = useState<CustomModelInfo[]>([]);
  const [saving, setSaving] = useState(false);
  const { report, view } = usePanelMessage();

  useEffect(() => {
    void api
      .adminSettings()
      .then((next) => {
        setSaved(next);
        setSettings(next);
      })
      .catch((cause) => report(errorText(cause), true));
    void api.adminModels().then(setShared).catch(() => setShared([]));
  }, [api, report]);

  async function save() {
    if (!settings) return;
    setSaving(true);
    try {
      const next = await api.updateAdminSettings(settings);
      setSaved(next);
      setSettings(next);
      report("公共配置已保存，新会话立即采用", false);
    } catch (cause) {
      report(errorText(cause), true);
    } finally {
      setSaving(false);
    }
  }

  const groups = defaultChoices(models, shared);
  const known = groups.some((group) => group.items.some((item) => item.id === settings?.defaultModel));
  const dirty = !!settings && !!saved && JSON.stringify(settings) !== JSON.stringify(saved);

  return (
    <section className="settings-section">
      <PanelHeading title="公共配置" hint="对所有用户生效；新会话立即采用，进行中的会话不受影响。" icon="部" />
      {settings && (
        <>
          <div className="admin-card">
            <div className="admin-card-head">
              <strong>新会话默认</strong>
              <span>用户没选模型时，新建的会话用这里的设置；之后可在会话里切换。</span>
            </div>
            <div className="add-model-grid admin-defaults-grid">
              <label>
                默认模型
                <select
                  className="settings-input settings-select"
                  value={settings.defaultModel}
                  onChange={(event) => setSettings({ ...settings, defaultModel: event.target.value })}
                >
                  {groups.map((group) => (
                    <optgroup key={group.title} label={group.title}>
                      {group.items.map((item) => (
                        <option key={item.id} value={item.id}>
                          {item.label}
                        </option>
                      ))}
                    </optgroup>
                  ))}
                  {!known && <option value={settings.defaultModel}>{`${settings.defaultModel} · 当前（不在可选列表中）`}</option>}
                </select>
              </label>
              <label>
                默认推理强度
                <select
                  className="settings-input settings-select"
                  value={settings.defaultThinking}
                  onChange={(event) => setSettings({ ...settings, defaultThinking: event.target.value })}
                >
                  {thinkingLevels.map((level) => (
                    <option key={level.value} value={level.value}>
                      {level.label}
                    </option>
                  ))}
                </select>
              </label>
            </div>
            {!known && (
              <p className="security-note">
                当前默认模型 {settings.defaultModel} 不在公共模型或已配置 Key 的模型里，其他用户新建会话可能无法使用，建议换一个。
              </p>
            )}
            <p className="credential-note">只列出所有人都能用的模型；个人添加的模型只对本人可见，不能做默认。</p>
          </div>

          <div className="admin-card">
            <div className="admin-card-head">
              <strong>账号与 Key</strong>
            </div>
            <label className="admin-switch">
              <input
                type="checkbox"
                checked={settings.allowRegistration}
                onChange={(event) => setSettings({ ...settings, allowRegistration: event.target.checked })}
              />
              <span>
                <strong>开放注册</strong>
                <small>关闭后只有已有账号能登录，新用户需管理员创建。</small>
              </span>
            </label>
            <label className="admin-switch">
              <input
                type="checkbox"
                checked={settings.allowUserKeys}
                onChange={(event) => setSettings({ ...settings, allowUserKeys: event.target.checked })}
              />
              <span>
                <strong>允许用户自带 API Key</strong>
                <small>关闭后用户已存的 Key 停用但不删除，所有调用走公共 Key。</small>
              </span>
            </label>
          </div>

          <Subagent api={api} report={report} models={models} shared={shared} />

          <ModelParams api={api} report={report} models={models} shared={shared} />

          <SandboxEnv api={api} report={report} />

          <div className="admin-save-bar">
            <span>{dirty ? "有未保存的修改" : "已是最新"}</span>
            <div className="context-editor-actions">
              {dirty && (
                <button type="button" className="ghost-button" onClick={() => setSettings(saved)}>
                  还原
                </button>
              )}
              <button type="button" className="primary-button" disabled={!dirty || saving} onClick={() => void save()}>
                {saving ? "保存中…" : "保存"}
              </button>
            </div>
          </div>
        </>
      )}
      {view}
    </section>
  );
}

// Subagent is what the task tool may do: whether the model has it at all, how
// many sub-agents run at once, how far each may go, and which models a call
// may ask for. Like the sandbox variables it saves per change, not with the
// rest of the form.
function Subagent({
  api,
  report,
  models,
  shared,
}: {
  api: PigoAPI;
  report(text: string, bad: boolean): void;
  models: ModelInfo[];
  shared: CustomModelInfo[];
}) {
  const [view, setView] = useState<SubagentView | null>(null);
  const [picking, setPicking] = useState(false);

  useEffect(() => {
    void api
      .subagent()
      .then(setView)
      .catch(() => undefined);
  }, [api]);

  async function save(next: SubagentSettings) {
    try {
      setView(await api.putSubagent(next));
      report("子代理设置已保存", false);
    } catch (cause) {
      report(errorText(cause), true);
    }
  }

  if (!view) return null;
  const s = view.settings;
  const chosen = s.models ?? [];
  const groups = defaultChoices(models, shared);
  // A number field: empty means "the default", which the placeholder shows.
  const number = (value: number | undefined, fallback: number, apply: (n: number | undefined) => SubagentSettings) => (
    <input
      className="settings-input"
      type="number"
      min={1}
      placeholder={String(fallback)}
      value={value ?? ""}
      onChange={(event) => {
        const raw = event.target.value.trim();
        setView({ ...view, settings: apply(raw === "" ? undefined : Number(raw)) });
      }}
      onBlur={() => void save(view.settings)}
    />
  );

  return (
    <div className="admin-card">
      <div className="admin-card-head">
        <strong>子代理（task 工具）</strong>
        <span>模型可以把一件独立的活交给子代理：它有自己的上下文和工具，跑完返回一份报告，费用记在同一轮。场景会话不提供这个工具。</span>
      </div>
      <label className="admin-switch">
        <input type="checkbox" checked={!s.disabled} onChange={(event) => void save({ ...s, disabled: !event.target.checked })} />
        <span>
          <strong>启用 task 工具</strong>
          <small>关闭后所有会话都看不到它；已在运行的一轮不受影响。</small>
        </span>
      </label>
      {!s.disabled && (
        <>
          <div className="add-model-grid subagent-grid">
            <label>
              并发上限
              {number(s.maxConcurrent, view.defaults.maxConcurrent, (n) => ({ ...s, maxConcurrent: n }))}
              <small>一个会话同时跑几个子代理</small>
            </label>
            <label>
              单个最大步数
              {number(s.maxSteps, view.defaults.maxSteps, (n) => ({ ...s, maxSteps: n }))}
              <small>超过后让它收尾给结论</small>
            </label>
            <label>
              单个超时（秒）
              {number(s.timeoutSeconds, view.defaults.timeoutSeconds, (n) => ({ ...s, timeoutSeconds: n }))}
              <small>超时返回已有进展</small>
            </label>
          </div>
          <div className="subagent-models">
            <div className="subagent-models-head">
              <span>可选模型 {chosen.length > 0 ? `· ${chosen.length} 个` : "· 未配置"}</span>
              <button type="button" className="ghost-button" onClick={() => setPicking(!picking)}>
                {picking ? "收起" : "选择"}
              </button>
            </div>
            <p className="credential-note">
              勾选后，调用可以指定其中一个模型（例如把调研交给便宜的模型）。不勾选任何模型时，子代理只能用当前会话的模型。
            </p>
            {picking &&
              groups.map((group) => (
                <div key={group.title} className="scene-tool-group">
                  <span className="eyebrow">{group.title}</span>
                  <div className="scene-tool-chips">
                    {group.items.map((item) => {
                      const on = chosen.includes(item.id);
                      return (
                        <label key={item.id} className={on ? "scene-tool-chip on" : "scene-tool-chip"}>
                          <input
                            type="checkbox"
                            checked={on}
                            onChange={() => void save({ ...s, models: on ? chosen.filter((m) => m !== item.id) : [...chosen, item.id] })}
                          />
                          {item.label}
                        </label>
                      );
                    })}
                  </div>
                </div>
              ))}
            {!picking && chosen.length > 0 && (
              <div className="scene-tool-chips">
                {chosen.map((id) => (
                  <label key={id} className="scene-tool-chip on">
                    <input type="checkbox" checked readOnly onClick={() => void save({ ...s, models: chosen.filter((m) => m !== id) })} />
                    {id}
                  </label>
                ))}
              </div>
            )}
          </div>
        </>
      )}
    </div>
  );
}

// SandboxEnv edits the variables injected into every session container. The
// container is started with an empty environment, so a tool that needs a proxy
// (or a gateway address) gets it here. It saves on its own, not with the rest
// of the form: each row is its own write.
// ModelParams is the per-model table: how big the model's context is, at what
// share of it a session compacts, and when its training data ends. The last one
// goes into the system prompt so the model treats an unfamiliar name as its own
// staleness rather than as proof the thing does not exist (spec/prompt-tuning.md).
//
// Rows are the models this deployment actually offers, plus any model already
// configured. OpenRouter's free catalog is left out on purpose: it is hundreds
// of models and their windows come from the catalog itself.
function ModelParams({
  api,
  report,
  models,
  shared,
}: {
  api: PigoAPI;
  report(text: string, bad: boolean): void;
  models: ModelInfo[];
  shared: CustomModelInfo[];
}) {
  const [list, setList] = useState<ModelParamList | null>(null);
  const [draft, setDraft] = useState<Record<string, { window: string; pct: string; cutoff: string }>>({});
  const [busy, setBusy] = useState("");

  useEffect(() => {
    void api
      .modelParams()
      .then(setList)
      .catch(() => undefined);
  }, [api]);

  const rows = useMemo(() => {
    const out = new Map<string, { provider: string; model: string; label: string }>();
    const add = (provider: string, model: string, label: string) => {
      const key = `${provider.toLowerCase()}::${model}`;
      if (!out.has(key)) out.set(key, { provider, model, label });
    };
    for (const item of shared) if (!item.expired) add(item.provider, item.id, item.label);
    for (const item of models) if (item.source === "preset") add(item.provider, item.id, item.label);
    for (const row of list?.params ?? []) add(row.provider, row.model, row.model);
    return [...out.entries()].map(([key, row]) => ({ key, ...row }));
  }, [models, shared, list]);

  function stored(key: string) {
    const row = (list?.params ?? []).find((item) => `${item.provider.toLowerCase()}::${item.model}` === key);
    return {
      window: row?.contextWindow ? String(row.contextWindow) : "",
      pct: row?.compactPct ? String(row.compactPct) : "",
      cutoff: row?.knowledgeCutoff ?? "",
    };
  }

  function current(key: string) {
    return draft[key] ?? stored(key);
  }

  function edit(key: string, field: "window" | "pct" | "cutoff", value: string) {
    setDraft({ ...draft, [key]: { ...current(key), [field]: value } });
  }

  async function save(row: { key: string; provider: string; model: string }) {
    const next = current(row.key);
    const window = next.window.trim();
    const pct = next.pct.trim();
    // Coercing a typo to NaN would drop the field, and a PUT replaces the whole
    // row — the previous value would vanish under a "saved" message.
    if ([window, pct].some((value) => value !== "" && !/^\d+$/.test(value))) {
      report("上下文窗口和压缩阈值只能填数字", true);
      return;
    }
    const empty = !window && !pct && !next.cutoff.trim();
    setBusy(row.key);
    try {
      setList(
        empty
          ? await api.deleteModelParam(row.provider, row.model)
          : await api.putModelParam({
              provider: row.provider,
              model: row.model,
              contextWindow: Number(window) || undefined,
              compactPct: Number(pct) || undefined,
              knowledgeCutoff: next.cutoff.trim() || undefined,
            }),
      );
      setDraft((all) => {
        const rest = { ...all };
        delete rest[row.key];
        return rest;
      });
      report(empty ? `已恢复 ${row.model} 的默认参数` : `已保存 ${row.model}`, false);
    } catch (cause) {
      report(errorText(cause), true);
    } finally {
      setBusy("");
    }
  }

  return (
    <div className="admin-card">
      <div className="admin-card-head">
        <strong>模型参数</strong>
        <span>按模型覆盖上下文窗口、压缩阈值和训练截止；留空就用部署默认。改动对新会话生效，进行中的会话切换模型时也会重新生效。</span>
      </div>
      {rows.length === 0 && <p className="credential-note">还没有公共模型；先在模型页添加，或给内置服务商配置 Key。</p>}
      {rows.length > 0 && (
        <ul className="param-list">
          <li className="param-head">
            <span>模型</span>
            <span>上下文窗口</span>
            <span>压缩阈值 %</span>
            <span>训练截止</span>
            <span />
          </li>
          {rows.map((row) => {
            const next = current(row.key);
            const was = stored(row.key);
            const dirty = next.window !== was.window || next.pct !== was.pct || next.cutoff !== was.cutoff;
            return (
              <li key={row.key}>
                <span className="param-model" title={`${row.model} · ${row.provider}`}>
                  <code>{row.model}</code>
                  <small>{row.provider}</small>
                </span>
                <input
                  className="settings-input"
                  inputMode="numeric"
                  placeholder={list ? String(list.defaultWindow) : ""}
                  value={next.window}
                  onChange={(event) => edit(row.key, "window", event.target.value)}
                />
                <input
                  className="settings-input"
                  inputMode="numeric"
                  placeholder={list ? String(list.defaultCompactPct) : ""}
                  value={next.pct}
                  onChange={(event) => edit(row.key, "pct", event.target.value)}
                />
                <input
                  className="settings-input"
                  placeholder="2026-05"
                  value={next.cutoff}
                  onChange={(event) => edit(row.key, "cutoff", event.target.value)}
                />
                <button type="button" className="ghost-button" disabled={!dirty || busy === row.key} onClick={() => void save(row)}>
                  {busy === row.key ? "…" : "保存"}
                </button>
              </li>
            );
          })}
        </ul>
      )}
      <p className="credential-note">
        训练截止写成 <code>2026-05</code>，会写进系统提示词：告诉模型它的知识到此为止，遇到不认识的名字应当去查，而不是断定它不存在。不填也会提示「训练数据早于今天」，只是没有具体日期。
      </p>
    </div>
  );
}

function SandboxEnv({ api, report }: { api: PigoAPI; report(text: string, bad: boolean): void }) {
  const [list, setList] = useState<SandboxEnvList>({ vars: [], runningOld: 0 });
  const [name, setName] = useState("");
  const [value, setValue] = useState("");
  const [editing, setEditing] = useState("");

  useEffect(() => {
    void api
      .sandboxEnv()
      .then(setList)
      .catch(() => undefined);
  }, [api]);

  async function act(run: () => Promise<SandboxEnvList>, text: string) {
    try {
      setList(await run());
      report(text, false);
      return true;
    } catch (cause) {
      report(errorText(cause), true);
      return false;
    }
  }

  async function save(nextName: string, nextValue: string) {
    if (!nextName.trim() || !nextValue.trim()) return;
    if (await act(() => api.putSandboxEnv(nextName.trim(), nextValue.trim()), `已保存 ${nextName.trim()}`)) {
      setName("");
      setValue("");
      setEditing("");
    }
  }

  const vars = list.vars ?? [];
  // The server's own proxy settings, offered as one click each.
  const hints = (list.processHint ?? []).filter((pair) => !vars.some((item) => item.name === pair.slice(0, pair.indexOf("="))));

  return (
    <div className="admin-card">
      <div className="admin-card-head">
        <strong>沙箱环境变量</strong>
        <span>注入到每个会话的沙箱容器。容器默认没有任何环境变量，需要走代理或访问内网服务的工具在这里配置。</span>
      </div>
      {vars.length > 0 && (
        <ul className="env-list">
          {vars.map((item) => (
            <li key={item.name}>
              <code>{item.name}</code>
              {editing === item.name ? (
                <input
                  className="settings-input"
                  autoFocus
                  defaultValue={item.value}
                  onKeyDown={(event) => {
                    if (event.key === "Enter") void save(item.name, (event.target as HTMLInputElement).value);
                    if (event.key === "Escape") setEditing("");
                  }}
                  onBlur={(event) => void save(item.name, event.target.value)}
                />
              ) : (
                <span className="env-value" onClick={() => setEditing(item.name)} title="点击修改">
                  {item.value}
                </span>
              )}
              <button type="button" className="ghost-button danger-button" onClick={() => void act(() => api.deleteSandboxEnv(item.name), `已删除 ${item.name}`)}>
                删除
              </button>
            </li>
          ))}
        </ul>
      )}
      <div className="env-add">
        <input className="settings-input" placeholder="HTTPS_PROXY" value={name} onChange={(event) => setName(event.target.value)} />
        <input className="settings-input" placeholder="http://192.168.1.2:8080" value={value} onChange={(event) => setValue(event.target.value)} />
        <button type="button" className="ghost-button" disabled={!name.trim() || !value.trim()} onClick={() => void save(name, value)}>
          添加
        </button>
      </div>
      {hints.length > 0 && (
        <div className="env-hints">
          <span>服务器自己的设置：</span>
          {hints.map((pair) => {
            const at = pair.indexOf("=");
            return (
              <button key={pair} type="button" className="ghost-button" onClick={() => void save(pair.slice(0, at), pair.slice(at + 1))}>
                用 {pair}
              </button>
            );
          })}
        </div>
      )}
      <p className="credential-note">
        任何用户都能在自己的会话里用 <code>env</code> 读到这些值，不要放 API Key；模型服务商的 Key 由服务端直接使用，不会进入沙箱。
      </p>
      {list.runningOld > 0 && <p className="credential-note">当前有 {list.runningOld} 个沙箱容器在运行，它们保留启动时的环境变量；新会话立即生效，旧会话闲置回收或重启服务后生效。</p>}
    </div>
  );
}

// --- administrator: accounts ------------------------------------------------

export function AdminUsers({ api }: { api: PigoAPI }) {
  const [users, setUsers] = useState<AdminUser[]>([]);
  // pendingDelete holds the id of the account whose confirmation box is open;
  // confirmName is what the administrator has typed into it.
  const [pendingDelete, setPendingDelete] = useState("");
  const [confirmName, setConfirmName] = useState("");
  // temporaryPassword is shown once, right after a reset. Nothing can retrieve
  // it afterwards, so it stays on screen until dismissed.
  const [temporaryPassword, setTemporaryPassword] = useState<{ username: string; password: string } | null>(null);
  const { report, view } = usePanelMessage();

  const refresh = useCallback(async () => setUsers(await api.adminUsers()), [api]);

  useEffect(() => {
    void refresh().catch((cause) => report(errorText(cause), true));
  }, [refresh, report]);

  async function act(run: () => Promise<string>) {
    try {
      const text = await run();
      await refresh();
      report(text, false);
    } catch (cause) {
      report(errorText(cause), true);
    }
  }

  return (
    <section className="settings-section">
      <PanelHeading title="用户" hint="停用会保留数据；删除不可撤销。管理员身份来自服务端的环境变量。" icon="U" />
      {temporaryPassword && (
        <div className="temp-password">
          <span>{temporaryPassword.username} 的临时密码（仅显示一次）</span>
          <code>{temporaryPassword.password}</code>
          <button
            type="button"
            className="ghost-button"
            onClick={() => void navigator.clipboard.writeText(temporaryPassword.password).catch(() => undefined)}
          >
            复制
          </button>
          <button type="button" className="ghost-button" onClick={() => setTemporaryPassword(null)}>
            知道了
          </button>
        </div>
      )}
      <ul className="admin-user-list">
        {users.map((user) => (
          <li key={user.id}>
            <div className="admin-user-row">
              <span className="admin-user-name">
                {user.username}
                {user.admin && <em className="admin-badge">管理员</em>}
                {user.disabled && <em className="admin-badge disabled-badge">已停用</em>}
              </span>
              <span className="admin-user-meta">
                {user.sessions} 个会话
                {user.credentials?.length ? ` · 自带 Key：${user.credentials.join(", ")}` : ""}
              </span>
            </div>
            <div className="admin-user-actions">
              <button
                type="button"
                className="ghost-button"
                onClick={() =>
                  void act(async () => {
                    await api.setUserDisabled(user.id, !user.disabled);
                    return user.disabled ? `已恢复 ${user.username}` : `已停用 ${user.username}`;
                  })
                }
              >
                {user.disabled ? "恢复" : "停用"}
              </button>
              <button
                type="button"
                className="ghost-button"
                onClick={() =>
                  void act(async () => {
                    setTemporaryPassword(await api.resetUserPassword(user.id));
                    return `已重置 ${user.username} 的密码`;
                  })
                }
              >
                重置密码
              </button>
              <button
                type="button"
                className="ghost-button danger-button"
                onClick={() => {
                  setPendingDelete(pendingDelete === user.id ? "" : user.id);
                  setConfirmName("");
                }}
              >
                删除
              </button>
            </div>
            {pendingDelete === user.id && (
              <div className="admin-confirm">
                <p>
                  将永久删除 <strong>{user.username}</strong> 的账号、全部会话与工作区、已存的 API Key 和自定义模型。此操作不可撤销。输入用户名确认：
                </p>
                <input
                  className="settings-input"
                  value={confirmName}
                  placeholder={user.username}
                  onChange={(event) => setConfirmName(event.target.value)}
                />
                <button
                  type="button"
                  className="primary-button full-button danger-button"
                  disabled={confirmName !== user.username}
                  onClick={() =>
                    void act(async () => {
                      const result = await api.deleteUser(user.id);
                      setPendingDelete("");
                      setConfirmName("");
                      if (result.problems?.length) {
                        // A partial delete is reported rather than hidden: the
                        // leftovers need a human.
                        return `已删除 ${result.username}，但有残留：${result.problems.join("；")}`;
                      }
                      return `已删除 ${result.username}（含 ${result.sessions} 个会话）`;
                    })
                  }
                >
                  确认删除
                </button>
              </div>
            )}
          </li>
        ))}
      </ul>
      {view}
    </section>
  );
}
