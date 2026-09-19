// The Provider panel: one table for everything that makes an endpoint usable —
// its address, and the credentials at each tier.
//
// This replaces the earlier split between "我的 API Key" and "公共 API Key",
// which were the same concept filed under two owners and therefore sat in two
// places in the navigation. A user who wanted to use DeepSeek had to visit one
// panel for the key and another for the model, and had nowhere at all to put an
// address. Here a provider is one row, and who owns which key is a property of
// that row rather than a reason to split the page.
//
// The precedence a run actually applies — a user's own key, then the shared
// pool, then the process environment — is shown on the row instead of being
// described in prose, because it is the thing people get wrong.
import { useCallback, useEffect, useState } from "react";
import type { CredentialList, CustomModelInfo, CustomProvider, PigoAPI, ProviderInfo } from "./api";
import { PanelHeading, errorText, usePanelMessage } from "./panel";
import { navigate } from "./route";

// tierLabel names the credential tiers in the order the server applies them.
const tiers = [
  { key: "user", label: "我的 Key" },
  { key: "public", label: "公共" },
  { key: "env", label: "环境变量" },
] as const;

// ProviderRow shows one endpoint: which tier is in effect, the address when it
// is a custom one, and the controls to change either.
function ProviderRow({
  info,
  isAdmin,
  userHint,
  publicHint,
  modelCount,
  onSaveKey,
  onDeleteKey,
  onRemoveProvider,
  onEditProvider,
}: {
  info: ProviderInfo;
  isAdmin: boolean;
  userHint?: string;
  publicHint?: string;
  modelCount: number;
  onSaveKey(scope: "user" | "public", key: string): void;
  onDeleteKey(scope: "user" | "public"): void;
  onRemoveProvider(): void;
  onEditProvider(next: CustomProvider): Promise<boolean>;
}) {
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState<CustomProvider>({ name: "", protocol: "openai", baseUrl: "", conversationId: false });
  const [userKey, setUserKey] = useState("");
  const [publicKey, setPublicKey] = useState("");

  return (
    <li className={info.source === "none" ? "provider-row unconfigured" : "provider-row"}>
      <button type="button" className="provider-summary" onClick={() => setOpen(!open)} aria-expanded={open}>
        <span className="provider-name">
          {info.name}
          {info.custom && <em className="admin-badge">自定义</em>}
        </span>
        <span className="provider-tiers">
          {tiers.map((tier) => (
            <em key={tier.key} className={info.source === tier.key ? "tier active" : "tier"}>
              {info.source === tier.key ? "●" : "○"} {tier.label}
            </em>
          ))}
        </span>
        <span className="provider-caret">{open ? "▾" : "▸"}</span>
      </button>

      {open && (
        <div className="provider-detail">
          {info.custom && editing ? (
            <div className="provider-edit">
              <label className="field-label" htmlFor={`edit-name-${info.name}`}>名称</label>
              <input
                id={`edit-name-${info.name}`}
                className="settings-input"
                value={draft.name}
                onChange={(event) => setDraft({ ...draft, name: event.target.value })}
              />
              <label className="field-label" htmlFor={`edit-protocol-${info.name}`}>协议</label>
              <select
                id={`edit-protocol-${info.name}`}
                className="settings-input settings-select"
                value={draft.protocol}
                onChange={(event) =>
                  setDraft({
                    ...draft,
                    protocol: event.target.value,
                    conversationId: event.target.value === "openai" && draft.conversationId,
                  })
                }
              >
                <option value="openai">openai（Chat Completions 兼容）</option>
                <option value="anthropic">anthropic（Messages 兼容）</option>
              </select>
              <label className="field-label" htmlFor={`edit-url-${info.name}`}>Base URL</label>
              <input
                id={`edit-url-${info.name}`}
                className="settings-input"
                value={draft.baseUrl}
                onChange={(event) => setDraft({ ...draft, baseUrl: event.target.value })}
              />
              <label className="admin-toggle">
                <input
                  type="checkbox"
                  checked={!!draft.conversationId}
                  disabled={draft.protocol !== "openai"}
                  onChange={(event) => setDraft({ ...draft, conversationId: event.target.checked })}
                />
                <span>请求带会话 ID（conversation_id）· 用于 workbuddy2api 等 CodeBuddy 反代，上游据此复用前缀缓存；仅 openai 协议</span>
              </label>
              {draft.name.trim().toLowerCase() !== info.name && (
                <p className="security-note">
                  改名会一并迁移它的 Key、模型、价格和会话；账单记录保留原名。
                </p>
              )}
              <div className="provider-key-row">
                <button
                  type="button"
                  className="primary-button"
                  disabled={!draft.name.trim() || !draft.baseUrl.trim()}
                  onClick={() =>
                    void onEditProvider({
                      name: draft.name.trim(),
                      protocol: draft.protocol,
                      baseUrl: draft.baseUrl.trim(),
                      conversationId: draft.conversationId,
                    }).then((ok) => ok && setEditing(false))
                  }
                >
                  保存
                </button>
                <button type="button" className="ghost-button" onClick={() => setEditing(false)}>
                  取消
                </button>
              </div>
            </div>
          ) : info.custom ? (
            <p className="provider-endpoint">
              <code>{info.protocol}</code> · <code>{info.baseUrl}</code>
              {info.conversationId && " · 带会话 ID"}
              {isAdmin && (
                <button
                  type="button"
                  className="ghost-button"
                  onClick={() => {
                    setDraft({
                      name: info.name,
                      protocol: info.protocol ?? "openai",
                      baseUrl: info.baseUrl ?? "",
                      conversationId: !!info.conversationId,
                    });
                    setEditing(true);
                  }}
                >
                  编辑
                </button>
              )}
            </p>
          ) : (
            <p className="provider-endpoint">
              内置 · 未配置 Key 时回退到环境变量 <code>{info.keyHint}</code>
            </p>
          )}

          <label className="field-label">我的 Key{userHint ? ` · ${userHint}` : ""}</label>
          <div className="provider-key-row">
            <input
              className="settings-input"
              type="password"
              autoComplete="off"
              placeholder={userHint ? "输入新值以替换" : "sk-..."}
              value={userKey}
              onChange={(event) => setUserKey(event.target.value)}
            />
            <button
              type="button"
              className="ghost-button"
              disabled={!userKey.trim()}
              onClick={() => {
                onSaveKey("user", userKey.trim());
                setUserKey("");
              }}
            >
              保存
            </button>
            {userHint && (
              <button type="button" className="ghost-button danger-button" onClick={() => onDeleteKey("user")}>
                删除
              </button>
            )}
          </div>

          {isAdmin && (
            <>
              <label className="field-label">公共 Key{publicHint ? ` · ${publicHint}` : ""}</label>
              <div className="provider-key-row">
                <input
                  className="settings-input"
                  type="password"
                  autoComplete="off"
                  placeholder={publicHint ? "输入新值以替换" : "所有用户共用"}
                  value={publicKey}
                  onChange={(event) => setPublicKey(event.target.value)}
                />
                <button
                  type="button"
                  className="ghost-button"
                  disabled={!publicKey.trim()}
                  onClick={() => {
                    onSaveKey("public", publicKey.trim());
                    setPublicKey("");
                  }}
                >
                  保存
                </button>
                {publicHint && (
                  <button type="button" className="ghost-button danger-button" onClick={() => onDeleteKey("public")}>
                    删除
                  </button>
                )}
              </div>
            </>
          )}

          {/* A provider on its own runs nothing: it supplies an address and a
              key, and a model has to name it. Saying so here is the difference
              between "I configured it and nothing happened" and knowing the
              next step. */}
          <p className="provider-models">
            {modelCount > 0
              ? `${modelCount} 个模型使用此 Provider`
              : "还没有模型使用它 —— Provider 只提供地址和凭据，需要再添加一个模型。"}
            <button type="button" className="ghost-button" onClick={() => navigate("/settings/models")}>
              {modelCount > 0 ? "管理模型" : "去添加模型"}
            </button>
          </p>

          {isAdmin && info.custom && (
            <button type="button" className="ghost-button danger-button provider-remove" onClick={onRemoveProvider}>
              删除此 Provider
            </button>
          )}
        </div>
      )}
    </li>
  );
}

// AddProvider is the one place an endpoint enters the list: pick a built-in to
// configure, or define a custom one. Only administrators can define custom
// endpoints — pointing the server at an arbitrary URL is a deployment decision.
function AddProvider({
  builtins,
  isAdmin,
  onPick,
  onCreate,
}: {
  builtins: ProviderInfo[];
  isAdmin: boolean;
  onPick(name: string): void;
  onCreate(next: CustomProvider): void;
}) {
  const [mode, setMode] = useState<"builtin" | "custom">("builtin");
  const [picked, setPicked] = useState("");
  const [name, setName] = useState("");
  const [protocol, setProtocol] = useState("openai");
  const [baseUrl, setBaseUrl] = useState("");
  const [conversationId, setConversationId] = useState(false);

  return (
    <div className="provider-add">
      <div className="provider-add-tabs">
        <button
          type="button"
          className={mode === "builtin" ? "ghost-button active" : "ghost-button"}
          onClick={() => setMode("builtin")}
        >
          内置 Provider
        </button>
        {isAdmin && (
          <button
            type="button"
            className={mode === "custom" ? "ghost-button active" : "ghost-button"}
            onClick={() => setMode("custom")}
          >
            自定义端点
          </button>
        )}
      </div>

      {mode === "builtin" ? (
        <>
          <select
            className="settings-input settings-select"
            aria-label="选择内置 Provider"
            value={picked}
            onChange={(event) => setPicked(event.target.value)}
          >
            <option value="">选择一个…</option>
            {builtins.map((item) => (
              <option key={item.name} value={item.name}>{item.name}</option>
            ))}
          </select>
          <button
            type="button"
            className="primary-button full-button"
            disabled={!picked}
            onClick={() => {
              onPick(picked);
              setPicked("");
            }}
          >
            添加到列表
          </button>
        </>
      ) : (
        <>
          <label className="field-label" htmlFor="custom-name">名称</label>
          <input
            id="custom-name"
            className="settings-input"
            placeholder="my-gateway"
            value={name}
            onChange={(event) => setName(event.target.value)}
          />
          <label className="field-label" htmlFor="custom-protocol">协议</label>
          <select
            id="custom-protocol"
            className="settings-input settings-select"
            value={protocol}
            onChange={(event) => {
              setProtocol(event.target.value);
              if (event.target.value !== "openai") setConversationId(false);
            }}
          >
            <option value="openai">openai（Chat Completions 兼容）</option>
            <option value="anthropic">anthropic（Messages 兼容）</option>
          </select>
          <label className="field-label" htmlFor="custom-url">Base URL</label>
          <input
            id="custom-url"
            className="settings-input"
            placeholder="http://10.0.0.5:8000/v1"
            value={baseUrl}
            onChange={(event) => setBaseUrl(event.target.value)}
          />
          <label className="admin-toggle">
            <input
              type="checkbox"
              checked={conversationId}
              disabled={protocol !== "openai"}
              onChange={(event) => setConversationId(event.target.checked)}
            />
            <span>请求带会话 ID（conversation_id）· 用于 workbuddy2api 等 CodeBuddy 反代，上游据此复用前缀缓存；仅 openai 协议</span>
          </label>
          <button
            type="button"
            className="primary-button full-button"
            disabled={!name.trim() || !baseUrl.trim()}
            onClick={() => {
              onCreate({ name: name.trim(), protocol, baseUrl: baseUrl.trim(), conversationId });
              setName("");
              setBaseUrl("");
              setConversationId(false);
            }}
          >
            添加端点
          </button>
          <p className="security-note">
            本地无鉴权的端点（vLLM / LMStudio 等）仍需在上面填一个占位 Key，驱动会拒绝空凭据。
          </p>
        </>
      )}
    </div>
  );
}

export function Providers({
  api,
  isAdmin,
  onChanged,
}: {
  api: PigoAPI;
  isAdmin: boolean;
  // onChanged lets the settings page reload the provider list it hands to the
  // model panel; the panels share one component instance, so a stale copy would
  // otherwise hide a just-added provider from the model form.
  onChanged(): void;
}) {
  const [providers, setProviders] = useState<ProviderInfo[]>([]);
  const [mine, setMine] = useState<CredentialList | null>(null);
  const [shared, setShared] = useState<CredentialList | null>(null);
  const [models, setModels] = useState<CustomModelInfo[]>([]);
  // shown holds the built-ins the user explicitly added to the table this
  // visit; a built-in with no key anywhere is otherwise hidden, since listing
  // all 38 would bury the handful that matter.
  const [shown, setShown] = useState<string[]>([]);
  const { report, view } = usePanelMessage();

  const refresh = useCallback(async () => {
    const [nextProviders, nextMine, nextModels] = await Promise.all([
      api.providers(),
      api.credentials(),
      api.customModels(),
    ]);
    setProviders(nextProviders);
    setMine(nextMine);
    setModels(isAdmin ? nextModels.concat(await api.adminModels()) : nextModels);
    if (isAdmin) setShared(await api.adminCredentials());
  }, [api, isAdmin]);

  useEffect(() => {
    void refresh().catch((cause) => report(errorText(cause), true));
  }, [refresh, report]);

  function hintFor(list: CredentialList | null, name: string) {
    return list?.credentials.find((item) => item.provider === name)?.hint;
  }

  // act runs one change and reports it; it resolves true when the change
  // went through, so a form can close only on success.
  async function act(run: () => Promise<string>): Promise<boolean> {
    try {
      const text = await run();
      await refresh();
      onChanged();
      report(text, false);
      return true;
    } catch (cause) {
      report(errorText(cause), true);
      return false;
    }
  }

  // A row is worth showing when something is configured for it, when it is a
  // custom endpoint, or when the user just picked it.
  const rows = providers.filter(
    (item) => item.custom || item.source !== "none" || shown.includes(item.name),
  );
  const addable = providers.filter((item) => !rows.includes(item) && !item.custom);
  const disabled = mine && !mine.enabled;

  return (
    <section className="settings-section">
      <PanelHeading
        title="Provider"
        hint="模型从这里取地址和凭据。同一个 Provider 上，我的 Key 优先于公共，公共优先于环境变量。"
        icon="PRV"
      />
      {disabled && <p className="security-note">{mine?.reason}</p>}

      {rows.length > 0 && (
        <ul className="provider-list">
          {rows.map((info) => (
            <ProviderRow
              key={info.name}
              info={info}
              isAdmin={isAdmin}
              userHint={hintFor(mine, info.name)}
              publicHint={hintFor(shared, info.name)}
              modelCount={models.filter((item) => item.provider === info.name).length}
              onSaveKey={(scope, key) =>
                void act(async () => {
                  if (scope === "user") await api.setCredential(info.name, key);
                  else await api.setAdminCredential(info.name, key);
                  return `已保存 ${info.name} 的${scope === "user" ? "个人" : "公共"} Key`;
                })
              }
              onDeleteKey={(scope) =>
                void act(async () => {
                  if (scope === "user") await api.deleteCredential(info.name);
                  else await api.deleteAdminCredential(info.name);
                  return `已删除 ${info.name} 的${scope === "user" ? "个人" : "公共"} Key`;
                })
              }
              onEditProvider={(next) =>
                act(async () => {
                  await api.patchAdminProvider(info.name, next);
                  const renamed = next.name.toLowerCase() !== info.name;
                  return renamed ? `已将 ${info.name} 改名为 ${next.name.toLowerCase()}` : `已更新端点 ${info.name}`;
                })
              }
              onRemoveProvider={() =>
                void act(async () => {
                  await api.deleteAdminProvider(info.name);
                  return `已删除端点 ${info.name}`;
                })
              }
            />
          ))}
        </ul>
      )}

      <AddProvider
        builtins={addable}
        isAdmin={isAdmin}
        onPick={(name) => setShown([...shown, name])}
        onCreate={(next) =>
          void act(async () => {
            await api.putAdminProvider(next);
            setShown([...shown, next.name]);
            return `已添加端点 ${next.name}`;
          })
        }
      />
      {view}
    </section>
  );
}
