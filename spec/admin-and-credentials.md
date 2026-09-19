# SPEC: 管理员、公共配置与用户自带凭据

- 状态：全部四步已实现
- 日期：2026-09-18
- 背景：pigo-server 目前的 provider key 只来自服务进程的环境变量，所有用户共用同一把；模型下拉只有 OpenRouter 免费目录；没有角色概念，`userRecord` 只有 `{ID, Username, PasswordHash, CreatedAt}`。

## 1. 要解决的问题

1. 多用户部署时，想用 OpenRouter 以外的 provider，必须改环境变量并重启，且所有人共用一把 key。
2. 公共 key 之外，个别用户想用自己的额度（更贵的模型、更高的限额），目前无处安放。
3. 没有人能在运行时维护"这个部署提供哪些模型"，用户只能各自手填模型 id。

## 2. 角色模型

两种角色：**管理员**与**普通用户**。管理员是普通用户的超集，自身仍可正常聊天、配自带 key。

- 身份来源：环境变量 `PIGO_ADMIN_USERS`（逗号分隔用户名，大小写不敏感，按 `usernameKey` 规则归一）
- **预授权**：名单里的用户名注册后自动具备管理员权限；顺序无关
- **空名单 = 没有任何管理员**，公共配置退化为只读（只反映环境变量），即当前行为
- 角色**不落盘**：每次请求按当前环境变量判定。好处是权限不会被数据文件篡改、`auth.json` 无需迁移；代价是改名单要重启
- 启动日志明确打印：`admins: alice, bob`，或 `admins: none (set PIGO_ADMIN_USERS to enable the admin console)`

`requestPrincipal` 增加 `Admin bool`，在 `principalForRequest` 里按名单判定；新增 `requireAdmin` 中间件，非管理员返回 403。

## 3. 凭据

### 3.1 三处来源与优先级

解析某个 (用户, provider) 要用的 key，从高到低：

1. **用户自带 key**（该用户为该 provider 配置的）
2. **管理员配置的公共 key**
3. **进程环境变量**（`OPENROUTER_API_KEY` 等，registry 里声明的）

前两级命中时通过 `CredentialStore.SetOverride(provider, key)` 注入（`internal/provider/auth.go:195`，已有的 seam）；都不命中时不设 override，`CredentialStore` 自然回退到环境变量与配置文件，行为与今天一致。

注意一个后果：用户填了自带 key 之后**不再消耗公共池**。所以"自带 key"实际上是让个人用更贵模型的通道，而不是替公共池省钱的手段——这与"公共池不限量、内网信任"的选择是自洽的。

### 3.2 存储与加密

用户 key 与公共 key 都**加密存盘**，共用一套方案：

- 主密钥来自 `PIGO_SERVER_SECRET`，经 HKDF-SHA256 派生出 32 字节加密密钥（`info` 区分用途，便于将来派生别的用途密钥）
- AES-256-GCM，每条记录独立随机 nonce，密文与 nonce 一起 base64 存入 JSON
- 落盘位置：`<dataDir>/credentials.json`，权限 0600，写入走"临时文件 + rename"以免半截文件
- 结构：`{"public": {provider: enc}, "users": {userID: {provider: enc}}}`

**`PIGO_SERVER_SECRET` 未设置时降级**：

- 启动时打印一次明确警告
- 自带 key 与公共 key 的**写入接口返回 503** 并说明原因；读取路径直接跳过（视为未配置）
- 其余功能（公共模型列表、默认模型、注册开关、用户管理）照常
- 已存在的 `credentials.json` 不读、不删、不覆盖——密钥找回后数据还在

**key 永不回显**：所有读接口只返回 `{provider, configured: true, hint: "sk-...abcd"}`（末 4 位）。没有任何接口能取回明文，管理员也不行。

## 4. 公共配置

管理员可维护、全体生效，存 `<dataDir>/settings.json`（明文，不含密钥）：

| 项 | 说明 |
| --- | --- |
| 默认模型 / 默认 thinking | 新建会话的默认值，替代写死的 `-model openrouter/free` |
| 注册开关 | 关闭后 `POST /api/auth/register` 返回 403（`PIGO_SERVER_TOKEN` 仍可覆盖，用于管理员拉人） |
| 是否允许用户自带 key | 关闭后自带 key 的写接口返回 403，已存在的条目**停止生效**（不删除） |
| 公共模型列表 | 见下 |

**生效时机**：只影响此后新建的会话与新发起的请求。正在进行的 run 不受影响（重新解析 provider 会打断流）。已存在的空闲会话在下一次请求时经 `applyHostConfig`（`hostloop.go:192`）重新解析，因此改了公共 key 后用户不必新建会话。

## 5. 模型列表

现有 `customModelStore`（`custommodels.go`）加一个 `Scope` 字段：

- `scope: "public"` —— 管理员维护，对所有用户可见，**没有过期日期**，`UserID` 为空
- `scope: "user"` —— 保持现状：per-user、带 `ExpiresAt`、到期后拒绝

老数据（无 `scope` 字段）反序列化后一律补成 `"user"`，行为不变。

用户看到的列表 = OpenRouter 免费目录（现状）+ 公共模型 + 自己的自定义模型。**同一个 model id 重复时公共条目优先显示**，避免用户被自己过期的同名条目挡住。

## 6. 用户管理

管理员可以：

**看用户列表** —— 用户名、注册时间、是否禁用、是否管理员、自带 key 配了哪几个 provider（只给 provider 名，不给 key）。

**禁用用户** —— `userRecord` 加 `DisabledAt *time.Time`。禁用时：
- 登录被拒
- **立即吊销该用户所有活跃会话**（`authState.Sessions` 里按 `UserID` 清除），否则已登录的浏览器还能用到 token 过期
- 已有的 workspace 与 transcript 保留

**硬删用户** —— 不可逆，需二次确认（前端要求输入完整用户名）。按顺序：
1. 终止该用户所有在跑的沙箱会话（活跃 run 先取消）
2. 删除 `auth.json` 里的用户与其全部 auth session
3. 删除其全部会话目录与 workspace（`sessiondir.go` 的路径）
4. 删除 `credentials.json` 里该用户的条目
5. 删除其 `scope: "user"` 的自定义模型

任何一步失败都要记日志并继续删剩下的，最后返回哪些没删干净——半删状态比"报错后什么都没删"更好处理。

**重置密码** —— 服务端生成随机临时密码（16 字符，字母数字），bcrypt 后写入，**明文只在该次响应里返回一次**，此后无处可查。同时吊销该用户的活跃会话。是否强制首次登录改密作为后续可选项，本期不做。

**管理员不能对自己做**：禁用、删除、降权都拒绝（避免把最后一个管理员锁死）。改自己的密码走普通改密流程。

## 7. API

全部挂在 `requireAdmin` 后面：

```
GET    /api/admin/settings            # 公共配置（不含密钥明文）
PUT    /api/admin/settings            # 默认模型/thinking、注册开关、自带 key 开关
GET    /api/admin/credentials         # [{provider, configured, hint}]
PUT    /api/admin/credentials/{name}  # 写入公共 key（secret 缺失时 503）
DELETE /api/admin/credentials/{name}
GET    /api/admin/users               # 用户列表
POST   /api/admin/users/{id}/disable  # body: {disabled: bool}
POST   /api/admin/users/{id}/password # 返回一次性临时密码
DELETE /api/admin/users/{id}          # 硬删
POST   /api/admin/models              # 公共模型（复用 custom-models 的形状，scope=public）
DELETE /api/admin/models/{id...}
```

普通用户侧新增：

```
GET    /api/credentials               # 自己的：[{provider, configured, hint}]
PUT    /api/credentials/{name}        # 写入自带 key
DELETE /api/credentials/{name}
```

`GET /api/providers`（已有）补一个字段，说明该 provider 的 key 来自哪一级：`source: "user" | "public" | "env" | "none"`，这样前端能如实告诉用户"你正在用公共池"还是"用你自己的 key"。

## 8. 前端

设置是**独立页面** `/settings/<tab>`（真实路径，服务端 SPA fallback 支持冷加载直达）。

信息架构按**被配置的对象**组织，而不是按所有权——早先按「个人 / 管理」切分，导致同一个概念被拆成两份隔在两个分组里（我的 Key / 公共 Key、自定义模型 / 公共模型），用户想用某家模型要跨三个面板拼装，且地址无处可填。现在：

```
配置                          管理（仅管理员）
  会话     当前会话的模型+推理      部署   默认模型、注册开关、自带 Key 开关
  Provider 地址 + 两级凭据         用户   账号管理
  模型     目录 + 公共 + 我的
  工作区
  运行状态
```

**Provider 表**（一行一个端点，展开后配置）：

- 行上用 `○我的Key ●公共 ○环境变量` 直接标出当前生效的层级——这是最容易搞错的规则，所以画出来而不是写在说明里
- 展开后同时有「我的 Key」和「公共 Key」两个输入框，管理员多看到后者；自定义端点额外显示协议与地址
- 默认只显示**已配置的**（有 key 或是自定义端点），38 个内置的通过「添加」下拉挑选加入，避免列表淹没
- 「自定义端点」标签页（仅管理员）录入 `名称 / 协议 / base_url`

**模型表**（一行一个模型，来源作为行上的标签）：

- `[公共]` / `[已被公共覆盖]` / `[已过期]` 三种标记，覆盖关系标在被盖住的那一行上
- 管理员添加时勾选「加为公共模型」，**勾选后有效期字段直接消失**——公共条目不会过期，留一个无意义的必填项只会让人困惑

每个面板都不超过一屏（实测 `scrollHeight == clientHeight`）。

## 8.1 自定义 Provider

管理员可定义不在 registry 里的端点，存 `settings.json` 的 `customProviders`（地址不是密钥，明文即可；API Key 仍走加密存储，key 名就是自定义名）。

```json
{"name": "my-gateway", "protocol": "openai", "baseUrl": "http://10.0.0.5:8000/v1"}
```

- 归管理员管：把服务端指向任意 URL 是部署级决定，不该是个人偏好
- 名称须为 2-32 位小写字母/数字/下划线/短横线，且**不得与内置 provider 重名**
- 协议只接受 `openai`（Chat Completions 兼容）或 `anthropic`（Messages 兼容）
- 删除端点会一并删掉它的公共 key，否则会留下无人能解的密文

**实现上最关键的一点**：`provider.ResolveProvider` 走 protocol 分支时返回的驱动名是写死的 `"openai"`/`"anthropic"`。服务端的 `resolveProvider`（`hostloop.go`）必须**保留自定义名**——让通用名透出去会把自定义端点的凭据记到内置 provider 名下，等于把真正的 OpenAI key 交给一个自建网关。有测试专门守这条。

**已知毛刺**：`NewOpenAICompatibleProvider` 的 `requiresAuth: true`，所以本地无鉴权端点（vLLM / LMStudio）也必须填一个占位 Key，否则请求在建流前就被拒。UI 里有提示。要根治需要在 provider 层加一个"不要求鉴权"的构造入口。

## 9. 安全边界

- 明文 key 只在三个地方出现：写入请求体、内存、发往 provider 的请求头。不写日志、不写 transcript、不进任何 GET 响应
- 主密钥泄露 = 所有存储的 key 失守，这是选择"服务端加密存盘"时接受的代价。缓解：`PIGO_SERVER_SECRET` 从环境注入而非文件，与 `dataDir` 分离
- 管理员权限来自环境变量，因此**拿到 `auth.json` 写权限不等于能提权**
- 公共池不限量是明确选择（内网信任）。若日后要对外部署，限流是必须先补的一块

## 10. 我替你定的默认（若不同意请指出）

1. 角色不落盘，每次按环境变量判定——因此改名单要重启
2. 公共配置只对新会话与空闲会话的下一次请求生效，不打断正在跑的 run
3. 自带 key 的粒度是「每 provider 一把」，不做「每模型一把」
4. 管理员写操作打一行结构化日志（谁、做了什么、对谁），不单独做审计文件
5. 临时密码不强制首次改密
6. 禁用/删除用户时立即吊销其 auth session
7. 管理员不能对自己执行禁用/删除

## 11. 实施顺序

分四步，每步都能独立跑起来并验证：

1. ~~**角色与鉴权**~~：已完成。`admin.go`（roster + `requireAdmin` + 审计日志 + settings handlers）、`auth.go`（`Admin` 字段、`withPrincipal` helper、注册开关）、启动日志打印 `admins:`。
2. ~~**凭据存储**~~：已完成。`credentials.go`（HKDF-SHA256 + AES-256-GCM，AAD 绑定 slot，降级）、`credentials_api.go`（用户侧 + 管理员侧接口）、`settings.go`（公共配置，第 3 步的基础，因被自带 key 开关依赖而提前落地）、`hostloop.go` 的 `credentialStoreFor` 接进 `SetOverride`、`/api/providers` 增加 `source` 字段。
3. ~~**公共模型**~~：已完成。`customModel.Scope`（`public`/`user`）、加载时与 `add` 两处补默认的迁移、`listVisible` 的"公共优先"去重、管理员三个接口；`handleModels` 改为"显式配置 > 抓取目录"。
4. ~~**用户管理与前端**~~：已完成。`auth.go` 的 `DisabledAt` 与四个 store 方法、`users_api.go` 的控制台与级联删除、`frontend/src/admin.tsx` 的两块面板、`web/dist` 已重建。

## 12. 实现补充说明

实现过程中定下的、spec 原文没写的细节：

- **服务令牌视同管理员**（`requireAdmin`）：`PIGO_SERVER_TOKEN` 是部署方自己的凭证，本来就能建账号，权限不应低于它创建出来的管理员。
- **停用是双重防线**：`setDisabled` 主动吊销会话，同时 `authenticate` 也拒绝已停用账号，防止同一瞬间签发的 token 漏网。
- **停用会停掉沙箱但保留数据**；硬删才连 workspace 一起清。
- **临时密码字母表排除易混字符**（`0/O`、`1/l/I`），因为它要被人从屏幕上读出来手打；生成用拒绝采样避免取模偏置。
- **公共模型即使客户端传了 `expiresAt` 也会被丢弃**，避免共享条目某天悄悄失效。
- **模型列表顺序改为"公共 > 用户 > OpenRouter 目录"**：管理员显式指定的路由不应被自动抓取的同名条目覆盖。
- **`add` 也补 scope 默认值**，不只在加载时迁移，这样任何来路的条目都满足不变量。
