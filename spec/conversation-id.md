# 自定义 provider 的会话 ID（conversation_id）

状态：已实现（2026-09-19）

## 1. 问题

hy3 通过 workbuddy2api（CodeBuddy 反代，`codebuddy-proxy`）调用时，前缀缓存几乎不命中：一个会话 12 次调用只有 1 次命中。

## 2. 排查结论

| 路径 | 增长中的工具对话，第 1–5 次调用的缓存命中 |
|------|------|
| 官方 CodeBuddy CLI（CN 端点） | 2% · 98% · 99% · 99% · 99% |
| pigo → 反代，不带会话 ID（原状） | 0 · 0 · 0 · 0 · 0 |
| 反代，带唯一 `prompt_cache_key` | 0 · 0 · 0 · 0 · 0 |
| 反代，body 带 `conversation_id` | 0 · 0 · 87% · 88% · 89% |
| pigo 实装后（9988，真实 hy3） | 0 · 94% · 92% · 94% · 90% · 93% |

- pigo 的请求前缀本身是稳定的：系统提示词、工具声明逐次不变，每次只是在末尾追加消息。唯一每次重建的是最后的 todo 提醒，只有几百 token。
- 反代对 body 的改写都是确定性的，不会破坏前缀。
- 决定性的是会话标识。官方 CLI 在每个请求的 `x-conversation-id` 头里带会话 ID，body 里既没有 `prompt_cache_key` 也没有 `conversation_id`。反代只在 body 带 `conversation_id` 时才向上游发 `X-Conversation-ID`；pigo 原来不带，上游就不会把同一会话留在同一份缓存上。
- `prompt_cache_key` 对命中率没有影响。

反代侧另有两处问题，已记录，未改动：

- 客户端不带会话 ID 时，`InjectPromptCacheKey` 为同一账号的所有会话生成同一个 key；
- 它不读入站的 `X-Conversation-ID` 头，注释里却说读。

## 3. 设计

| 决策 | 选择 | 理由 |
|------|------|------|
| 范围 | 按自定义 provider 开关 | 需要它的是反代这条路径（走它的所有模型），不是某个模型；其它 OpenAI 兼容服务可能拒绝不认识的字段，不能全局发 |
| 配置位置 | Provider 面板里的复选框（存在 settings 的 `customProviders[].conversationId`） | 立即生效；改名、编辑时随定义一起走 |
| 形式 | 专用开关，值固定为会话 ID | 简单、不会配错；底层是通用的 `Extra["body"]`，以后要加别的字段可以再扩 |
| 值 | pigo 会话 ID | 跨轮、跨重启、换模型都不变；反代会用账号 UID 再哈希一次，不会跨账号碰撞 |
| 字段 | body 的 `conversation_id` | 反代只从 body 读 |
| 协议 | 仅 openai | 只有 OpenAI 编码器合并额外字段；anthropic 端点勾选会被校验拒绝，避免"开了但没用" |

## 4. 实现

- **`internal/provider`（唯一改动上游的地方）**：`ExtraBody = "body"`。`encodeOpenAIRequest` 把 `StreamConfig.Extra["body"]`（`map[string]any`）合并进请求体顶层，只填编码器没设过的字段，不会覆盖 model、messages、tools 等。
- **`cmd/pigo-server/conversation_id.go`**：`conversationStream` 包在 provider stream 外，每次调用时读取该 provider 的开关，开着就往 `cfg.Extra` 的副本里加 `conversation_id`。
  - 为什么包在 stream 上，而不是放进 `RunConfig.Extra`：压缩（摘要）调用会自己构造 `StreamConfig`，不带 `Extra`，放在 RunConfig 里它们就拿不到；而且逐次读取开关，切换后下一次调用就生效，已加载的会话也一样。
  - 在 `meterStreams` 里位于最内层，和命名映射一起，对话调用和压缩调用都会经过它。
- **设置与界面**：`customProvider.ConversationID` 加在 `validate()` 里校验；`/api/providers` 返回这个字段；添加和编辑表单里有复选框，协议不是 openai 时禁用；端点那一行显示"· 带会话 ID"。

## 5. 测试

- `internal/provider`：额外字段会被合并，不覆盖编码器已设的字段，不是 map 的值被忽略。
- `conversation_id_test.go`：
  - 开：带上会话 ID，保留原有 Extra，不改调用方的 map；
  - 关、以及其它 provider：不带；
  - 运行中关掉开关，下一次调用就不再带；
  - anthropic 协议被拒绝；
  - 改名后开关保留。
- e2e（9988）：
  - 假模型抓包，开关打开时一轮的 3 次调用都带同一个会话 ID，关掉后（未重启）下一轮不带；
  - 真实 hy3 经反代，从第 2 次调用起命中率达 90–94%。

## 6. 使用

在 设置 → Provider 里编辑 `codebuddy-proxy`，勾选"请求带会话 ID"，保存后立即生效。
