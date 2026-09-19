// The model panel: one table for every model the user can choose, whatever it
// came from.
//
// This replaces the earlier split between "自定义模型" and "公共模型", which
// were the same concept filed under two owners. Here the owner is a tag on the
// row, and the shadowing rule — a shared entry wins over a personal one with
// the same id — is shown on the rows it affects instead of being described in
// prose under the form.
import { useCallback, useEffect, useState } from "react";
import type { CustomModelInfo, PigoAPI, ProviderInfo } from "./api";
import { PanelHeading, errorText, usePanelMessage } from "./panel";

// today is the default expiry floor for a personal entry.
function inDays(days: number): string {
  const date = new Date();
  date.setDate(date.getDate() + days);
  return date.toISOString().slice(0, 10);
}

export function Models({
  api,
  providers,
  isAdmin,
  onChanged,
}: {
  api: PigoAPI;
  providers: ProviderInfo[];
  isAdmin: boolean;
  onChanged(): void;
}) {
  const [mine, setMine] = useState<CustomModelInfo[]>([]);
  const [shared, setShared] = useState<CustomModelInfo[]>([]);
  const [id, setID] = useState("");
  const [label, setLabel] = useState("");
  const [provider, setProvider] = useState("openrouter");
  const [expires, setExpires] = useState(inDays(30));
  const [asPublic, setAsPublic] = useState(false);
  const { report, view } = usePanelMessage();

  const refresh = useCallback(async () => {
    const [nextMine, nextShared] = await Promise.all([
      api.customModels(),
      isAdmin ? api.adminModels() : Promise.resolve([] as CustomModelInfo[]),
    ]);
    setMine(nextMine);
    setShared(nextShared);
  }, [api, isAdmin]);

  useEffect(() => {
    void refresh().catch((cause) => report(errorText(cause), true));
  }, [refresh, report]);

  async function act(run: () => Promise<string>) {
    try {
      const text = await run();
      await refresh();
      onChanged();
      report(text, false);
    } catch (cause) {
      report(errorText(cause), true);
    }
  }

  // A shared id shadows a personal one, so the personal row says so rather than
  // silently doing nothing.
  const sharedIDs = new Set(shared.map((item) => item.id));

  return (
    <section className="settings-section">
      <PanelHeading
        title="模型"
        hint="除了自动抓取的 OpenRouter 免费目录，这里维护手动添加的模型。"
        icon="M"
      />

      {(shared.length > 0 || mine.length > 0) && (
        <ul className="credential-list model-list">
          {shared.map((item) => (
            <li key={`public:${item.id}`}>
              <span className="credential-provider">
                {item.label}
                <em className="admin-badge">公共</em>
              </span>
              <code className="credential-hint">{item.provider}</code>
              {isAdmin && (
                <button
                  type="button"
                  className="ghost-button"
                  onClick={() => void act(async () => {
                    await api.deleteAdminModel(item.id);
                    return `已删除公共模型 ${item.id}`;
                  })}
                >
                  删除
                </button>
              )}
            </li>
          ))}
          {mine.map((item) => (
            <li key={`user:${item.id}`} className={item.expired ? "expired" : undefined}>
              <span className="credential-provider">
                {item.label}
                {sharedIDs.has(item.id) && <em className="admin-badge shadowed-badge">已被公共覆盖</em>}
                {item.expired && <em className="admin-badge disabled-badge">已过期</em>}
              </span>
              <code className="credential-hint">
                {item.provider} · 至 {item.expiresAt}
              </code>
              <button
                type="button"
                className="ghost-button"
                onClick={() => void act(async () => {
                  await api.deleteCustomModel(item.id);
                  return `已删除 ${item.id}`;
                })}
              >
                删除
              </button>
            </li>
          ))}
        </ul>
      )}

      <label className="field-label" htmlFor="model-id">模型 ID</label>
      <input
        id="model-id"
        className="settings-input"
        placeholder="如 deepseek-chat"
        value={id}
        onChange={(event) => setID(event.target.value)}
      />
      <label className="field-label" htmlFor="model-label">显示名（可选）</label>
      <input
        id="model-label"
        className="settings-input"
        value={label}
        onChange={(event) => setLabel(event.target.value)}
      />
      <label className="field-label" htmlFor="model-provider">Provider</label>
      <select
        id="model-provider"
        className="settings-input settings-select"
        value={provider}
        onChange={(event) => setProvider(event.target.value)}
      >
        {[...providers]
          .sort((a, b) => Number(b.hasKey) - Number(a.hasKey) || a.name.localeCompare(b.name))
          .map((item) => (
            <option key={item.name} value={item.name}>
              {item.name}
              {item.hasKey ? "" : "（未配置 Key）"}
            </option>
          ))}
        {providers.length === 0 && <option value="openrouter">openrouter</option>}
      </select>

      {isAdmin && (
        <label className="admin-toggle">
          <input type="checkbox" checked={asPublic} onChange={(event) => setAsPublic(event.target.checked)} />
          <span>加为公共模型（所有用户可见，不会过期）</span>
        </label>
      )}

      {/* A shared entry is the deployment's own catalog and does not expire, so
          the date disappears rather than sitting there meaninglessly. */}
      {!asPublic && (
        <>
          <label className="field-label" htmlFor="model-expires">有效期至</label>
          <input
            id="model-expires"
            className="settings-input"
            type="date"
            value={expires}
            onChange={(event) => setExpires(event.target.value)}
          />
        </>
      )}

      <button
        type="button"
        className="primary-button full-button"
        disabled={!id.trim() || (!asPublic && !expires)}
        onClick={() =>
          void act(async () => {
            if (asPublic) {
              await api.addAdminModel({ id: id.trim(), label: label.trim() || undefined, provider });
            } else {
              await api.addCustomModel({
                id: id.trim(),
                label: label.trim() || undefined,
                provider,
                expiresAt: expires,
              });
            }
            setID("");
            setLabel("");
            return asPublic ? "已添加公共模型" : "已添加模型";
          })
        }
      >
        {asPublic ? "添加公共模型" : "添加模型"}
      </button>
      {view}
    </section>
  );
}
