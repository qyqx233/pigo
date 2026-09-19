# SPEC: Token 计费与用量审计

- 状态：需求已确认、设计已评审，待实现
- 日期：2026-09-19（v2：补充建模方案与完整改动清单）
- 范围：pigo-server（Web）。CLI/TUI 不在本期，但上游改动对它们的影响在第 8 节逐项核查。

## 1. 目标

1. 按用户审计**每一次** LLM 调用的 token：普通输入、缓存命中、缓存写入、输出。
2. 管理员为每个模型配置单价，费用在调用发生时算好并记账。
3. 实时显示：每条回复的本轮用量、当前会话累计、个人用量面板、管理员全员汇总与导出。

本期只审计和展示，不拦截任何请求。

## 2. 已确认的决定

| 问题 | 决定 |
| --- | --- |
| 自带 Key 的调用 | 照记 token 和费用，标记 `自付`，平台支出汇总里单列 |
| 额度 / 拦截 | 本期不做 |
| 改价对历史的影响 | 每条账目快照调用时的单价，账目一旦记下不再改动 |
| 未定价的模型 | 按 0 元计；另记 `priced: false` 供管理员查漏（不影响金额） |
| 货币 | 统一人民币 |
| OpenRouter 返回的真实扣费 | 两个都记，账面以配置价为准，上游扣费（美元）另存一栏对账 |
| 展示位置 | 每条回复下方、顶栏会话累计、设置页"用量"、管理员汇总 + CSV |
| 删除用户后的账目 | 保留，显示为"已删除用户" |

## 3. 建模：token 口径由"线路方言"决定，价格由"模型"决定

### 3.1 为什么不按模型建模 token 口径

实测 DeepSeek（2026-09-19，同一段 973 token 的输入连发两次）：

```
chat/completions 第 1 次: prompt_tokens 973  prompt_tokens_details.cached_tokens 0    prompt_cache_hit 0    miss 973
chat/completions 第 2 次: prompt_tokens 973  prompt_tokens_details.cached_tokens 768  prompt_cache_hit 768  miss 205
/responses:              input_tokens  973  input_tokens_details.cached_tokens  768   （请求 deepseek-chat，返回模型名 deepseek-flash）
```

由此得出三点：

1. **同一个模型，换个接口，字段就不同**（chat 和 responses）。
2. **经过代理后字段也会变**：同一个 DeepSeek 模型经 OpenRouter 调用，返回的是 OpenRouter 的格式。
3. **模型名不可靠**：服务端会做别名映射（`deepseek-chat` → `deepseek-flash`），`openrouter/free` 每轮换模型，自定义端点背后是什么模型也不知道。

所以 token 口径必须由**响应本身的格式**决定，而不是由"我们以为调的是哪个模型"决定。模型这个维度只用来**定价**。

### 3.2 统一的标准模型（值对象）

`agentcore.Usage` 与 pi 对齐：四类 token **互不重叠**，另附两项参考信息。

```go
type Usage struct {
    InputTokens      int      // 未命中缓存的输入（口径变更，见 3.5）
    CacheReadTokens  int      // 缓存命中
    CacheWriteTokens int      // 写入缓存（目前只有 Anthropic 方言会有）
    OutputTokens     int      // 输出，含推理
    ReasoningTokens  int      // 其中推理的部分，仅供展示，不单独计价
    UpstreamCostUSD  *float64 // 上游报告的真实扣费，没有就是 nil
}
```

不变量：四类都 ≥ 0；`Input + CacheRead + CacheWrite` = 这次请求的总输入。

### 3.3 每种方言一个适配器（Adapter / Strategy）

三种线路协议各有自己的原始 usage 结构，各自负责转换成标准模型。策略由**协议**选定，而协议早已决定了用哪个解码器，所以不需要新的分发表，原有的解码器多态就是策略选择：

| 方言 | 解码器 | 原始字段 | 转换 |
| --- | --- | --- | --- |
| OpenAI Chat（28 个 provider + 自定义 openai 端点 + OpenRouter） | `openai.go` | `prompt_tokens`（含缓存）、缓存命中数（见 3.4）、`completion_tokens_details.reasoning_tokens`、OpenRouter 的 `cost` | `Input = prompt − cached` |
| OpenAI Responses（deepseek） | `responses.go` | `input_tokens`（含缓存）、`input_tokens_details.cached_tokens`、`output_tokens_details.reasoning_tokens` | `Input = input − cached` |
| Anthropic Messages（anthropic、minimax、bedrock 等 5 个 + 自定义 anthropic 端点） | `anthropic.go` | `input_tokens`（不含缓存）、`cache_read_input_tokens`、`cache_creation_input_tokens` | 直接对应 |

### 3.4 同一方言内的厂商差异：按字段逐级兜底（Chain of Responsibility）

OpenAI Chat 方言下，各厂商报"缓存命中数"的字段不同。按优先级取第一个存在的：

1. `prompt_tokens_details.cached_tokens`（OpenAI 标准：OpenAI、OpenRouter、DeepSeek、智谱等）
2. `prompt_cache_hit_tokens`（DeepSeek 原生字段）
3. `usage.cached_tokens`（Moonshot/Kimi，放在顶层）

**按"响应里有什么字段"判断，而不是按 provider 名**。这样自定义端点和代理（bonai、OpenRouter 背后的任何模型）不用额外配置也能正确处理。实测 DeepSeek 两套字段同时返回且数字一致，所以优先级不会造成冲突。

防御性规则：缓存数大于总输入时（说明遇到了一个不按常理报数的厂商），不做减法，`Input` 取原值、`CacheRead` 截为 0，并在账目上标 `usageAnomaly`，事后能查出是哪些调用、哪个方言。

### 3.5 ⚠️ `InputTokens` 口径变更

从"各方言各报各的"改为"统一不含缓存"（和 pi 一致）。今天的现状其实已经不一致：OpenAI 系的 `InputTokens` 含缓存，Anthropic 系不含。

受影响的地方（已全仓核查，完整列表见第 8 节）：

- **压缩的上下文估算**：必须改成四项相加，否则缓存命中越多、估出来的上下文越短、压缩越晚触发，直到撑爆上下文窗口。上游代码的注释原话是 "pi additionally folds cache read/write, which pigo does not track"，所以这里是**向 pi 看齐**。
- **对外接口**：公共 Go API（`agent.UsageEvent`）和 headless 的 stream-json 输出里的 `inputTokens` 含义随之改变，需要在 CHANGELOG 里写明。
- **旧会话兼容**：旧 transcript 里 OpenAI 系的 `InputTokens` 是含缓存的总数、缓存字段为 0。新公式四项相加 = 旧的总数，估算结果不变，**不需要迁移**。

## 4. 采集：在"模型调用"这一层装一个计量装饰器（Decorator）

### 4.1 为什么放在这一层

每一次 LLM 调用都经过 `provider.StreamFn` 这个函数：对话、工具循环里的每一步、被重试的尝试、上下文压缩（`LoopConfig.SummaryStream` 为 nil 时用的也是它）。在服务端把它包一层 `meteredStream`，是**唯一一个能看到所有调用的位置**，而且**完全不用改上游的事件结构和函数签名**：

| 需要记账的情况 | 用事件（`MessageEndEvent`）采集 | 用装饰器采集 |
| --- | --- | --- |
| 普通调用 | 能 | 能 |
| 上下文压缩的摘要调用 | **不能**：`GenerateSummary` 只返回文本，usage 被丢掉了，要记就得改上游三个文件的签名 | 能：把 `SummaryStream` 设成标记为 `compaction` 的装饰器 |
| 请求被中断 | **不能**：`ctx` 取消后事件发不出去 | 能：流关闭时没收到 usage，就记一条"用量未知" |
| 被重试掉的尝试 | 不能：重试时这次尝试已被撤销，不再发事件 | 能：按上游实际报告的用量记 |

### 4.2 结构

```
hostloop 构建运行配置时:
  runCfg.Stream        = meteredStream(providerStream, kind="chat")
  runCfg.SummaryStream = meteredStream(providerStream, kind="compaction")

每次用户提问（runHostLoop）:
  ctx 里带上本轮信息 { userID, username, sessionID, turnID, keySource, emit }

meteredStream(ctx, model, llm, cfg):
  inner := 调真正的 provider
  返回一个代理流：原样转发所有事件
  在终止事件（done / error）或流关闭时：
    取 usage、responseModel、responseID、stopReason
    → 计价（PriceBook）→ 追加账目（Ledger）→ 通过 ctx 里的 emit 推给前端
```

- 每一轮的信息通过 `ctx` 传递：模型调用函数本来就会收到当前运行的 `ctx`，压缩调用用的也是同一个 `ctx`（已核实 `runCompaction(ctx, …)`），不需要改任何函数签名。
- `keySource` 在构建凭据时就已确定（`credentialStoreFor`），和那次请求实际使用的 key 一致。

### 4.3 其余组件

| 组件 | 模式 | 职责 |
| --- | --- | --- |
| `PriceBook` | Repository | 价格表，存 `settings.json`；按 `provider + 模型` 查价，先用应答模型、再用请求模型 |
| `rate(usage, price)` | 纯函数 | 算钱，结果为整数纳元，不碰 I/O，单测覆盖所有分支 |
| `Ledger` | Repository，只追加 | `<dataDir>/ledger/YYYY-MM.jsonl`；写入加锁，读取用于汇总和导出 |
| 前端推送 | Observer | 装饰器通过 `ctx` 里的回调往当前 HTTP 流推 `usage` 事件 |

## 5. 计价

- 单位：元 / 每百万 token，四档单价。缓存命中、缓存写入没配时按普通输入价计。
- 金额用整数存储（纳元，1e-9 元），展示保留 4 位小数。
- 匹配时先用实际应答模型，再用请求的模型（实测别名：请求 `deepseek-chat` 返回 `deepseek-flash`，两个名字都能配价格）。
- 价格表存 `settings.json` 的 `modelPrices`，每项记录最后修改人和时间。

## 6. 账本

每次 LLM 调用一条，只追加：

```json
{
  "id": "…", "at": "2026-09-19T08:00:00Z",
  "userId": "…", "username": "zhouh",
  "sessionId": "…", "turnId": "…", "responseId": "gen-…",
  "provider": "deepseek", "model": "deepseek-chat", "responseModel": "deepseek-flash",
  "kind": "chat", "status": "ok",
  "keySource": "public", "billedTo": "platform",
  "input": 205, "cacheRead": 768, "cacheWrite": 0, "output": 1, "reasoning": 0,
  "price": {"input": 2.0, "cacheRead": 0.2, "cacheWrite": 2.0, "output": 8.0},
  "priced": true, "cost": 571600,
  "upstreamCostUsd": null, "usageAnomaly": false
}
```

- `kind`：`chat` / `compaction`；`status`：`ok` / `error` / `aborted`（`aborted` 表示用量未知，token 为 0）
- `responseId` 用来把账目和 transcript 里的消息对上（三个解码器都已记录这个字段），刷新页面后历史消息下方的用量行就靠它恢复

## 7. 实时展示

- 服务端流新增 `usage` 事件（每次调用一个）；前端按轮累加。
- 每条回复下方一行用量，一轮里的多次调用合成一行，悬停看明细。
- 顶栏模型选择器旁显示会话累计。
- 设置 → 用量（个人）；管理 → 用量（全员、筛选、未定价清单、CSV）；管理 → 模型价格。
- 历史消息：`/messages` 接口按 `responseId` 从账本关联出每轮用量。中断的调用没有 `responseId`，只在用量面板里显示，不挂在回复下面。

## 8. 完整改动清单

### 8.1 上游代码（尽量少、只追加）

| 文件 | 改动 | 说明 |
| --- | --- | --- |
| `internal/agentcore/message.go` | `Usage` 增加 4 个字段，`InputTokens` 口径改为不含缓存 | JSON 字段追加，旧数据兼容 |
| `internal/provider/openai.go` | 原始 usage 结构扩展、按 3.4 兜底取缓存数、转换为标准模型 | |
| `internal/provider/responses.go` | 读取 `InputTokensDetails.CachedTokens`、`OutputTokensDetails.ReasoningTokens` | |
| `internal/provider/anthropic.go` | 读取两个缓存字段；`message_delta` 里的累计值也要合并 | |
| `internal/provider/providers.go` | 仅对 openrouter 的请求体加 `"usage": {"include": true}` | 其它厂商可能不认识这个参数 |
| `internal/compaction/tokens.go` | `calculateContextTokens` 改为四项相加 | 关键，见 3.5 |
| `agent/events.go` | 公共 `Usage` 同步新增字段 | 对外口径变更，写 CHANGELOG |

### 8.2 核查过、确认不用改的

| 位置 | 为什么不受影响 |
| --- | --- |
| `internal/cli/goal/goal.go:310` | 只累加 `OutputTokens`，口径不变 |
| `internal/runtime/telemetry.go`、TUI 状态栏 | 不直接读 usage，上下文占用来自压缩的估算，随 8.1 自动正确 |
| `internal/runtime/headless.go` | 原样输出消息 JSON，新字段自动出现（口径变更随 CHANGELOG 说明） |
| `internal/runtime/loop.go`、`stream_response.go` | 装饰器挂在外面，不需要改 |
| `internal/compaction/summary.go`、`compact.go` | 压缩调用由装饰器计量，不需要改签名 |
| 旧 transcript | 兼容（3.5） |

### 8.3 服务端（新增为主）

| 文件 | 内容 |
| --- | --- |
| `billing_meter.go`（新） | `meteredStream` 装饰器、代理流、`ctx` 里的本轮信息 |
| `billing_price.go`（新） | `PriceBook`、`rate()` |
| `billing_ledger.go`（新） | 账本读写、按用户/模型/日期汇总、CSV |
| `billing_api.go`（新） | 用量接口、价格接口 |
| `hostloop.go` | 构建运行配置时套上两个装饰器；`runHostLoop` 往 `ctx` 放本轮信息；`credentialStoreFor` 同时返回 `keySource` |
| `settings.go` | `modelPrices` |
| `main.go` | 路由；`streamEvent` 增加 `usage` 类型 |
| `history.go` | 历史消息按 `responseId` 附上用量 |
| `users_api.go` | 不改：删除用户时刻意不动账本（加一行注释说明这是有意的） |

### 8.4 前端

| 文件 | 内容 |
| --- | --- |
| `api.ts` | `usage` 流事件、用量与价格接口 |
| `App.tsx` | adapter 把 `usage` 事件挂到消息的 metadata；回复下方的用量行；顶栏会话累计；设置导航加"用量""模型价格" |
| `billing.tsx`（新） | 个人用量面板、管理员用量面板、价格表 |
| `styles.css` | 对应样式 |

## 9. 测试计划

| 层 | 测什么 |
| --- | --- |
| 解码器 | **用真实抓到的响应做样例**：本文 3.1 的 DeepSeek 三份；Anthropic 按官方文档格式；Moonshot 顶层 `cached_tokens`；缓存数大于总输入的异常情况 |
| 压缩估算 | 同一请求用旧口径（含缓存、缓存为 0）和新口径（拆开）算出的上下文长度相等 |
| 装饰器 | 正常结束、上游报错、中途取消（记 `aborted`）、被重试的尝试、压缩调用标记为 `compaction`；代理流跑 `-race` |
| 计价 | 整数运算、未定价按 0 且 `priced=false`、缓存价缺省回落到输入价、先用应答模型再用请求模型 |
| 账本 | 并发追加、跨月分文件、删除用户后仍在、汇总 |
| 端到端 | 用会记录请求的假 LLM 端点：一次提问触发两次工具调用 + 一次压缩，账本里应恰好 4 条，`turnId` 相同，金额与手算一致 |

## 10. 我替你定的默认（不同意请指出）

1. 单位"元 / 每百万 token"，金额整数存储（纳元），展示保留 4 位小数
2. 价格先按实际应答模型匹配，再按请求的模型
3. 缓存命中/写入没配价时按普通输入价计
4. 账本按月分文件，只追加
5. 价格表对普通用户只读可见
6. 上线前的历史对话不回填
7. 中断的请求记"用量未知"，不估算
8. 被重试掉的尝试**如果上游报告了用量就记**（`status: error`），因为上游确实扣了钱；没报告就不记

## 11. 实施顺序

1. **上游采集**（8.1）＋解码器测试＋压缩估算测试。这一步改的是共享代码，单独提交，方便以后同步上游时识别。
2. **计量装饰器 + 账本 + 计价**（后端核心）＋端到端测试。
3. **价格表**接口与页面。
4. **实时展示**：流事件、回复下方用量行、顶栏累计、历史关联。
5. **汇总与导出**。
