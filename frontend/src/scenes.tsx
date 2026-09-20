// The 场景 panel: the administrator's prepared prompts, each for one kind of
// task. Everyone can read them — prompt included — since a scene is only as
// trustworthy as what it tells the model; administrators edit them here.
//
// A scene is used from the welcome page, the 新会话 menu, or as "/slug …" in
// any conversation (see spec/preset-scenes.md).
import { useCallback, useEffect, useState } from "react";
import type { CustomModelInfo, ModelInfo, PigoAPI, SceneInfo, SceneList, SceneToolInfo } from "./api";
import { PanelHeading, errorText, usePanelMessage } from "./panel";

const thinkingLevels = ["off", "minimal", "low", "medium", "high", "xhigh", "max"];

// scenePromptPreview is what a scene session appends to the system prompt
// (sceneSystemPrompt on the server).
export function scenePromptPreview(scene: Pick<SceneInfo, "name" | "prompt">): string {
  return `## 当前场景：${scene.name}\n\n${scene.prompt}`;
}

const emptyScene: SceneInfo = { slug: "", name: "", icon: "", description: "", prompt: "", examples: [], tools: [] };

// exampleScene is the starting point "从示例创建" offers: querying the share of
// a drug or treatment a patient pays in a given region. yibao_query is the
// deployment's own data tool (tools.yaml, scope: scene), used when present.
const exampleScene: SceneInfo = {
  slug: "yibao",
  name: "医保自付查询",
  icon: "💊",
  description: "查询某地区药品或诊疗项目的医保类别与个人自付比例",
  prompt: `你是医保政策查询助手，帮助用户查询某地区某个药品或诊疗项目的医保报销情况，重点是个人自付比例。

## 需要的信息
- 地区：省和城市（医保政策以地市为单位，不同城市可能不同）
- 参保类型：职工医保或城乡居民医保
- 药品或诊疗项目的名称（药品最好有通用名和剂型）
缺少任何一项先向用户追问，不要猜测或默认。

## 查询方法
1. 如果有 yibao_query 工具，先用它查询医保目录。
2. 查不到，或需要确认当地最新规定时，用 websearch 搜索当地医疗保障局的官方文件，用 webfetch 打开原文核对。优先采用政府网站（gov.cn）的信息。
3. 注意区分：国家医保药品目录的甲类/乙类、地方规定的个人先行自付比例、门诊与住院的不同报销比例、起付线和封顶线。

## 回答要求
- 先给结论：医保类别（甲类/乙类/目录外）和个人先行自付比例。
- 再说明报销限制：限定支付范围、限定医院等级、门诊与住院的差异等。
- 列出依据：文件名称、发布机构、日期和链接。
- 信息有冲突或无法确认时如实说明，不要编造数字。
- 最后提醒：结果仅供参考，以当地医保经办机构的解释为准。`,
  examples: ["苏州 职工医保 阿莫西林胶囊", "北京 城乡居民医保 核磁共振检查", "上海 职工医保 司美格鲁肽注射液"],
  tools: ["yibao_query", "websearch", "webfetch"],
  thinking: "medium",
};

// modelChoices lists the models a scene can default to, with their provider:
// the public catalog, then built-in and free models. Personal models are left
// out — they resolve only for the user who added them.
function modelChoices(models: ModelInfo[], shared: CustomModelInfo[]) {
  return [
    {
      title: "公共模型",
      items: shared.filter((item) => !item.expired).map((item) => ({ id: item.id, provider: item.provider, label: `${item.label} · ${item.provider}` })),
    },
    {
      title: "已配置 Key 的内置模型",
      items: models.filter((item) => item.source === "preset").map((item) => ({ id: item.id, provider: item.provider, label: `${item.label} · ${item.provider}` })),
    },
    {
      title: "OpenRouter 免费",
      items: models.filter((item) => item.source === "free").map((item) => ({ id: item.id, provider: item.provider, label: item.label })),
    },
  ].filter((group) => group.items.length > 0);
}

const modelValue = (provider?: string, id?: string) => (id ? JSON.stringify([provider ?? "", id]) : "");

function SceneEditor({
  initial,
  tools,
  models,
  shared,
  creating,
  onSave,
  onCancel,
}: {
  initial: SceneInfo;
  tools: SceneToolInfo[];
  models: ModelInfo[];
  shared: CustomModelInfo[];
  creating: boolean;
  onSave(next: SceneInfo): Promise<boolean>;
  onCancel(): void;
}) {
  const [draft, setDraft] = useState<SceneInfo>(initial);
  const [examples, setExamples] = useState((initial.examples ?? []).join("\n"));
  const [restrict, setRestrict] = useState((initial.tools ?? []).length > 0);
  const [saving, setSaving] = useState(false);
  const chosen = new Set(draft.tools ?? []);
  const groups = modelChoices(models, shared);
  const currentModel = modelValue(draft.provider, draft.model);
  const knownModel = !draft.model || groups.some((group) => group.items.some((item) => modelValue(item.provider, item.id) === currentModel));
  const toolGroups = [
    { title: "内置工具", items: tools.filter((item) => item.builtin) },
    { title: "扩展工具", items: tools.filter((item) => !item.builtin && !item.scene) },
    { title: "场景专用工具", items: tools.filter((item) => item.scene) },
  ].filter((group) => group.items.length > 0);
  // Listed tools the config no longer has stay visible, so saving does not
  // drop them silently.
  const missing = (draft.tools ?? []).filter((name) => !tools.some((item) => item.name === name));

  function toggleTool(name: string, on: boolean) {
    const next = on ? [...chosen, name] : [...chosen].filter((item) => item !== name);
    setDraft({ ...draft, tools: next });
  }

  async function save() {
    setSaving(true);
    const ok = await onSave({
      ...draft,
      examples: examples.split("\n").map((line) => line.trim()).filter(Boolean),
      tools: restrict ? draft.tools : [],
    });
    setSaving(false);
    if (ok) onCancel();
  }

  return (
    <div className="add-model scene-editor">
      <div className="add-model-grid scene-editor-grid">
        <label>
          名称
          <input className="settings-input" placeholder="医保自付查询" value={draft.name} onChange={(event) => setDraft({ ...draft, name: event.target.value })} />
        </label>
        <label>
          命令名
          <input className="settings-input" placeholder="yibao" value={draft.slug} onChange={(event) => setDraft({ ...draft, slug: event.target.value })} />
        </label>
        <label>
          图标
          <input className="settings-input" placeholder="💊" value={draft.icon ?? ""} onChange={(event) => setDraft({ ...draft, icon: event.target.value })} />
        </label>
        <label>
          排序
          <input
            className="settings-input"
            type="number"
            value={draft.order ?? 0}
            onChange={(event) => setDraft({ ...draft, order: Number(event.target.value) || 0 })}
          />
        </label>
        <label className="scene-wide">
          描述
          <input
            className="settings-input"
            placeholder="一句话说明这个场景做什么，显示在卡片上"
            value={draft.description ?? ""}
            onChange={(event) => setDraft({ ...draft, description: event.target.value })}
          />
        </label>
        <label className="scene-wide">
          提示词
          <textarea
            className="settings-input scene-textarea"
            rows={12}
            placeholder={"你是……\n\n需要的信息：……缺什么先问，不要猜。\n先调用 xxx 工具查询；查不到再联网……\n回答要写清：……"}
            value={draft.prompt}
            onChange={(event) => setDraft({ ...draft, prompt: event.target.value })}
          />
        </label>
        <label className="scene-wide">
          示例问法（每行一条，最多 5 条）
          <textarea
            className="settings-input scene-textarea small"
            rows={3}
            placeholder="北京 职工医保 阿莫西林胶囊"
            value={examples}
            onChange={(event) => setExamples(event.target.value)}
          />
        </label>
        <label>
          默认模型
          <select
            className="settings-input settings-select"
            value={currentModel}
            onChange={(event) => {
              const [provider, model] = event.target.value ? (JSON.parse(event.target.value) as [string, string]) : ["", ""];
              setDraft({ ...draft, provider, model });
            }}
          >
            <option value="">部署默认</option>
            {groups.map((group) => (
              <optgroup key={group.title} label={group.title}>
                {group.items.map((item) => (
                  <option key={`${item.provider}:${item.id}`} value={modelValue(item.provider, item.id)}>
                    {item.label}
                  </option>
                ))}
              </optgroup>
            ))}
            {!knownModel && <option value={currentModel}>{`${draft.model} · 当前（不在可选列表中）`}</option>}
          </select>
        </label>
        <label>
          默认推理强度
          <select className="settings-input settings-select" value={draft.thinking ?? ""} onChange={(event) => setDraft({ ...draft, thinking: event.target.value })}>
            <option value="">部署默认</option>
            {thinkingLevels.map((level) => (
              <option key={level} value={level}>
                {level}
              </option>
            ))}
          </select>
        </label>
      </div>

      <div className="scene-tools">
        <label className="admin-toggle">
          <input type="checkbox" checked={restrict} onChange={(event) => setRestrict(event.target.checked)} />
          <span>限定工具 · 不勾选时与普通会话的工具相同；场景专用工具只有在这里勾选才会出现</span>
        </label>
        {restrict &&
          toolGroups.map((group) => (
            <div key={group.title} className="scene-tool-group">
              <span className="eyebrow">{group.title}</span>
              <div className="scene-tool-chips">
                {group.items.map((item) => (
                  <label key={item.name} className={chosen.has(item.name) ? "scene-tool-chip on" : "scene-tool-chip"} title={item.description}>
                    <input type="checkbox" checked={chosen.has(item.name)} onChange={(event) => toggleTool(item.name, event.target.checked)} />
                    {item.name}
                  </label>
                ))}
              </div>
            </div>
          ))}
        {restrict && missing.length > 0 && <p className="security-note">工具配置里已没有：{missing.join("、")}，保存会被拒绝，请取消勾选。</p>}
      </div>

      {draft.prompt.trim() && (
        <details className="scene-preview">
          <summary>预览：场景会话追加到 system prompt 的内容</summary>
          <pre>{scenePromptPreview({ name: draft.name || "（未命名）", prompt: draft.prompt.trim() })}</pre>
        </details>
      )}

      <div className="add-model-footer">
        <label className="admin-toggle scene-disabled-toggle">
          <input type="checkbox" checked={!!draft.disabled} onChange={(event) => setDraft({ ...draft, disabled: event.target.checked })} />
          <span>停用（不出现在入口，已有会话不受影响）</span>
        </label>
        <div className="context-editor-actions">
          <button
            type="button"
            className="primary-button"
            disabled={saving || !draft.name.trim() || !draft.slug.trim() || !draft.prompt.trim()}
            onClick={() => void save()}
          >
            {creating ? "创建场景" : "保存"}
          </button>
          <button type="button" className="ghost-button" onClick={onCancel}>
            取消
          </button>
        </div>
      </div>
    </div>
  );
}

function SceneRow({
  scene,
  isAdmin,
  editor,
  onEdit,
  onToggle,
  onDelete,
}: {
  scene: SceneInfo;
  isAdmin: boolean;
  editor: React.ReactNode;
  onEdit(): void;
  onToggle(): void;
  onDelete(): void;
}) {
  const [open, setOpen] = useState(false);
  const tools = scene.tools ?? [];
  return (
    <li className={scene.disabled ? "provider-row unconfigured" : "provider-row"}>
      <button type="button" className="provider-summary scene-summary" onClick={() => setOpen(!open)} aria-expanded={open}>
        <span className="scene-icon" aria-hidden="true">
          {scene.icon || scene.name.slice(0, 1)}
        </span>
        <span className="scene-title">
          <strong>
            {scene.name}
            {scene.disabled && <em className="admin-badge disabled-badge">已停用</em>}
            {(scene.missingTools ?? []).length > 0 && <em className="admin-badge shadowed-badge">缺少工具</em>}
          </strong>
          <small>{scene.description || "（无描述）"}</small>
        </span>
        <code className="scene-slug">/{scene.slug}</code>
        <span className="provider-model-count">{tools.length > 0 ? `${tools.length} 个工具` : "默认工具"}</span>
        <span className="provider-caret">{open || editor ? "▾" : "▸"}</span>
      </button>
      {editor ? (
        <div className="provider-body">{editor}</div>
      ) : (
        open && (
          <div className="provider-body">
            <dl className="scene-facts">
              <dt>命令</dt>
              <dd>
                <code>/{scene.slug} 你的问题</code>
              </dd>
              <dt>工具</dt>
              <dd>
                {tools.length > 0
                  ? tools.map((name) => (
                      <code key={name} className={(scene.missingTools ?? []).includes(name) ? "scene-tool missing" : "scene-tool"}>
                        {name}
                      </code>
                    ))
                  : "与普通会话相同"}
              </dd>
              <dt>默认模型</dt>
              <dd>
                {scene.model ? `${scene.model} · ${scene.provider}` : "部署默认"}
                {scene.thinking ? ` · 推理 ${scene.thinking}` : ""}
              </dd>
              {(scene.examples ?? []).length > 0 && (
                <>
                  <dt>示例</dt>
                  <dd className="scene-examples">
                    {scene.examples?.map((example) => (
                      <span key={example}>{example}</span>
                    ))}
                  </dd>
                </>
              )}
            </dl>
            <pre className="scene-prompt">{scene.prompt}</pre>
            <div className="provider-footer">
              <span>{scene.updatedAt ? `更新于 ${new Date(scene.updatedAt).toLocaleString()}${scene.updatedBy ? ` · ${scene.updatedBy}` : ""}` : ""}</span>
              {isAdmin && (
                <div className="context-editor-actions">
                  <button type="button" className="ghost-button" onClick={onEdit}>
                    编辑
                  </button>
                  <button type="button" className="ghost-button" onClick={onToggle}>
                    {scene.disabled ? "启用" : "停用"}
                  </button>
                  <button
                    type="button"
                    className="ghost-button danger-button"
                    onClick={() => {
                      if (window.confirm(`删除场景「${scene.name}」？已有的场景会话不受影响。`)) onDelete();
                    }}
                  >
                    删除
                  </button>
                </div>
              )}
            </div>
          </div>
        )
      )}
    </li>
  );
}

export function Scenes({
  api,
  isAdmin,
  models,
  onChanged,
}: {
  api: PigoAPI;
  isAdmin: boolean;
  models: ModelInfo[];
  // onChanged lets the app reload the scenes it shows on the welcome page
  // and in the 新会话 menu.
  onChanged(): void;
}) {
  const [list, setList] = useState<SceneList>({ scenes: [], tools: [] });
  const [shared, setShared] = useState<CustomModelInfo[]>([]);
  // editing: the slug being edited, "" for a new scene, null for none;
  // draft is what a new scene starts from.
  const [editing, setEditing] = useState<string | null>(null);
  const [draft, setDraft] = useState<SceneInfo>(emptyScene);
  const { report, view } = usePanelMessage();

  const refresh = useCallback(async () => {
    setList(isAdmin ? await api.adminScenes() : await api.scenes());
  }, [api, isAdmin]);

  useEffect(() => {
    void refresh().catch((cause) => report(errorText(cause), true));
    if (isAdmin) void api.adminModels().then(setShared).catch(() => setShared([]));
  }, [api, isAdmin, refresh, report]);

  async function act(run: () => Promise<SceneList>, text: string): Promise<boolean> {
    try {
      setList(await run());
      onChanged();
      report(text, false);
      return true;
    } catch (cause) {
      report(errorText(cause), true);
      return false;
    }
  }

  const editorFor = (scene: SceneInfo, creating: boolean) => (
    <SceneEditor
      initial={scene}
      tools={list.tools}
      models={models}
      shared={shared}
      creating={creating}
      onSave={(next) => act(() => api.putScene(creating ? "" : scene.slug, next), creating ? `已创建场景 ${next.name}` : `已保存场景 ${next.name}`)}
      onCancel={() => setEditing(null)}
    />
  );

  const enabled = list.scenes.filter((scene) => !scene.disabled).length;

  return (
    <section className="settings-section">
      <PanelHeading
        title="场景"
        hint="为一类专门任务预设的提示词。可从欢迎页或「新会话」下拉开启场景会话，也可以在任意会话里用 /命令 单次使用。"
        icon="景"
      />
      <div className="model-toolbar">
        <h4>
          场景 <span className="credential-note">{isAdmin ? `${enabled} / ${list.scenes.length} 启用` : `${enabled} 个`}</span>
        </h4>
        {isAdmin && editing !== "" && (
          <div className="context-editor-actions">
            {!list.scenes.some((scene) => scene.slug === exampleScene.slug) && (
              <button
                type="button"
                className="ghost-button"
                onClick={() => {
                  // Only the example's tools this deployment has.
                  const tools = (exampleScene.tools ?? []).filter((name) => list.tools.some((item) => item.name === name));
                  setDraft({ ...exampleScene, tools });
                  setEditing("");
                }}
              >
                从示例创建
              </button>
            )}
            <button
              type="button"
              className="primary-button"
              onClick={() => {
                setDraft(emptyScene);
                setEditing("");
              }}
            >
              + 新建场景
            </button>
          </div>
        )}
      </div>

      {editing === "" && editorFor(draft, true)}

      {list.scenes.length > 0 ? (
        <ul className="provider-list">
          {list.scenes.map((scene) => (
            <SceneRow
              key={scene.slug}
              scene={scene}
              isAdmin={isAdmin}
              editor={editing === scene.slug ? editorFor(scene, false) : null}
              onEdit={() => setEditing(scene.slug)}
              onToggle={() =>
                void act(() => api.putScene(scene.slug, { ...scene, disabled: !scene.disabled }), `已${scene.disabled ? "启用" : "停用"}场景 ${scene.name}`)
              }
              onDelete={() => void act(() => api.deleteScene(scene.slug), `已删除场景 ${scene.name}`)}
            />
          ))}
        </ul>
      ) : (
        editing !== "" && <p className="credential-note">{isAdmin ? "还没有场景，点「+ 新建场景」开始。" : "管理员还没有配置场景。"}</p>
      )}
      {view}
    </section>
  );
}
