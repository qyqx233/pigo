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
import { useCallback, useEffect, useState } from "react";
import type { AdminUser, CustomModelInfo, ModelInfo, PigoAPI, ServerSettings } from "./api";
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
