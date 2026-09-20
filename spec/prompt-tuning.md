# SPEC: 系统提示词与工具描述的长期优化

- 状态：**长期迭代中**。本文档不会有「已完成」的一天，它是提示词这条线的台账和规则总表，每发现一次模型行为问题就在第 6 节记一条，规则落到第 5 节。
- 范围：`cmd/pigo-server` 的提示词组装、内置工具描述的覆盖、`tools.yaml` 的 naming profile、场景提示词。不改上游 `internal/`（`internal/runtime/prompt.go` 已有的参数尽量用起来，不为此改它的签名）。
- 立场：**提示词是产品的一部分，不是调试时的临时贴纸**。每一条规则要有案例出处、要能被推翻、要能被验证。
- 当前批次：P3（知识截止声明）**已实现**。查询词纪律（R2～R4）及其所需的 P1 机制**本轮不做**，理由见第 5 节。

## 1. 为什么这是长期任务

三条结构性原因，决定了它不可能一次做完：

1. **pigo 跑的是任意第三方模型。** Claude Code 只伺候自家后训练过的模型，很多行为（搜索纪律、事实核查、不臆造）是训练里带的，提示词可以很短。pigo 通过代理接任意模型，这些模型没经过那轮调教，**别家放在训练里的东西，我们必须显式写进提示词**。这是补位，不是抄作业。
2. **每加一个能力就欠一笔提示词债。** 联网搜索、沙箱、子代理、场景都是 pigo 自己加的，加的时候只加了工具，没有配套的行为约束。已经暴露的是搜索（第 6 节 C1），没暴露的不代表没有。
3. **模型会换。** 今天是 hy3，明天可能是别的。每个模型的毛病不同，需要按模型记录、按模型下药（naming profile 已经支持按模型覆盖工具描述）。

## 2. 现状盘点

### 2.1 一次会话的 system prompt 由什么拼成

| 顺序 | 内容 | 来源 | 谁能改 |
| --- | --- | --- | --- |
| 1 | `You are pigo, a helpful coding agent…` + todo 指引 + task 指引 | `internal/runtime/prompt.go` 的 `DefaultBaseInstruction` | 改上游代码（不做）或由 `BaseInstruction` 覆盖 |
| 2 | `Environment:` 工作目录 / OS / **Date** | 同上 `BuildSystemPrompt` | 同上 |
| 3 | 工作区里的 `AGENTS.md` 链 | 用户工作区 | 用户自己 |
| 4 | `AppendInstructions` | `PromptConfig` 参数 | **服务端从未传过，口子空着** |
| 5 | `<available_skills>` | skills 目录 | 部署方 |
| 6 | 工作区路径虚拟化改写 | `hostloop.go` 的 `virtualizeWorkspacePrompt` | 改代码 |
| 7 | 沙箱工具链说明 | `hostloop.go` 的 `toolPromptNote` | 改代码 |
| 8 | 场景提示词 | `scenes.go` 的 `sceneSystemPrompt` | **管理员在设置页改** |

实测渲染出来 **1947 字符**（空工作区、无场景、含两条沙箱工具链）。工具描述则来自各工具的 `Description()`（`internal/agenttool/*.go`），可被 `tools.yaml` 的 naming profile **按 provider/model 覆盖**（`naming.go` 的 `profileEntry.Description`）。

### 2.2 四家横向对比

事实来源：Claude Code = 本会话内直接观察自身提示词与工具描述；pi = 本地检出 `~/project/pi`；codex = 本地检出 `~/project/codex/codex-rs`（openai/codex 的 fork，含 fork 特有子系统）。

| | Claude Code | pi | codex | **pigo** |
| --- | --- | --- | --- | --- |
| 提示词本体 | — | 运行时拼段 | 每个模型一份完整独立文本，编译期内嵌在 `models-manager/models.json` 的 `instructions_template` | 运行时拼字符串 |
| 篇幅 | 中 | 中 | 12.9k–21.3k 字符/模型 | **1947 字符** |
| 结构 | Markdown 标题 | XML 命名段 | 分层：长规则 Markdown，状态数据 XML | 纯拼接 |
| 日期 | 有 | **无** | 有（`<current_date>` + 独立 `<current_time_reminder>` 带时区） | 有 |
| **知识截止** | **有** | 无 | **无** | 无 → **P3 要补** |
| 工作目录 | 有 | `<cwd>` | `<cwd>` | 有 |
| git 状态 | 有（reminder 里带） | 无 | 无 | 无 |
| 联网工具 | 有 | **无** | 有（`web.run`，描述 7507 字符） | 有 |
| **查询词纪律** | 训练里带，提示词没写 | — | **没有** | 没有 |
| 行为规则段 | `# Delivering work` 等 | `<rules>`（工具贡献） | base instructions 正文 | **一条都没有** |

两个结论直接决定了本文档的做法：

- **知识截止：四家只有 Claude Code 声明。** codex 注入了日期、时间、时区、cwd、沙箱模式、审批策略，唯独没有一句「你的训练数据早于今天」（全仓库 `cutoff` 命中全是 fork/revert 的序号，已核）。
- **查询词纪律：四家零家写进提示词。** codex 的 `web.run` 描述有「什么情况必须联网」「引用不超过 25 词」，但关于查询词只有效率约束（一次最多 4 条 query）。所以 R2 **没有先例可抄，是我们自造的规则**，必须靠回归用例验证才敢上。

### 2.3 三家的「会话中途注入信息」机制

同一个问题的三种解法，pigo 要选一种：

| | 做法 | 代价 |
| --- | --- | --- |
| pi | `sections` + `diffSystemPromptSections`：给 system prompt 分命名段，逐段替换 | **改 system prompt 任何一段都会把整个前缀缓存作废** |
| Claude Code | system prompt 不动，往消息流里插 `<system-reminder>` | 缓存全保；但片段无标识，历史里会堆积 |
| **codex** | `ContextualUserFragment` trait（`context-fragments/src/fragment.rs:64`）：`role()`（通常是 `developer`）+ `content_kind()` 稳定分类 + `markers()` XML 开闭标签 + `body()`，追加成独立消息；**`type_markers()` / `matches_text()` 让注入过的片段能在历史里被认出来** | 需要一层抽象；换来可去重、可替换、可剥离 |

**pigo 应当走 codex 那条。** 我们的 transcript 是落盘的，没有 marker 匹配的话，「当前时间是…」这类片段会在历史里堆积几十份，既占上下文又误导模型。

## 3. 原则

1. **内容与机制分开。** 能靠配置（tools.yaml / 设置页）解决的，不改代码；确实缺机制的才动代码，且一次把口子留够。
2. **条款挂在能力上。** 规则写在工具或功能上，工具不在工具集里（被 `-tools` 过滤、被场景收窄），规则自动消失。
3. **只写模型做不到的。** 提示词占上下文、也稀释注意力。模型本来就会的不写；写了没效果的删掉。
4. **每条规则都有出处。** 第 5 节每条规则必须指向第 6 节的一个案例编号。来历不明的规则不许进。
5. **system prompt 在会话内不可变。** 它在请求最前面，改任何一段都让前缀缓存全废。会变的信息一律走消息流注入（第 2.3 节）。唯一例外是模型切换——那一刻缓存本来就换了一套，重建反而免费。
6. **不改 `internal/`。** 上游已有的参数（`AppendInstructions`、`BaseInstruction`、`Now`）用起来；缺的能力在 `cmd/pigo-server` 里补。

## 4. 机制改造

### P3 知识截止声明（本轮实施）

**数据**：`modelParam`（`context_params.go`，已有 `contextWindow` / `compactPct`）新增 `KnowledgeCutoff string`，按 `priceKey(provider, model)` 定位，和窗口覆盖同一套增删改接口。格式 `YYYY-MM` 或 `YYYY-MM-DD`，规范化成 `YYYY-MM`；不得晚于今天；`modelParam.validate()` 现有的「三项至少填一项」规则要一并放宽。

**解析**：新增 `func (s *apiServer) knowledgeCutoff(providerName, model string) string`，与 `contextParams` 同构——查覆盖，查不到返回空。**不做内置的模型 cutoff 对照表**：那张表本身会过时，正是本文档要治的病；而且 pigo 面对的是任意代理模型，表注定不全。

**文案**（英文，与提示词其余部分一致）：

- 配置了：
  ```
  - Knowledge cutoff: 2026-05. Your training data ends there, before today's date above.
    Anything released since is unfamiliar to you: not recognizing a name means your
    knowledge is stale, not that the thing does not exist — check before you assert.
  ```
- 未配置（**默认仍然输出**）：
  ```
  - Your training data ends well before today's date above. Anything released since is
    unfamiliar to you: not recognizing a name means your knowledge is stale, not that the
    thing does not exist — check before you assert.
  ```
  理由：C1 的病根是「不认识 ⇒ 不存在」这个推理没被打断，有没有准确日期是次要的；而绝大多数第三方模型查不到准确 cutoff，只对配置过的模型生效等于绝大多数会话什么都得不到。

**注入位置**：插在 `Environment:` 块里 `- Date:` 那一行之后。用锚点字符串 `"- Date: " + ts.Format("2006-01-02")` 定位（`PromptConfig.Now` 传固定时间戳，锚点因此可精确构造），找不到锚点时退化为追加到基础提示词末尾。**必须有一个单元测试断言走的是锚点分支**，这样上游改了环境块格式是测试失败，而不是功能静默失效。已用临时用例验证渲染效果正确（AGENTS.md 段、工作区虚拟化段都不受影响）。

**三个注入点**：

| 点 | 位置 | 说明 |
| --- | --- | --- |
| A 主会话建环 | `hostloop.go` `ensureHostLoop` | 依赖：cutoff 要按 **resolved provider** 查，而现在 `BuildSystemPrompt` 在 `resolveProvider` **之前**，需调序 |
| B 模型中途切换 | `hostloop.go` `applyHostConfig` | 现状：它刷新 model/provider/thinking/streams，**不碰 `agentCtx.SystemPrompt`**，切模型后 cutoff 会停留在旧模型的。按 `(provider, model)` 记录提示词建给了谁，变了就**只替换那一行**（`swapCutoffNote`）。不整段重建：`BuildSystemPrompt` 每次都会重读工作区的 `AGENTS.md`，而那个工作区模型自己能写文件——会话的指令不该因为换了个模型就被换掉，场景用快照冻结正是同一个道理。找不到原文时才回退到重建 |
| C 子代理 | `subagent.go` `runSubagent` | 现状：子代理的 `taskSystemPrompt` **连日期都没有**。子代理可以指定自己的模型，所以要按子代理实际用的 `(provider, model)` 查，并补一个最小环境块（日期 + cutoff）。其余（工作区、沙箱说明）本轮不动 |

### P1 工具自带提示词条款（**暂缓**）

原计划：给工具挂行为条款，随工具集出现/消失。**本轮不做**，因为它服务的 R2～R4 已暂缓。

落点结论保留备用：条款应挂在**工具描述**上，不是单拉一段 system prompt。理由是 pigo 的提示词里**根本没有工具清单段**（不同于 pi 的 `<tools>`），模型对每个工具的全部认知就是 API tool 声明里的 `description`；把规则拼成独立一段等于放到离工具最远的地方。实现路径：在 `cmd/pigo-server` 加一个 `describedTool` 包装器（同 `countedTool` 的路子）给内置工具的 `Description()` 追加文字，不动 `internal/`；`tools.yaml` 的扩展工具直接写进自己的 `description`；按模型差异化走已有的 naming profile 覆盖。

### P2 附加提示词的口子（管理员可配）

- `settings` 新增 `PromptAddendum`（全局，管理员在设置页编辑，Markdown，有长度上限），经已有的 `PromptConfig.AppendInstructions` 传下去，**不改 `internal/`**。
- 用途：部署方注入自己的常量事实（本地模型清单、内网服务地址、当地口径），以及临时行为补丁——新规则先在这里试，验证有效再固化。
- **只放部署级常量**。会变的信息不许进 system prompt（原则 5），走 P4。

### P4 时效信息的消息流注入（原「抄 pi 的分段」，已推翻）

原写法说分段「缓存友好」，是反的：system prompt 在请求最前面，改任何一段前缀缓存全废，分不分段都一样。正确做法是 **system prompt 保持不可变，时效信息追加到消息流**，并抄 codex 的 marker 设计（`markers()` + `matches_text()`）做去重与剥离，避免落盘的 transcript 里堆积重复片段。

pigo 已有半个现成的：场景单次命令 (`sceneCommandMessage`) 就是把提示词展开进本轮用户消息。等出现第一个真实需求（会话中途要告诉模型新情况）再做。

### P5 回归评测（见第 7 节）

## 5. 规则台账

| 编号 | 规则文案 | 挂在 | 出处 | 状态 |
| --- | --- | --- | --- | --- |
| R1 | `Knowledge cutoff: <YYYY-MM>. Your training data ends there, before today's date above. Anything released since is unfamiliar to you: not recognizing a name means your knowledge is stale, not that the thing does not exist — check before you assert.` | 环境块（P3） | C1 | **本轮实施** |
| R2 | `Keep the user's literal terms in your first query — proper nouns, product codes, model names, version numbers, verbatim. Refine by ADDING context words; never replace an identifier the user gave you with one you believe is the correct spelling.` | `websearch`（P1） | C1 | **暂缓** |
| R3 | `If a name looks wrong or unfamiliar, search it verbatim on its own to check whether it exists, before treating it as a typo.` | `websearch`（P1） | C1 | **暂缓** |
| R4 | `When a search result contradicts an assumption you made before searching, the result wins. Open it with webfetch and read it before you repeat the assumption.` | `websearch`（P1） | C1 | **暂缓** |
| R5 | `Cite the pages you used as markdown links at the end of an answer built from search results.` | `websearch`（P1） | C1 | 候选 |

**R2～R4 暂缓的理由**（2026-09-20 决定）：一是先看 R1 单独能解决多少——C1 的四层根因里，第 1 层（不认识就断定不存在）是 R1 的靶子，打掉它后面三层可能不攻自破；二是四家 harness 无一写过此类规则，属自造，得有回归用例垫底才敢上，否则是在正式部署上盲调；三是提示词越短越好，能少一条是一条。等 R1 上线后重跑 C1 用例，仍复发再加。

废弃的规则不删行，改状态并写明原因——避免后人重新发明一遍。

## 6. 案例台账

### C1 · 2026-09-20 · 模型用记忆改写查询词，把真实存在的新模型判定为不存在

- **证据**：9987 会话 `659e860126e9a3427f953a43a40ae2e1`，transcript 第 21～33 条。
- **现象**：用户问「S4000能推理qwen3.8 27b吗」。模型 thinking 里先断定 *"there's no Qwen3.8 … they probably mean Qwen3-27B or maybe a typo"*，随后两次搜索的查询词是 `MTT S4000 vllm-musa Qwen3 32B 27B 推理 支持` 和 `Qwen3-27B OR Qwen3.6-27B 参数 显存 推理 要求`——**用户原词 "qwen3.8" 一次都没进过查询**。更糟的是第二次搜索的第 2 条结果标题就是「Qwen3.8-27B VRAM 需求：你真正需要多少硬件」（orcarouter.ai，2026-08-12），模型把这条里的 27GB/48GB 数字抄进了答案、署名给了 Qwen3.6，开头照旧写「目前没有 Qwen3.8-27B 这个官方模型」。用户手动要求「直接搜 qwen3.8 27b」后一搜即中。
- **根因**（四层）：
  1. 模型训练数据早于 Qwen3.8 发布，把「我不认识」当成「不存在」；
  2. **替换式**改写查询词，把唯一的新信息入口用过时先验堵死；
  3. thinking 里已出口的判断抵抗反证，后续搜索退化为找支持材料；
  4. harness 缺位：环境块只给日期不给 cutoff、`websearch` 描述对查询词写法只字未提、基础指令是纯 coding agent 口径、结果只给 snippet 而模型从未 webfetch 那条反证页面。
- **对应规则**：R1（本轮）；R2 R3 R4 暂缓。
- **复发情况**：R1 已上线（2026-09-20），真实模型回归待跑，判定标准见 `spec/prompt-cases/c1-unknown-model-name.md`。

## 7. 回归评测

提示词改动**没有单元测试能覆盖行为**——它改的是模型行为，不是代码分支。所以分两层：

- **Go 测试只保证结构**：规则文案在该出现时出现、不该出现时不出现、插在正确位置（锚点分支被走到）、未配置时的退化文案、API 往返。这些必须有。
- **行为验证靠回归问题集**：`spec/prompt-cases/` 下每个用例包含输入、**可观察的期望行为**、判定方式。可观察很重要——「回答得更好」不可判定，「第一次 websearch 的查询词包含 qwen3.8」可判定，读 transcript 就行。
- C1 的用例：输入 `S4000能推理qwen3.8 27b吗`；判定①最终回答是否出现「没有这个模型」类否定（R1 的直接靶子）；判定②首次 `websearch` 的 query 是否含 `qwen3.8`（R2 的靶子，本轮不做，作为对照观察）。
- **执行**：每次提示词改动后，用 2～3 个线上常用模型各跑一遍，人工读 transcript 判分，结果记进第 6 节。先手工，用例过十个再脚本化。
- **不用假模型跑行为用例**：假模型的行为是我们写死的，验证不了任何东西；它只用于验证结构。

## 8. 按模型的差异记录

| 模型 | 观察到的行为 | 措施 | 出处 |
| --- | --- | --- | --- |
| hy3（codebuddy-proxy） | 思考链长，结论出得早且抗反证；替换式改写查询词 | R1（R2～R4 暂缓） | C1 |

## 9. 开发计划

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| P3 | `modelParam.KnowledgeCutoff` + 解析 + R1 文案 + 三个注入点（主会话/模型切换/子代理）+ 管理界面 | **已实现**（2026-09-20），9988 上端到端验证 |
| P5 | `spec/prompt-cases/` 用例集与人工回归流程 | 用例已落（C1），**真实模型回归待跑** |
| P1 | 工具条款机制（`describedTool`）+ R2～R4 | 暂缓，看 R1 效果 |
| P2 | `PromptAddendum` 设置项 | 未开始 |
| P4 | 时效信息的消息流注入 + marker 去重 | 等真实需求 |

## 10. 不做（第一版）

- 不做提示词的 A/B 或自动评分：样本量不够，判分成本高于收益。
- 不做用户级的提示词自定义：只有全局（管理员）和场景两级。
- 不做内置的模型 cutoff 对照表（理由见 P3）。
- 不为了提示词去改 `internal/runtime/prompt.go` 的结构。
- 不把规则写成中文：工具描述和基础指令都是英文，混排伤弱模型。

## 11. 代码索引

| 位置 | 内容 |
| --- | --- |
| `internal/runtime/prompt.go` | 上游的提示词组装：基础指令、环境块、AGENTS.md、`AppendInstructions`、`Now`、skills（不改） |
| `cmd/pigo-server/hostloop.go` | `ensureHostLoop`（注入点 A）、`applyHostConfig`（注入点 B）、`virtualizeWorkspacePrompt`、`toolPromptNote` |
| `cmd/pigo-server/subagent.go` | `taskSystemPrompt`、`runSubagent`（注入点 C） |
| `cmd/pigo-server/context_params.go` | `modelParam`、`contextParams`；P3 的 `KnowledgeCutoff` 与解析 |
| `cmd/pigo-server/naming.go` | 按 provider/model 覆盖工具名与**描述**；第 8 节的下药位置 |
| `cmd/pigo-server/tools_config.go` | `tools.yaml` 解析；P1 的条款字段 |
| `cmd/pigo-server/settings.go` | P2 的 `PromptAddendum` |
| `cmd/pigo-server/knowledge_cutoff.go` | R1 的文案与插入（`cutoffNote`、`withKnowledgeCutoff`、`subagentEnvironment`） |
| `spec/prompt-cases/` | 回归用例集（P5） |

## 12. 参考

- **pi**（`~/project/pi`，`earendil-works/pi`）：`packages/coding-agent/src/core/system-prompt.ts` 的分段结构、`buildRules` 的工具贡献、`diffSystemPromptSections`。
- **codex**（`~/project/codex/codex-rs`，openai/codex 的 fork）：`context-fragments/src/fragment.rs` 的 `ContextualUserFragment`；`models-manager/models.json` 的按模型独立提示词；`ext/web-search/web_run_description.md`。
  - 坑：`core/` 下那 5 个 `.md` 提示词文件（含最出名的 `gpt_5_codex_prompt.md`）**已无任何 Rust 代码 `include_str!` 引用**，是历史遗留副本，照着它们研究 codex 会研究到死文件。
- **Claude Code**（本会话内直接观察）：cutoff 与今天日期成对给出；`WebSearch` 工具描述里重复当前月份并强制 `Sources:`；对已知会过时的领域（自家模型清单）直接在环境块里灌 ground truth。
