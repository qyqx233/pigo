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
import type { AdminUser, PigoAPI, ServerSettings } from "./api";
import { PanelHeading, errorText, usePanelMessage } from "./panel";

// --- administrator: deployment settings -------------------------------------

export function AdminSettings({ api }: { api: PigoAPI }) {
  const [settings, setSettings] = useState<ServerSettings | null>(null);
  const { report, view } = usePanelMessage();

  useEffect(() => {
    void api.adminSettings().then(setSettings).catch((cause) => report(errorText(cause), true));
  }, [api, report]);

  async function save() {
    if (!settings) return;
    try {
      setSettings(await api.updateAdminSettings(settings));
      report("公共配置已保存", false);
    } catch (cause) {
      report(errorText(cause), true);
    }
  }

  return (
    <section className="settings-section">
      <PanelHeading
        title="公共配置"
        hint="对所有用户生效；新会话立即采用，进行中的回答不受影响。"
        icon="ADM"
      />
      {settings && (
        <>
          <label className="field-label" htmlFor="admin-default-model">默认模型</label>
          <input
            id="admin-default-model"
            className="settings-input"
            value={settings.defaultModel}
            onChange={(event) => setSettings({ ...settings, defaultModel: event.target.value })}
          />
          <label className="field-label" htmlFor="admin-default-thinking">默认推理强度</label>
          <select
            id="admin-default-thinking"
            className="settings-input settings-select"
            value={settings.defaultThinking}
            onChange={(event) => setSettings({ ...settings, defaultThinking: event.target.value })}
          >
            {["off", "minimal", "low", "medium", "high", "xhigh", "max"].map((level) => (
              <option key={level} value={level}>{level}</option>
            ))}
          </select>
          <label className="admin-toggle">
            <input
              type="checkbox"
              checked={settings.allowRegistration}
              onChange={(event) => setSettings({ ...settings, allowRegistration: event.target.checked })}
            />
            <span>开放注册</span>
          </label>
          <label className="admin-toggle">
            <input
              type="checkbox"
              checked={settings.allowUserKeys}
              onChange={(event) => setSettings({ ...settings, allowUserKeys: event.target.checked })}
            />
            <span>允许用户自带 API Key（关闭后已存的 Key 停用但不删除）</span>
          </label>
          <button type="button" className="primary-button full-button" onClick={() => void save()}>
            保存公共配置
          </button>
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
