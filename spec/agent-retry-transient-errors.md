# SPEC: Transient 错误自动重试（移植自 pi）

- 状态：已实现，待审核
- 日期：2026-09-17（v2：重试下沉到请求边界，分类改为结构化）
- 关联背景：OpenRouter 免费模型（如 `stealth/union-alpha`）频繁触发 429 限流，导致一次提问直接失败、无任何回复

## 1. 需求

用户使用 OpenRouter 免费模型时，一次会话内的第二次 completion 请求撞上限流（429），pigo 直接报错终止，用户得不到回复，只能手动重问。

pi（pigo 复刻的上游项目）在 agent 层内置了 transient 错误自动重试。本次将该能力移植进 pigo：

- 默认开启，无需任何配置
- 只对 transient（临时性）错误重试：429 / overloaded / 5xx / 超时 / 连接中断
- permanent（永久性）错误立即失败：缺 API key、401/403/404/400/422、未知模型等
- 指数退避：2s 起步、逐次翻倍、60s 封顶，尊重服务端 `Retry-After`
- 每个 run 最多重试 3 次
- 可通过配置文件调整或关闭

## 2. 分层：为什么重试在请求边界而不在 turn 循环

失败信息在向上传递的过程中会**逐层降级**：transport 知道 HTTP 状态码和 `Retry-After`，decoder 知道 provider 自己的 error type（`overloaded_error`），而到了 `loop.go` 的 turn 层，这些都已经被压成一句 `ErrorMessage` 字符串。在 turn 层做重试，就只能用子串匹配去**反推**下层本来就知道的事实——这是 v1 的做法，实测对含长数字串的真实 429 响应体会误判为 permanent（`"retry after 4000 ms"` 命中 `400`）。

因此本版分三层落位：

| 层 | 职责 |
| --- | --- |
| `internal/provider`（errors.go） | **判定**：错误自带结构（状态码 / provider error type / `Retry-After`），`IsTransient(err)` 是唯一判定入口 |
| `internal/runtime/retry.go` | **策略**：一个 run 的重试配额、退避时长 |
| `internal/runtime/stream_response.go` | **执行**：在"一次 provider 请求"的边界上重试 |

选择请求边界（而非 turn 边界）的三个理由：

1. **上下文归属**：往 `agentCtx.Messages` 里 backfill partial 的就是这一层，所以它能干净地把失败的那次尝试撤销（`Messages[:mark]`）。turn 层看不到这个写入的边界，撤销不了——v1 因此会把失败/半截的 assistant message 留在上下文里，重试请求以 assistant message 结尾，在 Anthropic 协议下会被当成 prefill 并触发 400。
2. **turn 语义**：v1 用 `continue` 重跑 turn，于是一次逻辑 turn 会 emit 多次 `turn_start`，telemetry 的 turn 数虚高、消费者会看到带 error 的假 turn。现在重试完全发生在一个 turn 内部。
3. **可见性**：这一层知道这次尝试是否已经把内容吐给消费者了，而这决定了重试是否安全（见 §3.3）。

## 3. 行为规格

### 3.1 判定（`provider.IsTransient`）

自带类型的 provider 错误自己回答，其余按传输层事实回答：

| 来源 | 类型 | transient 判据 |
| --- | --- | --- |
| HTTP 非 2xx | `*provider.UpstreamError{Status, RetryAfter, Body}` | 408 / 429 / 5xx（含 Cloudflare 529）→ 是；401/403/404/400/422 等 → 否 |
| 流内 error 事件 | `*provider.APIError{Family, Type, Message}` | error **type** 含 `rate_limit` / `overloaded` / `server_error` / `api_error` / `service_unavailable` / `timeout` → 是；`invalid_request_error`、`insufficient_quota` 等 → 否 |
| 看门狗 | `errStreamIdle` / `errStreamStall` | 是 |
| 连接层 | `net.Error` / `*net.OpError` / `io.ErrUnexpectedEOF` | 是；`*net.DNSError` 仅在非 `IsNotFound` 时是 |
| 取消 | `context.Canceled`、裸 `context.DeadlineExceeded` | 否（取消是决定，不是故障；调用方的 deadline 已死，重试无处可跑） |
| 其余 | — | 否（未知错误快速失败，比重试三次慢慢失败对用户更友好） |

关键差异：**只匹配 error type 这种机器生成的封闭词表，绝不匹配自由文本的 message**。响应体里的时间戳、请求 id、"try again" 之类的措辞与"能否重试成功"无关。最后一道兜底是一组明确的网络短语（`connection reset` / `broken pipe` / `unexpected eof` / `goaway` / `stream error`），不含任何状态码数字。

### 3.2 策略（`retryBudget`）

- 配额**整个 run 共享**（一个 run 最多 `MaxRetries` 次），避免多 turn 长任务里无限累积——这是与 pi 的有意差异（pi 按单次请求计）
- 退避 `BaseDelay << (attempt-1)`，封顶 `MaxDelay`（默认 2s → 4s → 8s，封顶 60s）
- 服务端 `Retry-After` 作为**下限**：比退避长则采用它；**超过 `MaxDelay` 则直接放弃重试**，而不是睡到封顶再失败一次
- `RetrySettings` 零值 = 开启且取默认值；关闭只能显式 `Disabled: true`，无法误触

### 3.3 执行（`streamAssistantResponse`）

一次 turn 内的循环：

1. `attemptAssistantResponse` 发一次请求，返回 `{Message, Failure, Streamed}`
2. `Failure != nil && !Streamed` 且预算允许 → emit `RetryEvent` → 等待 → `Messages[:mark]` 撤销这次尝试 → 重来
3. 否则 emit `MessageEndEvent` 并返回

三条不变量：

- **重试是不可见的**：`message_end` 由外层在"最终结果"上 emit，重试掉的那次尝试不会产生 `message_end`、不会产生 `turn_end`、不会留在上下文里。消费者看到的是 `message_start → retry → message_start → message_end`。
- **已经吐出内容的尝试不重试**（`Streamed`）。`DrainStream` 的增量记账（`printed` 仅在 turn 末重置）和 TUI transcript 都假设一个 turn 内文本只增不减，重试会造成重复或截断。实际要防的失败（429、overloaded、一个字节都没吐就断）都发生在出内容之前，正好落在覆盖范围内。
- **退避中被取消**：不撤销上下文，直接把已有的失败作为最终结果上报，run 立即结束。

`StopReasonAborted`（用户中断）永不重试。

## 4. 配置

`config.toml` 的 `[retry]` 段（全部可省，缺省即默认开启）：

```toml
[retry]
enabled = true        # false 可整体关闭
max_retries = 3       # 每个 run 的最大重试次数
base_delay_ms = 2000  # 首次重试等待
max_delay_ms = 60000  # 退避封顶，同时也是 Retry-After 的容忍上限
```

解析链：`config.RetryConfig` → `cmd/pigo` 的 `resolveRetryConfig` → `cliOptions.retryCfg` → 三个入口（tui / repl / headless）的 Options → `LiveConfig.Retry` → `LoopConfig.Retry` → `newRetryBudget`。

注意：`internal/cli/config` 不能 import `internal/runtime`（import cycle：runtime → compaction → provider → cli/config），所以 TOML 结构与 runtime 结构的合并函数放在 `cmd/pigo`。

pigo-server 未暴露配置，走零值 → 默认开启。

## 5. 与 transport 层重试的关系

`transport.connect` 早就有连接级重试（429/503/529，默认 2 次，遵守 `Retry-After`），但它只能在**流还没开始**时重试；一旦 SSE 开始推送，partial 已经进了上下文和 UI，transport 无法回滚——这正是需要一个更高层重试的原因。

两层叠加后单个 turn 最多 3 × 4 = 12 次上游请求。`TransportConfig.MaxConnectRetries` 现在支持传负值来关闭连接级重试，把重试完全交给 agent 层。

## 6. 事件与可观测性

`agentcore.RetryEvent{Attempt, MaxRetries, Delay, Reason}`：

- REPL：暗色打印 `request failed (...), retrying in Ns (a/b)…`
- TUI：spinner 固定提示 `Retrying in Ns (a/b) after transient error`；下一次输出（text delta 或 turn end）自动解除固定
- headless stream-json：`{"type":"retry","attempt":N,"maxRetries":3,"delayMs":2000,"reason":"..."}`
- pigo-server：`{"type":"notice","notice":"retry","text":"..."}`
- telemetry：`TelemetryEvent.RetryCount`，进入 `TelemetrySummary.retry_count`（schema v1 增字段，向后兼容）与 stream-json 的 `retryCount`

## 7. 改动文件清单

| 文件 | 改动 |
| --- | --- |
| `internal/provider/errors.go` | 新建：`UpstreamError` / `APIError` / `IsTransient` / `RetryAfterHint` |
| `internal/provider/transport.go` | 非 2xx 返回 `*UpstreamError`（带 `Retry-After`）；provider 自报错误不再冠以 "decode error"；`MaxConnectRetries` 负值语义 + 防止 nil/nil 返回 |
| `internal/provider/openai.go`、`anthropic.go` | 流内 error 事件返回 `*APIError` |
| `internal/provider/responses.go` | SDK API 错误重新映射为 `*UpstreamError` |
| `internal/runtime/retry.go` | 重写：`RetrySettings`（零值即开启）+ `retryBudget` + 退避 |
| `internal/runtime/stream_response.go` | 拆成 `attemptAssistantResponse` + 重试外壳；撤销、延后 `message_end` |
| `internal/runtime/loop.go` | 恢复为单纯的终止分支，只负责创建 run 级预算 |
| `internal/runtime/telemetry*.go`、`internal/agentcore/event.go` | `RetryEvent` 定义；`RetryCount` 进 telemetry 两个投影 |
| `internal/runtime/headless.go` | stream-json 输出 retry 事件与 `retryCount` |
| `agent/events.go` | 公共 API 的 `EventRetry`/`RetryEvent`（DelayMs 毫秒）及映射 |
| `internal/cli/repl/*`、`internal/cli/tui/*` | 提示 + 透传；spinner 解除固定 |
| `internal/cli/headless/headless.go`、`internal/cli/liveconfig.go`、`internal/cli/config/config.go`、`cmd/pigo/main.go` | 配置透传 |
| `cmd/pigo-server/hostloop.go`、`main.go` | retry → `notice` 事件 |
| `frontend/src/api.ts` | `notice` 事件类型（仅类型，UI 未接，见 §9） |
| `config.toml.example` | `[retry]` 示例段 |
| `internal/provider/errors_test.go`、`internal/runtime/retry_test.go` | 新建/重写 |

## 8. 测试

`internal/provider/errors_test.go`：

- 状态码判定，含"429 响应体里含 `400` 数字串"和"404 body 写着 try again"两个反例（正是 v1 分类器会误判的输入）
- provider error type 判定；message 里写 "try again in 404 seconds" 也不影响结论
- 连接层错误（idle/stall/reset/DNS/GOAWAY）与取消、裸 deadline 的区分
- 端到端：httptest 返回 429 + `Retry-After: 7` → 调用方拿到 `*UpstreamError{429, 7s}`
- 端到端：Anthropic `overloaded_error` 事件 → `*APIError` 且 transient

`internal/runtime/retry_test.go`：

- 策略：分类委派、退避序列、零值/部分覆盖、nil 预算、run 级配额耗尽、`Retry-After` 作为下限且超限即放弃
- run 级：2 次 429 后成功时**事件序列逐项断言**（只有 1 个 turn、1 个 message_end、1 条消息留在上下文）；配额耗尽；permanent 立即失败；关闭开关；**已出内容不重试**；退避中取消（以 retry 事件为触发点，无共享状态、无数据竞争）
- telemetry：`RetryCount == 2` 且 `Turns == 1`

端到端（真实 transport + OpenRouter driver + httptest）：

- 前两次请求 500（body 含 `retry after 4000 ms` 这种会骗过子串匹配的数字串）→ 重试 2 次后成功，上游共 3 次请求，上下文只留成功那条
- 401 → 0 次重试、上游 1 次请求，失败经 `DrainStream` 上报（这类"连流都没建起来"的早期失败不进上下文，与改动前一致）

`cmd/pigo/main_test.go`：`[retry]` 缺省 / 部分覆盖 / `enabled = false`。

全量：`go build ./...`、`go test -race ./...` 全绿（v1 的 cancel 用例存在数据竞争，`go test -race` 必挂，CI 用的正是 `-race`）。

## 9. 已知限制 / 后续

- ~~web 前端未渲染重试提示~~：已完成。`api.ts` 的 `stream()` 增加 `onNotice` 回调，`App.tsx` 用 `RunNotice` 在输入框上方显示一条带脉冲圆点的状态行，正文一开始产出或 run 结束（含失败/中断）即清除；`web/dist` 已重建。
- pigo-server 暂无 `[retry]` 配置入口（写死默认开启）。
- 已出内容后的中途断流不重试（见 §3.3）。要放开需要一个"丢弃已发 partial"的事件，让 `DrainStream` 和 TUI transcript 能回滚。
- 重试次数 run 级共享，长任务后段可能已无额度。
- 早期失败（`cfg.Stream` 直接返回 error，如 401/缺 key）产生的终止消息不写入 `agentCtx.Messages`，而流中途的失败会写入。这个不一致是改动前就有的，本次未动。

## 10. 上游合并指引

本特性刻意把重量放在上游少动的层（`stream_response.go` 在仓库 378 个 commit 里只被改过 2 次），并让绝大多数改动是纯追加。万一 `git merge upstream` 在这些位置冲突，按下面的锚点重新施加，不要凭直觉合并——每一处都在守一条不变量。

**零冲突**：`internal/provider/errors.go`、`errors_test.go`、`internal/runtime/retry.go`、`retry_test.go` 是新文件。

**纯追加（14 个文件）**：结构体加字段、switch 加 case、常量表加一项。冲突形态只会是"两边都在同一位置追加"，保留双方即可。

**需要人工判断的 7 处**：

| 位置 | 锚点 | 重新施加 |
| --- | --- | --- |
| `stream_response.go` 的 drain 循环 | `StreamDoneEvent` / `StreamErrorEvent` 两个分支 | 分支内**只**做 `finalizeMessage` + 返回 `attemptResult`，**不要**在这里 emit `message_end`——它由外层在最终结果上 emit，这是"重试不可见"的实现方式 |
| 同上，各 `Stream*Event` 分支 | `update()` 闭包 | partial 有内容就置 `streamed = true`；漏了会导致重试撕裂已显示的文本 |
| `stream_response.go` 函数签名 | `budget *retryBudget` 参数 | 若上游改了签名，把 budget 参数加回去；`nil` 表示不重试 |
| `loop.go` 的 `case StopReasonError, StopReasonAborted` | 分支只有 `emit(TurnEndEvent)` + `finish()` | **不要**在这里加重试逻辑（那是 v1 的做法，见 §2） |
| `transport.go` 的 `connect()` | 两个非 2xx 分支 | 返回 `*UpstreamError` 而非 `fmt.Errorf`，且带上 `retryAfter(resp.Header)` |
| `transport.go` 的 `flush()` | `dec.Decode` 错误分支 | provider 自报的 `*APIError` 不加 "decode error:" 前缀 |
| `openai.go` / `anthropic.go` 的 error 事件 | 返回值 | 返回 `*APIError{Family, Type, Message}`，别退回拼字符串——拼完就没法分类了 |

**合并后跑这三个测试即可确认不变量没丢**（它们是这套设计的看门人，任何一条被无声地合掉都会红）：

```
go test -race ./internal/runtime/ -run 'TestRunRetriesTransientFailureThenSucceeds|TestRunDoesNotRetryAfterVisibleOutput|TestRunRetriesAgainstRealTransport'
```

第一个逐项断言事件序列（重试不可见），第二个守住"已出内容不重试"，第三个走真实 transport + driver，任何一层的分类退化都会让它失败。

**长期解**：provider 层的类型化错误（§3.1）本身与重试无关、对上游普遍有用，可以单独提 PR 给 smallnest/pigo；一旦上游接受，这部分的合并成本归零。
