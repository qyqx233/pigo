# Web 版上下文压缩

状态：已实现（2026-09-19）

## 1. 现状

上下文压缩已经完整实现在 `internal/compaction`（从 pi 移植）：

1. **触发**：每轮结束后 `maybeAutoCompact`，条件是上下文 token 数 > 窗口 − `ReserveTokens`。
2. **切点**：从最新的消息往回数，保留大约 `KeepRecentTokens` 的尾部；切点不会落在工具结果上。
3. **总结**：切点之前的部分单独调用一次模型，生成结构化摘要。已有旧摘要时，改用增量更新的模板。
4. **重建**：上下文变成摘要 + 保留的尾部。

CLI（REPL/TUI）把它打开了（`DefaultCompactionSettings`，窗口 128k）。**pigo-server 没有打开**：`run.NewConfig` 既不设 `Compaction` 也不设 `ContextWindow`，所以 Web 版从不压缩，长会话会一直涨到被模型拒绝；`/compact` 在 Web 上也标着"不可用"。

服务端已经具备的：

- 摘要调用走单独的 `SummaryStream`，经过计费（`kind=compaction`）、卡顿保护和会话 ID 包装；
- `turnrun.go` 能识别 `CompactionEvent`；
- 压缩后会整体重写 transcript。

## 2. 已确认的决策

| # | 问题 | 决定 |
|---|------|------|
| 1 | 哪些模型可以设参数 | **任意模型**，按 provider + 模型名覆盖（预设、OpenRouter 免费模型、自定义模型都行） |
| 2 | 阈值怎么表达 | **窗口的百分比**：公共默认 + 按模型覆盖 |
| 3 | 窗口未知 | **用公共默认窗口**；OpenRouter 免费模型优先用其目录里的 `context_length` |
| 4 | 保留多少尾部 | **窗口的 25%，最多 20k**，不单独做配置 |
| 5 | 存储与界面 | **仿价格表**：存在 settings 文档里，不加表、不迁移；设置 → 模型 里加一个「上下文与压缩」区块 |
| 6 | 谁能改 | **只有管理员**；普通用户只读 |
| 7 | 一起做的配套 | Web 版 **`/compact`**、活动日志里的**压缩记录**、**上下文占用**显示 |
| 8 | 公共默认值 | **窗口 128k，阈值 80%** |
| 9 | 压缩后的历史 | **保留完整历史**：transcript 只追加，压缩处在网页上显示一条分隔 |
| 10 | 占用显示在哪 | **每条回复的用量行**（"上下文 45k/128k"） |
| — | 超长报错后自动压缩重试 | 本期不做（要识别各家的报错文本，可能要改 internal 的循环） |

## 3. 参数与解析

### 3.1 存储（settings 文档，和 `modelPrices` 同一处）

```go
type serverSettings struct {
	...
	// 公共默认
	DefaultContextWindow int `json:"defaultContextWindow"` // 缺省 128000
	DefaultCompactPct    int `json:"defaultCompactPct"`    // 缺省 80
	// 按模型覆盖
	ModelParams []modelParam `json:"modelParams,omitempty"`
}

type modelParam struct {
	Provider      string    `json:"provider"`
	Model         string    `json:"model"`
	ContextWindow int       `json:"contextWindow,omitempty"` // 0 = 不覆盖
	CompactPct    int       `json:"compactPct,omitempty"`    // 0 = 不覆盖
	UpdatedBy     string    `json:"updatedBy,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt,omitempty"`
}
```

校验范围：

- 窗口 4k–10M；
- 阈值 50–95%。低于 50% 会过早、过频地压缩；高于 95% 时，留给摘要的输出预算（见 3.3）不够。
- 覆盖项至少设一项，两项都为 0 就是删除。

### 3.2 解析顺序（`s.contextParams(provider, model)`）

窗口：

1. 模型覆盖的 `contextWindow`；
2. OpenRouter 目录里该模型的 `context_length`（仅 provider 为 openrouter 时）；
3. 公共默认窗口。

阈值：模型覆盖的 `compactPct`，否则用公共默认。返回值里同时带上来源（override / openrouter / default），界面用来说明。

`openrouter/free` 是个路由模型，每次调用落在哪个模型上都不一定，所以只用覆盖项或默认值。

### 3.3 换算成内核参数（不改 internal）

```
ContextWindow    = window
ReserveTokens    = window × (100 − pct) / 100   // 触发点：tokens > window × pct%
KeepRecentTokens = min(20000, window / 4)
Enabled          = true
```

`ReserveTokens` 在内核里还决定摘要的输出上限（0.8 × reserve）。128k、80% 时上限约 20k；95% 时约 5k，勉强够用，所以阈值上限定在 95%。

**生效时机**：每轮开始时（`runHostLoop` 复制 runCfg 之前）按会话当前的模型现算。管理员改了参数，从下一轮起生效，不用重载会话。

### 3.4 OpenRouter 目录

- `fetchOpenRouterFreeModels` 顺带解析 `context_length`，返回给界面（显示用）。
- 另加一个带 1 小时 TTL 的进程内缓存，供 `contextParams` 使用。拉取失败就回落到默认窗口，不阻塞这一轮。

## 4. transcript：保留完整历史

**现状**：压缩时 agent 的消息列表变成 `[摘要, 保留的尾部…]`，`saveTranscript` 发现首条消息变了，就整体重写文件，更早的消息从文件里消失，刷新页面后也就从网页上消失。

**改为**：文件是完整的、只追加的日志；模型的上下文是这份日志的一个"视图"。

- **写**：`saveTranscript` 遇到消息列表的首条是一条新的 `CompactionMessage` 时：
  1. 找出保留部分的第一条对应文件里的哪条 entry：用旧视图里消息的编码按内容匹配，从后往前找；
  2. **追加**这条压缩消息，把 `firstKeptEntryId` 合并进它的 `Details`（JSON），不改原有的 read/modified files 字段；
  3. 之后的新消息照常追加。

  `transcriptState` 记录当前视图的 entry id 列表（替代现在的 count/first/last），视图和文件就能对应起来。
- **读**：`loadTranscript` 找到最后一条压缩 entry C，重建上下文：C 的消息 + 从 `firstKeptEntryId` 到 C 之前的 entries + C 之后的 entries。多次压缩时，每条压缩摘要本身就是增量累积的，只取最后一条即可。
- **兼容**：旧文件里没有 `firstKeptEntryId` 的压缩消息（以前被重写过的），按"它之前没有别的内容"处理，行为和今天一样。
- **网页历史**（`historyMessages`）：读完整文件。压缩 entry 显示为一条分隔"上下文已压缩：总结了前 N 条消息（约 92k → 24k）"，摘要可以展开查看；之前的消息照常显示。
- 其它仍会重写文件的情况（比如编辑了第一条消息）保持现状；只有"压缩"这一种改走追加。

## 5. `/compact`

- 在 Web 上启用（从"不可用"列表移到可用命令）。不带参数，理由见 6.5 第 7 条。
- **作为一轮运行**：复用 `startTurn`，所以它同样独占会话、可以停止、受刹车约束，摘要调用计入计费。这一轮的执行体调用 `compaction.Compact`：
  - 模型用会话当前的模型；stream 用 `SummaryStream`，已经带计费和会话 ID；
  - 设置用 3.3 算出的参数，但不看阈值；
  - 结果按第 4 节写入 transcript，并发出和自动压缩相同的事件。
- 没有可总结的内容时（会话太短），回复"上下文还很短，无需压缩"。

## 6. 界面

- **活动日志**：`CompactionStartEvent` / `CompactionEvent` 转成流事件 `{type:"compaction", phase, before, after, summarized, error}`：
  - 在对话里显示成一条独立的说明，比如"⟲ 正在压缩上下文…"，结束后变成"已压缩上下文：92k → 24k（总结 N 条）"；
  - 失败时说明原因，并注明对话继续；
  - 刷新页面后由第 4 节的分隔显示。
- **用量行**：每轮结束时附带上下文占用 `{tokens, window, compactAt}`：
  - `tokens` 取这一轮最后一次调用的上下文 token 数（input + 缓存 + output）；
  - 显示为"上下文 45k/128k"，超过阈值的 90% 时变色；
  - 历史消息从 transcript 里助手消息的 usage 算出，窗口按当前参数解析。
- **设置 → 模型**：模型和它的上下文参数放在同一个列表里（管理员可编辑，其他人只读）：
  - 顶部一行显示默认值（窗口、阈值%），点「修改」原地编辑；
  - 已添加的模型列成表：模型、provider、上下文·阈值（标明默认 / 单独设置 / 目录）、范围（公共 / 个人·到期日），行内「上下文」按钮在该行下方展开编辑器（保存 / 恢复默认）；
  - 「+ 添加模型」默认折叠，展开后是紧凑的表单，可以顺带设窗口和阈值；
  - 不在列表里的模型（预设、OpenRouter 免费模型）的覆盖项列在下方「其它模型的上下文设置」，模型可以从可用模型里选，并显示目录窗口作参考。
- API：`GET /api/model-params`（登录可读），`PUT/DELETE /api/admin/model-params`，`PUT /api/admin/settings` 带公共默认。

## 6.5 编码前的细化（推演结论）

1. **transcript 视图**：`transcriptState` 改为记录当前视图里每条消息的 entry id 和内容哈希（sha256），外加文件的 entry 总数。
   - 追加判定仍然只比较首尾两条的哈希，成本和现在一样。
   - 压缩判定：首条是新的 `CompactionMessage`，并且旧视图从某个位置 k 起的整段尾部，逐条等于新列表 `[1:]` 的开头。k 从小往大找，第一个整段匹配的 k 就是切点。
   - 匹配不上时退回整体重写，打日志，并标记"已重写"。
2. **重建时跳过旧的压缩记录**：第二次压缩保留的范围，在文件里可能跨过第一次的压缩 entry C0。所以重建是 `C` + `entries[firstKept:c]` 中的非压缩消息 + `entries[c+1:]`。
3. **压缩记录里多存几项**：`Details` 合并进 `firstKeptEntryId`、`summarized`（被总结的视图消息数）、`tokensAfter`（保存时估算）。这些字段只在服务端读；压缩消息发给模型时只用摘要文本（`AsUserMessage`），所以不会泄漏给模型。
4. **运行中轮次的历史截断**：`transcriptStart` 现在是上下文里的位置。改为：轮开始时的 checkpoint 写入用户消息之后，取文件 entry 数 − 1。文件只追加，这个位置在压缩后依然有效；`compacted`（停用截断）只在真的重写了文件时才置位，由 checkpoint 返回。
5. **每轮现算参数**：在 `runHostLoop` 复制 `runCfg` 之后，对副本设置 `ContextWindow` / `Compaction`，不改会话里存的配置。
6. **OpenRouter 窗口**：`/api/models` 拉免费目录时顺带把 `context_length` 写进一个 1 小时 TTL 的缓存。缓存未命中时，这一轮用默认窗口，并在后台刷新，不让轮次等网络。
7. **`/compact` 不带参数**：`compaction.Compact` 没有"额外要求"参数，为此改 internal 不值得。在 `handleMessage` 里、`resolveWebInput` 之前拦截 `/compact`，走 `startTurn` 的一个变体：执行体换成压缩，其余（独占、停止、计费、结束记录）不变。这一轮不写用户消息，所以刷新后压缩记录归到上一轮末尾。
8. **活动日志的压缩项**：服务端快照（`turnRun.publish`）和前端 `foldActivity` 都要处理 `compaction` 事件。
   - 渲染成 assistant-ui 的 `data` part（`name: "compaction"`），不参与工具分组；
   - 历史里的压缩记录归入它所在轮的活动项，可以展开看摘要。
9. **用量行的上下文**：取这一轮最后一次 `chat` 调用的 input + 缓存读写 + output。窗口和阈值来自会话响应的 `context`（当前模型），历史轮次也按当前模型的窗口显示。自动压缩发生在轮末最后一次调用之后，所以用量行显示的是压缩前的大小，旁边的压缩记录会给出"前 → 后"。

## 6.6 实施中的修正（e2e 发现）

1. **保留部分可能全是未保存的消息**：循环在步骤的 checkpoint 之前压缩。如果当前步骤的输出本身就占满了保留预算，保留下来的就全是这一步还没写入文件的消息，旧视图里一条也没保留。`keptTail` 现在接受这种情况（k = 视图长度，摘要后面的消息全部视为新消息），只有消息无法编码时才退回重写。原来的规则会退回重写，丢掉用户消息。
2. **没有可总结的内容时，循环只发 start 不发 end**：上下文超过阈值，但切点之前没有可以总结的内容时，内核发出 `CompactionStartEvent` 后就直接返回。服务端在收到下一个事件或这一轮结束时，补发一个 skipped 的 end；服务端快照和前端都把这种自动压缩项删掉，不显示。手动 `/compact` 仍然回复"无需压缩"。
3. **不显示"压缩后大小"**：压缩后内核的估算仍以保留下来的那条助手消息上报的 usage 为准，所以压缩后的数字会等于压缩前。说明里只写"上下文约 X token，总结了 N 条"，真实大小看下一条回复的用量行。
4. **同一条消息里 tool call id 去重**：有的服务端按每次响应编号（call_0、call_1…），一条消息跨多次响应时 id 会重复，assistant-ui 遇到重复 key 会直接崩溃、整页空白。`turnContent` 给重复的 id 加上后缀。
5. **已知的边界**：如果单个步骤的输出就超过了阈值和保留预算之差（窗口很小，或者工具输出极大），之后每一步都会触发一次压缩，但能总结的内容很少。默认 128k、80% 时，一步要超过约 80k token 才会出现，这种情况罕见，本期不处理。

## 7. 实施步骤

1. **参数**：settings 字段、校验、`contextParams` 解析、OpenRouter `context_length` 解析与缓存、API。
2. **打开自动压缩**：每轮开始按 3.3 设置 runCfg。
3. **transcript**：视图/日志分离的读写、`firstKeptEntryId`、兼容旧文件。这一步最复杂，测试要最细。
4. **`/compact`**：作为一轮执行，复用第 3 步的写入。
5. **事件与界面**：压缩流事件、活动日志说明、历史分隔、用量行的上下文占用、设置区块。
6. **测试与 e2e**（第 8 节）。

## 8. 测试

- **参数**：
  - 解析顺序：覆盖 > OpenRouter 目录 > 默认；
  - 校验边界；
  - 换算：128k、80% → reserve 25.6k、keep 20k；32k → keep 8k。
- **自动压缩**：假模型、小窗口（比如 8k、阈值 60%）下多轮后触发。之后上下文 = 摘要 + 尾部；计费里有一条 `compaction`；活动日志有记录。
- **transcript**：
  - 压缩后文件只增不减、`firstKeptEntryId` 正确；
  - 重载会话，上下文和压缩后内存里的一致；
  - 多次压缩；
  - 旧格式文件；
  - 压缩和新消息在同一次保存里；
  - 网页历史显示全部消息和分隔。
- **`/compact`**：短会话回复"无需压缩"；长会话压缩成功；运行中和别的轮互斥；可以停止。
- **e2e（9988 + 假模型）**：在小窗口下跑满，看活动日志、用量行、刷新后的历史。

## 9. 风险

- **按内容匹配保留部分**：理论上可能有两条编码完全相同的消息（比如同样的工具结果）。所以只在保留部分紧接旧视图末尾的范围里匹配，并且要求整段连续一致；匹配不上就退回整体重写，并打日志。
- **token 估算偏差**：没有 usage 的尾部按 4 字符/token 估算。中文偏多时会低估，阈值因此留了余量（默认 80%）。
- **摘要质量**：取决于模型。摘要会覆盖工具细节，所以文件读写列表会附在摘要里（内核已有）。
