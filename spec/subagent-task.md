# 子代理（task 工具）

状态：已实现（P1～P3），已在 9988 上用假模型走通

## 1. 目标

让 Web 版的模型能把一件独立的活交给子代理去做：子代理有自己的上下文和工具，跑完把一份报告交回主线。主线因此可以并行做调研、分头查资料，而不用把中间过程全塞进自己的上下文。

pigo 本体已经有这套机制（`internal/runtime/task.go` 的 `task` 工具、`SubAgentTool`、嵌套防护 `BuiltinToolsExcept`、并发信号量），CLI 在 `run.SetupEnv` 里接上了；Web 版自己拼工具集（`hostTools`），一直没接。本需求就是把它接进 Web 版，并按 Web 版的规矩（计量、密钥、沙箱、场景）重做装配。

## 2. 为什么不能照抄 CLI 的装配

| CLI 的做法 | Web 版的问题 |
|---|---|
| 子代理凭据用 `provider.NewCredentialStore(nil)` + `--api-key` 覆盖 | Web 版不读环境变量，Key 来自加密存储、按用户解析（`apiKeyFunc` → 个人 Key → 公共 Key）。照抄会让每个子代理都没有 Key |
| 子代理 stream 用 `provider.StreamFnFromProvider(prov)` 裸流 | 绕开 `meterStreams` 的四层包装（命名映射 → conversation_id → 计量 → 停滞守护），子代理的 token 和费用不入账 |
| 子工具集是 `BuiltinToolsExcept(cwd, …)`，以进程工作目录为根 | Web 版的工具绑在会话的沙箱容器和工作区上，还要经过扩展工具替换和 `-tools` 过滤 |

**解法**：子代理的 `RunConfig` 从父会话那套装配里派生，而不是照 CLI 新建。`GetAPIKey` 原样复用（密钥解析就对了），stream 仍旧过服务端自己的包装链（计量就对了）——计量器是从 context 里取 turn 的（`turnFrom(ctx)`），子代理跑在工具调用的 ctx 里，费用自然归到同一轮。只有换了模型/服务商时要按子代理自己的重搭这条链（4.2）。

## 3. 已确认的决定

| 问题 | 决定 |
|---|---|
| 子代理工具范围 | 按**子代理类型**（见 12、13 节）：`general-purpose`（父会话全部工具减 task，缺省）或 `explore`（再减 write/edit） |
| 界面展示 | 活动日志里折叠一条，可展开看子代理的工具调用和最终报告 |
| 子代理模型 | 调用时可指定，必须在管理员勾选的白名单内；不指定则用父会话的模型 |
| 上限 | 管理员在 部署 页配：总开关、并发数、单个子代理最大步数、单个子代理超时 |
| 结果回收 | 只把最终报告作为工具结果返回，超长截断并标注；中间过程不进主上下文 |
| 场景会话 | 不允许 task（场景是受控环境） |
| 用量 | 单独标 `kind = "subagent"`，费用仍归当前这一轮、这个用户 |
| 停止 | 主线停止时子代理立即一起取消，已产生的费用照常记账 |

## 4. 服务端设计

### 4.1 工具定义

```go
// cmd/pigo-server/subagent.go
func (s *apiServer) taskTool(managed *managedSession) agentcore.AgentTool
```

用 `runtime.NewSubAgentTool(runtime.SubAgentSpec{...})`，name `task`，schema 在上游的基础上加两个可选字段：

```json
{
  "description": "3-5 个字的任务简述，用于状态显示",
  "prompt":      "交给子代理的完整任务（它没有本会话的上下文）",
  "subagent_type": "general-purpose | explore，缺省 general-purpose",
  "model":       "可选，必须在管理员允许的清单里；不填用当前会话的模型"
}
```

上游的 `taskDescription` / `taskSystemPrompt` 沿用，只在描述后面补一句「子代理与你共用同一个沙箱容器和工作区」。

### 4.2 子 RunConfig 工厂

每次 spawn 现取一份：

- `Model` / `Provider`：`model` 参数解析后的结果（`resolveModel`），否则父会话的。
- `Stream`：**按子代理自己的服务商现搭一条链**，不能直接复用父会话那条：

  ```go
  stream := s.nameStream(s.conversationStream(provider.StreamFnFromProvider(childProv), childProviderName, convID), childProviderName)
  cfg.Stream = guardStream(s.meter.wrap(stream, childProviderName, "subagent"), idle)
  ```

  三个包装都跟服务商/模型绑定：`nameStream` 按（服务商, 模型）选命名 profile，`conversationStream` 按这个自定义服务商是否开了 conversation_id 决定带不带，计量要记子代理真正用的服务商和模型。子代理与父会话同模型时结果与复用父链一致，换了模型才有分别。
- `GetAPIKey`：父会话的 `s.apiKeyFunc(userID)`，原样复用（按这个用户解析：个人 Key → 公共 Key）。
- `Registry`：子工具集（下一节）。
- conversation_id：子代理用 `sessionID + "#" + toolCallID`。主线的前缀缓存按会话 id 复用，子代理的前缀完全不同，混用会把上游（CodeBuddy 反代那条链路）的缓存冲掉。
- 上下文窗口与压缩：按**子代理的**服务商和模型取 `contextParams`，填进子 `RunConfig` 的 `ContextWindow` / `Compaction`。子代理有自己的上下文，长任务同样会顶到窗口，不设就没有自动压缩。
- 开跑前先查 Key：对子代理的服务商跑一次 `errNoProviderKey(userID, childProvider)`，没有 Key 直接把错误当工具结果返回，不浪费一次调用（和主线开轮前的检查一致）。

### 4.2.1 记账归属

`turn.keySource` 是开轮时按**父会话的服务商**算一次的（个人 Key / 公共 Key，决定 `billedTo` 算谁头上）。子代理换到另一个服务商时可能不同：父用公共 Key、子模型只有这个用户自己的 Key。

改法很小：给计量器的 `call` 结构加一个 `keySource` 字段，非空时覆盖 `turn.keySource` 写进账本条目。子代理的调用按自己的来源记，费用归属才准。

### 4.3 子工具集

1. 先拿父会话的工具集 `hostTools(managed)`（已含扩展替换、`-tools` 过滤、场景过滤）。
2. 去掉 `task`（嵌套防护：子代理不能再 fan out）。
3. 按类型去掉它不该有的工具（`explore` 去掉 write/edit）。
4. 每个工具包一层 `subagentToolWrapper{inner, run, taskCallID, steps}`：
   - 把调用上报给当前 turn 的活动日志（工具名、detail、状态、耗时），挂在这条 task 记录下面；
   - 计步，超过管理员配置的上限后不再执行，返回「已达子代理步数上限，请给出目前的结论」让它收尾。

这样不动 `internal/`，就能拿到子代理的完整调用明细——上游的 `SubAgentProgressEvent` 只给活动名和 token 估算，不够展开用。

### 4.4 限额与并发

- 并发：`runtime.NewSubagentSemaphore()` 换成按配置容量创建的 channel，一个会话共用一个（放在 `managedSession` 上），跨会话不共享。
- 步数：由 4.3 的包装器计数。
- 超时：`SubAgentSpec` 的执行在父 ctx 下，工厂里再包一层 `context.WithTimeout`；超时返回已有进展。
- 取消：父 turn 停止 → ctx 取消 → 上游保证子代理跟着断；容器里的命令由 `runInSession` 的 ctx 一起断。

### 4.5 不注册 task 的情况

- 管理员关掉总开关；
- 场景会话（`managed.meta.Scene != nil`）；
- `-tools` 列举式配置里没有 `task`（`all` 视为包含）。

### 4.6 与主线轮次的关系

- **步数**：子代理的工具调用不计进父轮的 `maxSteps`。父轮的步数由 `turnRun.observe` 数主循环的事件；子代理的调用走我们自己的包装器上报活动日志，不经过 `observe`，天然不重复计数。子代理有自己的步数上限（4.4）。
- **停滞提示**：`RunNotice` 会显示「正在执行 task（已 x 分）」，子代理跑长任务时这正是想要的效果，不额外改。
- **transcript / 历史**：父会话的 transcript 里只有 `task` 这次工具调用和它返回的报告（和普通工具一样），子代理的内部消息不落盘。所以**展开看到的调用明细是本轮实时的**，刷新页面后历史里只剩这条 task 调用和报告。第一版接受；要留痕就把报告写进工作区文件（模型自己可以这么做）。
- **`-tools` 与命名**：`task` 要加进 `hostToolNames`，这样 `-tools a,b,c` 的列举式配置能点名它，命名 profile 能给它改名（某些模型偏好别的叫法），运行状态页也能列出来。

## 5. 活动日志与前端

- 新增活动项 `kind: "subagent"`：`description`、`subagent_type`、`model`、状态（running/ok/error）、耗时、步数、子调用列表（复用现有 tool 项的结构）、最终报告的前若干字。
- 折叠一条：`◇ task：调研 A 方案 · 3 步 · 12s`，展开后是子代理的工具调用时间线和报告。并行的多个子代理各占一条。
- 结果超长截断：上限 8000 字符，截断时在末尾标注「（已截断，子代理原文更长）」。

## 6. 管理端

部署 → 公共配置新增一张卡「子代理」：

| 项 | 默认 | 说明 |
|---|---|---|
| 启用 task 工具 | 开 | 关掉后所有会话都没有这个工具 |
| 并发上限 | 4 | 一轮里同时跑的子代理数 |
| 单个最大步数 | 20 | 超过就让它收尾 |
| 单个超时 | 5 分钟 | 壁钟上限 |
| 可用模型 | 空 | 勾选清单；空表示只能用父会话的模型 |

模型清单的候选口径与「默认模型」一致：公共模型 + 已配 Key 的内置模型 + OpenRouter 免费。

用量页的「类型」筛选加 `subagent` 一项。

## 7. 测试计划

- 单元：类型过滤（explore 有 bash、没有 write/edit）；未知类型报错并列出可用类型；嵌套防护（子集永远没有 task）；模型白名单（不在单中报错、不填回落父模型）；步数上限触发收尾；场景会话和总开关关闭时不注册；子代理换服务商时命名 profile、conversation_id、keySource 都按子代理的算。
- 端到端（fake 模型）：主线调用 task → 子代理调工具 → 返回报告；账本里这一轮有 `kind=subagent` 的调用且归同一 turn；子代理的 Key 解析走的是会话用户的；停止主线时子代理立刻结束。
- 并发：一条消息里两个 task 并行，信号量为 1 时串行执行，结果都回到主线。

## 8. 分阶段

| 阶段 | 内容 |
|---|---|
| P1 服务端 | `subagent.go`：工具、工厂（含换服务商时重搭 stream 链）、子工具集、包装器、限额；计量器 `call` 加 keySource；`task` 进 `hostToolNames`；设置项与接口；测试 |
| P2 前端 | 活动日志的 subagent 项与展开；部署页「子代理」卡；用量页 kind 筛选 |
| P3 验证 | 9988 上用假模型走通，截图；更新本文档状态 |

## 9. 已知取舍

- 并行的子代理和主线共用**同一个沙箱容器和工作区**：容器里只有一个 shell，bash 和命令工具会排队；同时改同一个文件会互相覆盖。第一版不做隔离，提示词里写明。
- 子代理拿不到会话历史，任务描述必须自足——这是上游的设计，保持不变。
- 子代理不继承场景提示词（场景会话本来就不给 task）。

## 10. 实施细化（编码前核实代码后的结论）

### 10.1 不能用上游的 SubAgentTool，自己实现

`runtime.SubAgentSpec` 的 `NewRunConfig func() RunConfig` **不带参数**，`Tools` 也是建工具时定死的；上游 `subAgentArgs` 只解析 prompt/description。也就是说「调用时指定子代理类型 / model」这条需求，上游那层承载不了。

所以在 `cmd/pigo-server/subagent.go` 自己实现一个 `task` 工具，直接用 `runtime.StartRun` + `runtime.DrainStream` 驱动子循环——`runHostLoop` 用的就是这套 API。代价是多写约 150 行，换来：每次调用自己的工具集和模型、子步骤能进活动日志、步数/超时/截断都在手里，且 `internal/` 一行不改。

### 10.2 子代理的事件怎么进活动日志

父循环发的工具事件形如 `streamEvent{Type:"tool", Phase:"start|output|end", ID, Tool, Detail}`，由 `turnRun.foldToolLocked` 折进 `activity`。

- `streamEvent` 加一个 `ParentID`；子代理的工具事件带上 task 这次调用的 id。
- `activityItem` 加 `Children []activityItem`；`foldToolLocked` 见到 `ParentID` 就折进对应条目的 `Children`（start/output/end 逻辑与顶层相同，抽成一个对切片操作的辅助函数）。
- 子代理的叙述文字不能进父轮的 `text`（那是父模型的回答），在子循环的 `OnTurnEnd` 里作为一条 `Kind:"text"` 的子项发出。
- 子代理运行时，用工具的 `onUpdate` 把它最近的动作回填到 task 这条的 `output`，运行中就能看到它在干什么。
- 子代理自己的压缩事件第一版不展示（`CompactionEvent` 忽略），避免把折叠层次再加一层。

前端：`ToolArgs` 加 `children`，`ToolLine` 在自己下面缩进渲染子行；`cutOff` 也要处理子项（父轮结束时仍在 running 的子调用标未完成）。客户端自己也折叠事件（`api.ts` 的 `foldActivity`），同样要认 `parentId`。

### 10.3 子代理的模型与计费

- `model` 参数先查白名单（管理员勾选的 id 列表），再 `resolveModel(userID, model, "")` 拿到 provider；不填就用父会话的模型和服务商。
- 开跑前 `errNoProviderKey(userID, childProvider)`，没 Key 直接把错误当工具结果返回。
- stream 链按子代理的服务商现搭：`guardStream(meter.wrap(nameStream(conversationStream(raw, childProvider, sessionID+"#"+callID), childProvider), childProvider, "subagent", childKeySource), idle)`。
- 计量器的 `call` 加 `keySource`，非空时覆盖 `turn.keySource` 写进账本（子代理换服务商时归属才对）。
- 子代理触发的压缩调用也记 `kind="subagent"`：那是子代理的成本，和它归在一起比混进 compaction 更好查。
- `ContextWindow` / `Compaction` 按子代理的服务商和模型取 `contextParams`。

### 10.4 子工具集与步数

- 每次 spawn 现调 `s.hostTools(managed)` 取一份新实例（含扩展替换、`-tools` 过滤、场景过滤），去掉 `task`；再去掉这个类型不该有的工具。现取而不是共享父会话那份，是因为 read/write/edit 共享一个快照记录器，子代理有自己的一份更干净。
- 每个子工具包一层计步壳：超过上限后不再执行，返回「已达子代理步数上限，请立即给出结论」。这比中途取消好——取消就拿不到报告了。
- 并发用会话级的门（mutex + cond 计数），每次 spawn 现读配置里的上限，管理员改了立即生效；用固定容量的 channel 做不到这点。

### 10.5 边界

| 情况 | 处理 |
|---|---|
| 主线被停止 | 子 ctx 由父 ctx 派生，自动取消；task 返回错误，活动日志里它和它的子调用都标未完成 |
| 子代理超时 | `context.WithTimeout`，返回已有文本 + 「超时，以下是中断前的进展」 |
| 报告超长 | 8000 字符截断，末尾标注 |
| 子代理一个工具都没调就回答 | 正常，返回报告即可 |
| 两个 task 并行 | 各自的 callID 不同，折叠互不干扰；容器里的 bash 会排队（沙箱只有一个 shell） |
| 白名单为空 | 只能用父会话的模型；传了 model 就报错说明 |
| 总开关关闭 / 场景会话 | 根本不注册这个工具，模型看不到 |

## 11. 实现与验证记录

代码：`cmd/pigo-server/subagent.go`（工具、设置、并发门、子工具集、计步壳、接口）、`hostloop.go`（注册）、`turnrun.go`+`activity.go`（子项折叠）、`billing_meter.go`（按调用记 keySource）、`main.go`（`ParentID`、路由、会话级门）、`tools_config.go`（`task` 进 `hostToolNames`）；前端 `activity.tsx`（嵌套展示）、`admin.tsx`（子代理卡）、`billing.tsx`（类型筛选）、`api.ts`。

实现时改掉的两处判断：

1. **子代理的叙述从流式消息事件取，不用 `OnText`。** `DrainStream` 的 `OnText` 在一轮结束时才刷，晚于这一轮的工具事件，日志里就会错位一条。改成 `MessageUpdateEvent` 的当前文本，在工具开始时落下。
2. **工具被循环拒绝（参数不合法）时没有 start 事件**，待发的叙述会被下一条覆盖，所以结束事件里也补一次冲刷。

验证：单元测试覆盖工具集与嵌套防护、步数上限、模型白名单、并发门（含取消唤醒）、设置接口；端到端测试跑完整一轮，断言子代理的调用挂在 task 条目下、账本里 `kind=subagent` 且与主线同轮。9988 上用假模型实测：主线派发 → 子代理两次调用 → 报告回到主线，活动日志按「叙述 → 调用」的顺序展开，用量页按「子代理」筛得到这些调用。

测试服务器也配了沙箱（`newTestServer` 用宿主机的 bwrap），测试里的工具集就是部署同款，不再为测试单独裁剪一份。

## 12. 从「权限档位」改成「子代理类型」（2026-09-20）

第一版把工具范围做成调用参数 `scope`（readonly / full，缺省 readonly）。9987 上第一次真机派发就选错了：任务写的是「在 /workspace 执行 `ls -la`」，却传了 `scope: "readonly"`，子代理没有 bash，只能改用 find，还在报告里说明「当前环境没有 shell 工具」。

问题不在模型，在接口：我们要求它在写 prompt 的同时判断「这活要不要 shell」。参考通用 harness，没有一个这么做：

| harness | 子代理的工具范围由什么决定 |
|---|---|
| Claude Code | **子代理类型**（`Task(subagent_type=…)`）。每种类型在 `.claude/agents/*.md` 的 frontmatter 里写死 `tools:`；内置的 Explore 只读、Plan 不给写入、general-purpose 全套 |
| pigo 本体（CLI） | 通用 task **完全继承**父进程的 `--allowed-tools/--disallowed-tools`（`ChildToolSet`）；skill 子代理按 SKILL.md 的 `allowed-tools`（`Skill.SubAgentSpec`）——同样是角色制 |
| Codex | 没有派发；权限是**会话级沙箱策略**（read-only / workspace-write / danger-full-access），启动时定好 |

共同点：权限是角色的固有属性或会话的既定事实，模型只判断「找谁干」——这是它判断得好的事。

所以改成角色制，与 Claude Code 的参数名对齐（模型见过这套）：

- 参数 `subagent_type`，内置两种：
  - `general-purpose`（**缺省**）：父会话全部工具减 task，能跑命令、改文件；
  - `explore`：读和查，不给编辑类工具（见 13 节）。
- 工具描述按 Claude Code 的样式列出「Available agent types」及各自适用场景，模型按任务性质挑。
- 缺省从只读改成全套：选错时代价小（多给了权限但活能干成），而缺省只读会让「要跑命令」的任务直接失败。写错类型名则报错并列出可用类型，不擅自回落。
- 活动日志的一行在非缺省类型时标出类型（`查版本 · explore`）。

后续可以把类型做成可配置的（管理员像定义场景那样写角色和它的工具清单），取值就从内置两个扩展成一份清单——和已有的场景机制能并到一起。

## 13. explore 要留着 bash（2026-09-20）

改成角色制之后，9987 上第二次真机派发是：

```json
{"description":"列出工作目录文件","prompt":"…使用 `find` 和 `ls -la` 命令…","subagent_type":"explore"}
```

选 explore 去列目录完全合理，但第一版的 explore 是 read/grep/find/webfetch/websearch 的**白名单**，没有 bash。子代理于是连跑四轮找别的路子，最后报告「无法运行 shell 命令」——账本里那一轮四条 `kind=subagent` 就是这么来的（子代理有自己的 loop，每轮一次模型调用；四轮里三轮是白费的）。

对照 Claude Code：它的 Explore 是「**除编辑类工具外都有**」，bash 是有的——在工作区里找东西本来就要 `ls`/`grep`/`rg`。白名单是我抄错了。

改法：类型不再声明「有哪些」，而是声明「没有哪些」。

- `general-purpose`：父会话工具减 task。
- `explore`：再减 `write`、`edit`。

代价说清楚：explore 仍能通过 bash 改文件（`rm`、重定向）。这是主流 harness 接受的取舍——真正的只读要靠沙箱策略（Codex 那种会话级 read-only），而不是藏掉一个工具；藏掉 bash 换来的不是安全，是子代理干不成活、白烧三轮 token。
