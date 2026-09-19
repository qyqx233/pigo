// The 服务商 (provider) panel: one row per endpoint a model can run on — its
// address, and the API keys at each tier. The row's header says at a glance
// whether it can be used and with whose key; the keys, the address and the
// endpoint's settings open below it.
//
// A run uses the user's own key first, then the shared (公共) one; a provider
// with neither cannot be used — the server's environment is not consulted.
import { useCallback, useEffect, useState } from "react";
import type { CredentialList, CustomModelInfo, CustomProvider, PigoAPI, ProviderInfo } from "./api";
import { PanelHeading, errorText, usePanelMessage } from "./panel";
import { navigate } from "./route";

// KeyStatus is the row's one-line answer to "can I use this, and on whose key".
function KeyStatus({ info, userHint, publicHint }: { info: ProviderInfo; userHint?: string; publicHint?: string }) {
  if (info.source === "user") return <span className="key-status ok">我的 Key {userHint}</span>;
  if (info.source === "public") return <span className="key-status ok">公共 Key{publicHint ? ` ${publicHint}` : ""}</span>;
  return <span className="key-status missing">未配置 Key</span>;
}

// KeyField sets or clears one tier's key.
function KeyField({
  label,
  hint,
  note,
  placeholder,
  onSave,
  onDelete,
}: {
  label: string;
  hint?: string;
  note?: string;
  placeholder: string;
  onSave(key: string): void;
  onDelete(): void;
}) {
  const [value, setValue] = useState("");
  return (
    <div className="key-field">
      <div className="key-field-label">
        <strong>{label}</strong>
        <span>{hint ? `已保存 ${hint}` : "未设置"}</span>
        {note && <small>{note}</small>}
      </div>
      <input
        className="settings-input"
        type="password"
        autoComplete="off"
        placeholder={hint ? "输入新值以替换" : placeholder}
        value={value}
        onChange={(event) => setValue(event.target.value)}
      />
      <div className="context-editor-actions">
        <button
          type="button"
          className="primary-button"
          disabled={!value.trim()}
          onClick={() => {
            onSave(value.trim());
            setValue("");
          }}
        >
          保存
        </button>
        {hint && (
          <button type="button" className="ghost-button danger-button" onClick={onDelete}>
            删除
          </button>
        )}
      </div>
    </div>
  );
}

// EndpointForm edits (or creates) a custom endpoint.
function EndpointForm({
  initial,
  submitLabel,
  renaming,
  onSubmit,
  onCancel,
}: {
  initial: CustomProvider;
  submitLabel: string;
  // renaming: editing an existing endpoint, whose name change moves its keys,
  // models, prices and sessions along.
  renaming?: string;
  onSubmit(next: CustomProvider): Promise<boolean>;
  onCancel?(): void;
}) {
  const [draft, setDraft] = useState<CustomProvider>(initial);
  const renamed = renaming !== undefined && draft.name.trim().toLowerCase() !== renaming;
  return (
    <div className="add-model">
      <div className="add-model-grid">
        <label>
          名称
          <input className="settings-input" placeholder="my-gateway" value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} />
        </label>
        <label>
          协议
          <select
            className="settings-input settings-select"
            value={draft.protocol}
            onChange={(event) =>
              setDraft({ ...draft, protocol: event.target.value, conversationId: event.target.value === "openai" && draft.conversationId })
            }
          >
            <option value="openai">openai · Chat Completions</option>
            <option value="anthropic">anthropic · Messages</option>
          </select>
        </label>
        <label className="endpoint-url">
          Base URL
          <input className="settings-input" placeholder="http://10.0.0.5:8000/v1" value={draft.baseUrl} onChange={(event) => setDraft({ ...draft, baseUrl: event.target.value })} />
        </label>
      </div>
      <label className="admin-toggle">
        <input
          type="checkbox"
          checked={!!draft.conversationId}
          disabled={draft.protocol !== "openai"}
          onChange={(event) => setDraft({ ...draft, conversationId: event.target.checked })}
        />
        <span>请求带会话 ID（conversation_id）· 用于 workbuddy2api 等 CodeBuddy 反代，上游据此复用前缀缓存；仅 openai 协议</span>
      </label>
      {renamed && <p className="security-note">改名会一并迁移它的 Key、模型、价格和会话；账单记录保留原名。</p>}
      <div className="add-model-footer">
        <span className="credential-note">本地无鉴权的端点（vLLM、LMStudio 等）也要填一个占位 Key，驱动不接受空凭据。</span>
        <div className="context-editor-actions">
          <button
            type="button"
            className="primary-button"
            disabled={!draft.name.trim() || !draft.baseUrl.trim()}
            onClick={() =>
              void onSubmit({ name: draft.name.trim(), protocol: draft.protocol, baseUrl: draft.baseUrl.trim(), conversationId: draft.conversationId })
            }
          >
            {submitLabel}
          </button>
          {onCancel && (
            <button type="button" className="ghost-button" onClick={onCancel}>
              取消
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

// ProviderRow is one endpoint: its status in the header, the rest below.
function ProviderRow({
  info,
  isAdmin,
  userHint,
  publicHint,
  modelCount,
  keysEnabled,
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
  keysEnabled: boolean;
  onSaveKey(scope: "user" | "public", key: string): void;
  onDeleteKey(scope: "user" | "public"): void;
  onRemoveProvider(): void;
  onEditProvider(next: CustomProvider): Promise<boolean>;
}) {
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState(false);

  return (
    <li className={info.source === "none" ? "provider-row unconfigured" : "provider-row"}>
      <button type="button" className="provider-summary" onClick={() => setOpen(!open)} aria-expanded={open}>
        <span className="provider-name">
          {info.name}
          {info.custom && <em className="admin-badge">自定义</em>}
        </span>
        <span className="provider-endpoint-short" title={info.custom ? info.baseUrl : undefined}>
          {info.custom ? `${info.protocol} · ${info.baseUrl}${info.conversationId ? " · 带会话 ID" : ""}` : "内置"}
        </span>
        <span className="provider-model-count">{modelCount > 0 ? `${modelCount} 个模型` : "无模型"}</span>
        <KeyStatus info={info} userHint={userHint} publicHint={publicHint} />
        <span className="provider-caret">{open ? "▾" : "▸"}</span>
      </button>

      {open && (
        <div className="provider-body">
          {(keysEnabled || isAdmin) && (
            <div className="key-grid">
              {keysEnabled && (
                <KeyField
                  label="我的 Key"
                  hint={userHint}
                  note="只有你用，优先于公共 Key"
                  placeholder="sk-..."
                  onSave={(key) => onSaveKey("user", key)}
                  onDelete={() => onDeleteKey("user")}
                />
              )}
              {isAdmin && (
                <KeyField
                  label="公共 Key"
                  hint={publicHint}
                  note="所有用户共用，费用由平台承担"
                  placeholder="所有用户共用"
                  onSave={(key) => onSaveKey("public", key)}
                  onDelete={() => onDeleteKey("public")}
                />
              )}
            </div>
          )}

          {info.custom && editing && (
            <EndpointForm
              initial={{
                name: info.name,
                protocol: info.protocol ?? "openai",
                baseUrl: info.baseUrl ?? "",
                conversationId: !!info.conversationId,
              }}
              submitLabel="保存端点"
              renaming={info.name}
              onSubmit={(next) =>
                onEditProvider(next).then((ok) => {
                  if (ok) setEditing(false);
                  return ok;
                })
              }
              onCancel={() => setEditing(false)}
            />
          )}
          {!info.custom && <p className="provider-note">内置服务商，地址固定；只使用这里配置的 Key，不读取服务端环境变量。</p>}

          {/* A provider on its own runs nothing: a model has to name it. */}
          <div className="provider-footer">
            <span>
              {modelCount > 0 ? `${modelCount} 个模型使用此服务商` : "还没有模型使用它 —— 服务商只提供地址和 Key，还需要添加模型。"}
            </span>
            <div className="context-editor-actions">
              <button type="button" className="ghost-button" onClick={() => navigate("/settings/models")}>
                {modelCount > 0 ? "管理模型" : "去添加模型"}
              </button>
              {isAdmin && info.custom && !editing && (
                <button type="button" className="ghost-button" onClick={() => setEditing(true)}>
                  编辑端点
                </button>
              )}
              {isAdmin && info.custom && (
                <button
                  type="button"
                  className="ghost-button danger-button"
                  onClick={() => {
                    if (window.confirm(`删除服务商 ${info.name}？它的 Key 会一并删除，使用它的模型将不可用。`)) onRemoveProvider();
                  }}
                >
                  删除服务商
                </button>
              )}
            </div>
          </div>
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
  onClose,
}: {
  builtins: ProviderInfo[];
  isAdmin: boolean;
  onPick(name: string): void;
  onCreate(next: CustomProvider): Promise<boolean>;
  onClose(): void;
}) {
  const [mode, setMode] = useState<"builtin" | "custom">("builtin");
  const [picked, setPicked] = useState("");

  return (
    <div className="provider-add-panel">
      <div className="provider-add-tabs">
        <button type="button" className={mode === "builtin" ? "ghost-button active" : "ghost-button"} onClick={() => setMode("builtin")}>
          内置服务商
        </button>
        {isAdmin && (
          <button type="button" className={mode === "custom" ? "ghost-button active" : "ghost-button"} onClick={() => setMode("custom")}>
            自定义端点
          </button>
        )}
      </div>

      {mode === "builtin" ? (
        <div className="add-model">
          <div className="provider-pick">
            <select
              className="settings-input settings-select"
              aria-label="选择内置服务商"
              value={picked}
              onChange={(event) => setPicked(event.target.value)}
            >
              <option value="">{builtins.length > 0 ? "选择一个内置服务商…" : "内置服务商都已在列表中"}</option>
              {builtins.map((item) => (
                <option key={item.name} value={item.name}>
                  {item.name}
                </option>
              ))}
            </select>
            <div className="context-editor-actions">
              <button
                type="button"
                className="primary-button"
                disabled={!picked}
                onClick={() => {
                  onPick(picked);
                  setPicked("");
                  onClose();
                }}
              >
                加入列表
              </button>
              <button type="button" className="ghost-button" onClick={onClose}>
                取消
              </button>
            </div>
          </div>
          <p className="credential-note">加入后在它的行里填 Key 即可使用；没有 Key 的内置服务商刷新后不再显示。</p>
        </div>
      ) : (
        <EndpointForm
          initial={{ name: "", protocol: "openai", baseUrl: "", conversationId: false }}
          submitLabel="添加端点"
          onSubmit={(next) =>
            onCreate(next).then((ok) => {
              if (ok) onClose();
              return ok;
            })
          }
          onCancel={onClose}
        />
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
  // shown holds the built-ins the user explicitly added to the list this
  // visit; a built-in with no key anywhere is otherwise hidden, since listing
  // all of them would bury the handful that matter.
  const [shown, setShown] = useState<string[]>([]);
  const [adding, setAdding] = useState(false);
  const { report, view } = usePanelMessage();

  const refresh = useCallback(async () => {
    const [nextProviders, nextMine, nextModels] = await Promise.all([api.providers(), api.credentials(), api.customModels()]);
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
  // custom endpoint, or when the user just picked it. Usable ones come first.
  const rows = providers
    .filter((item) => item.custom || item.source !== "none" || shown.includes(item.name))
    .sort((a, b) => Number(a.source === "none") - Number(b.source === "none"));
  const addable = providers.filter((item) => !rows.includes(item) && !item.custom);
  const keysEnabled = !mine || mine.enabled;
  const usable = rows.filter((item) => item.source !== "none").length;

  return (
    <section className="settings-section">
      <PanelHeading
        title="模型服务商"
        hint="模型从服务商取地址和 Key。同一个服务商上，我的 Key 优先于公共 Key；两者都没有时不可用。"
        icon="商"
      />
      {!keysEnabled && <p className="security-note">{mine?.reason}</p>}

      <div className="model-toolbar">
        <h4>
          服务商 <span className="credential-note">{usable} / {rows.length} 可用</span>
        </h4>
        {!adding && (
          <button type="button" className="primary-button" onClick={() => setAdding(true)}>
            + 添加服务商
          </button>
        )}
      </div>

      {adding && (
        <AddProvider
          builtins={addable}
          isAdmin={isAdmin}
          onPick={(name) => setShown([...shown, name])}
          onCreate={(next) =>
            act(async () => {
              await api.putAdminProvider(next);
              setShown([...shown, next.name]);
              return `已添加端点 ${next.name}`;
            })
          }
          onClose={() => setAdding(false)}
        />
      )}

      {rows.length > 0 ? (
        <ul className="provider-list">
          {rows.map((info) => (
            <ProviderRow
              key={info.name}
              info={info}
              isAdmin={isAdmin}
              userHint={hintFor(mine, info.name)}
              publicHint={hintFor(shared, info.name)}
              modelCount={models.filter((item) => item.provider === info.name).length}
              keysEnabled={keysEnabled}
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
                  return `已删除服务商 ${info.name}`;
                })
              }
            />
          ))}
        </ul>
      ) : (
        <p className="credential-note">还没有可用的服务商，点「+ 添加服务商」开始。</p>
      )}
      {view}
    </section>
  );
}
