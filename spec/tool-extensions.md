# 工具扩展与模型命名映射

状态：已实现（2026-09-19）

怎样写一个命令工具、挂上去、排错，见交接文档 [tool-extensions-cmd-guide.md](tool-extensions-cmd-guide.md)。

## 1. 目标

1. **扩展工具**：部署方能在启动时加入自己封装的工具，也能用同名扩展**替换**内置工具（bash、read、websearch……）。
2. **命名映射**：不同模型对工具名有偏好（`WebSearch` / `web_search` / `websearch`），发给模型的工具名、描述要能按模型定制。

范围只在 `cmd/pigo-server`，不改上游 `internal/`。

## 2. 已确认的决策

| # | 问题 | 决定 |
|---|------|------|
| 1 | 扩展形式 | **沙箱命令工具**为主（配置文件声明，在会话沙箱里执行，任何语言）；另有 **Go 注册入口**给需要宿主能力的实现 |
| 2 | 与内置重名 | **同名即替换**；不同名即新增 |
| 3 | 扩展需要密钥 | **按工具注入 env**：值取自服务端环境变量，只在执行该工具时可见 |
| 4 | 改名在哪层做 | **只在发给模型时映射**；transcript、活动日志、计费、`-tools` 一律用规范名 |
| 5 | Go 扩展接入 | **仓内 `cmd/pigo-server/ext` 包**：注册表 + 会话上下文接口，扩展放 `ext/<名字>/`，`init()` 注册，main 里 blank import |
| 6 | 配置 | **YAML 文件**：`-tools-config <path>` 或 `PIGO_TOOLS_CONFIG`；启动时校验，出错拒绝启动 |
| 7 | 命名偏好绑定 | 文件里定义 **profile**，再按 provider / 模型名**通配规则**选 profile，第一条命中生效 |
| 8 | 映射范围 | **名字 + 可选描述**；系统提示词和工具描述里提到工具名的地方一并改写；参数名不动 |
| 9 | 命令协议 | **stdin 传参数 JSON，stdout 为结果**；退出码非 0 为失败 |
| 10 | 活动日志显示 | 配置 **`detail` 模板**；缺省取第一个字符串参数；覆盖内置时沿用内置的显示 |
| 11 | Go 覆盖内置 | 工厂函数**拿得到被覆盖的内置实现**，可以包装后委托 |
| 12 | 命令在哪执行 | **进会话已有的 bwrap 容器**，与 bash 同一个环境，不另起实例 |

以下是未单独讨论、按惯例取的默认值：

- 配置是**部署级**的，不分用户；改配置需重启。
- `-tools all` = 内置 + 全部扩展；`-tools` 列表里可以写扩展工具名。
- 未命中任何规则的模型用规范名（不映射）。
- 规则里的 `provider` / `model` 是通配：`*` 匹配任意字符（包括 `/`，因为模型 id 常见 `anthropic/claude-…`）、`?` 匹配一个字符，不区分大小写；不写就是匹配全部。
- 映射后出现重名（两个工具映射成同一个名字，或映射名撞上另一个工具的规范名）→ 启动报错。
- 映射名须符合各家 API 的约束：`^[A-Za-z0-9_-]{1,64}$`。

## 3. 配置文件

```yaml
# -tools-config /etc/pigo/tools.yaml

tools:
  # 沙箱命令工具：新增
  - name: xlsx_summary
    description: |
      Summarize an Excel workbook in the workspace: sheets, columns, row counts.
    schema:
      type: object
      properties:
        path: { type: string, description: Workspace path of the .xlsx file }
      required: [path]
    command: python3 /opt/pigo-tools/xlsx_summary.py
    timeout: 60s          # 缺省 2m，上限 10m
    detail: "{path}"      # 活动日志里显示的内容

  # 沙箱命令工具：同名替换内置 websearch
  - name: websearch
    description: Search the company knowledge base and the web.
    schema: { type: object, properties: { query: { type: string } }, required: [query] }
    command: python3 /opt/pigo-tools/search.py
    env:
      SEARCH_TOKEN: ${PIGO_EXT_SEARCH_TOKEN}   # 取自服务端环境变量

  # Go 扩展：启用 ext 注册表里名为 webfetch_allowlist 的实现（ext/allowfetch），
  # 挂在名字 webfetch 上，替换内置：只允许抓取白名单里的域名，其余委托给内置实现
  - name: webfetch
    go: webfetch_allowlist
    env:
      ALLOW_HOSTS: docs.example.com,*.example.org

naming:
  profiles:
    claude:
      bash: Bash
      read: Read
      write: Write
      edit: Edit
      grep: Grep
      find: Glob
      todo: { name: TodoWrite }
      webfetch: WebFetch
      websearch:
        name: WebSearch
        description: Search the web for up-to-date information.
    snake:
      webfetch: web_fetch
      websearch: web_search
  rules:                   # 自上而下，第一条命中生效
    - model: "*claude*"
      profile: claude
    - provider: codebuddy-proxy
      profile: snake
```

`tools` 每项的规则：

- 恰好有 `command` 或 `go` 中的一个。
- 与内置同名的项替换内置实现（沙箱命令替换时必须自带 `description` / `schema`；Go 替换时可以不写，沿用内置的）。
- `env` 的值只允许 `${VAR}` 形式，或者字面量和 `${VAR}` 混写；引用的变量在启动时必须存在，否则报错，避免"配了却没生效"。

## 4. 沙箱命令工具

### 4.1 执行方式

每个会话已经有一个常驻的 bwrap 容器（live 沙箱）：第一次执行命令时启动，里面跑着一个 `/bin/sh`，空闲 `-idle`（默认 30 分钟）后回收。bash 的每条命令都以文本形式写进这个 shell，由一个新的 `sh -c` 执行。

扩展命令走**同一条路径**（`runLiveJobStream`），不另起 bwrap：

- 和 bash 完全同一个环境：工作区、home、`/tmp`、工具链、进程都共享。
- 容器没在运行时（从没执行过命令，或已被空闲回收），和 bash 一样先把它拉起来。
- 容器的 stdin 是下发命令的控制通道，命令自身的 stdin 是 `/dev/null`，stderr 也并入了 stdout。所以要做下面的处理：
  - **传参**：服务端把参数 JSON 写进会话目录下的临时文件，该目录在容器里可见；命令以 `< 该文件` 读 stdin，结束后删除。
  - **分开 stdout/stderr**：命令的 stderr 重定向到另一个临时文件，结束后读回。

### 4.2 约定

| 项 | 约定 |
|----|------|
| 参数 | 参数 JSON 从 stdin 读到，同时放在环境变量 `PIGO_TOOL_ARGS` |
| 其它环境变量 | `PIGO_TOOL_NAME`、`PIGO_SESSION_ID`、配置里的 `env`，只加在这一条命令上；其余沿用容器环境（PATH 里工具链在前） |
| 工作目录 | `/workspace` |
| 结果 | stdout 是给模型的文本（截断规则同 bash） |
| 失败 | 退出码非 0：结果为 `exit N` + stderr 尾部，标记为错误 |
| 超时 | 按工具配置，缺省 2m、上限 10m；超时的处理和 bash 一样 |
| 流式 | 运行中 stdout 像 bash 一样节流推到活动日志 |

### 4.3 密钥的可见范围

`env` 只加在这一条命令上，bash 的其它命令不会继承。但这条命令运行期间，它和模型的 bash 是同一容器里的同一用户：通过 `/proc/<pid>/environ`、`ps` 或命令行，模型能读到这些值。

- 适合 `env` 的：可以让模型看到的配置，或者泄露了影响不大的 token。
- 不能让模型接触的密钥：用 Go 扩展。它在宿主上运行，不进容器。

参数先按 `schema` 校验（注册表本身就会做），命令拿到的都是合法 JSON。

## 5. Go 扩展（`cmd/pigo-server/ext`）

`ext` 是独立包，不能 import `package main`，所以只定义接口，由服务端提供实现：

```go
package ext

// Session 是一次工具调用所属的会话。
type Session interface {
	ID() string
	UserID() string
	Workspace() string // 宿主上的工作区路径
	// Run 在该会话的沙箱里执行命令（与沙箱命令工具同一套机制）。
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// Factory 为一个会话构造工具。builtin 是被覆盖的内置实现（新增时为 nil），
// env 是配置里该项的 env（已展开）。
type Factory func(s Session, builtin agentcore.AgentTool, env map[string]string) (agentcore.AgentTool, error)

func Register(name string, f Factory) // 在 init() 里调用；重名 panic
```

- 注册 ≠ 启用：只有配置文件里 `go: <名字>` 引用到的才会挂上。
- 挂载名由配置的 `name` 决定。工厂返回的工具如果名字不同，服务端会包一层，用配置里的名字。
- 可选接口 `Detailer`（`Detail(args json.RawMessage) string`）定义它在活动日志里显示什么。
- `cmd/pigo-server/ext/all` 负责 blank import 仓内全部扩展，main 只 import 这一个；附一个示例扩展（包装内置 webfetch，加域名白名单），同时作为测试。

## 6. 工具集的装配

`hostTools` 改为：

1. 构造内置工具（现状）。
2. 按配置顺序应用扩展：同名替换，否则追加。
3. 按 `-tools` 过滤（`all` 包含扩展）。

依赖工具名的地方不需要改：规范名没变，`hasHostBash`、`toolDetail`、提示词里的工具说明继续认规范名。活动日志摘要的规则：

- 规范名是内置名，且没被 Go 扩展声明 `Detailer`：沿用内置显示。
- 其它情况：`detail` 模板 → `Detailer` → 第一个字符串参数。

## 7. 命名映射

### 7.1 在哪做

在 `meterStreams` 里给 `StreamFn` 再包一层（最内层，紧贴 provider）：

```
guardStream( meter.wrap( nameMap( provider ) ) )
```

每次调用时按会话当前的 provider + 模型选 profile。所以中途换模型会自动换一套名字，transcript 里始终是规范名。

请求方向（规范名 → 模型名）：

- `llm.Tools`：每个工具包一层，`Name()` 和 `Description()` 返回映射后的值，Schema、Execute 原样。
- `llm.Messages`：复制一份再改写，不动原消息。助手消息里 `ToolCallContent.Name`、`ToolResultMessage.ToolName` 换成模型名。
- `llm.SystemPrompt` 和各工具描述里提到工具名的地方，见 7.2。

响应方向（模型名 → 规范名）：

- 流里的 `StreamToolCallEvent.Partial`、`StreamDoneEvent.Message`、`StreamErrorEvent.Message` 中的 `ToolCallContent.Name` 换回规范名。
- 模型返回一个映射表里没有的名字：原样放过，由注册表报"未知工具"，行为和现在一样。

### 7.2 提示词改写

只替换有明确形态的提及，不做全文单词替换（`read`、`find` 这类词在正文里太常见）：

- `the <name> tool` → `the <Mapped> tool`（例：`the todo tool` → `the TodoWrite tool`）
- 反引号包裹的 `` `<name>` `` → `` `<Mapped>` ``

作用于系统提示词和每个工具的描述。服务端自己拼的提示（工具链说明等）也用这两种形态写。

## 8. 可见性

- `/healthz` 和运行能力面板列出最终工具集：每个工具的规范名和来源（内置 / 沙箱命令 / Go / 替换了内置）。
- 面板显示当前会话模型命中的命名 profile。
- 启动日志逐条打印加载的扩展和规则。

## 9. 实施步骤

1. **配置**：YAML 解析与校验（名字格式、`command`/`go` 二选一、env 展开、profile 与规则、映射重名）。
2. **沙箱命令工具**：走会话容器（`runLiveJobStream`），参数文件作 stdin、stderr 单独落文件、单条命令的 env、超时、输出流。
3. **`ext` 包**：注册表、`Session` 实现、示例扩展。
4. **装配**：`hostTools` 替换/追加、`-tools` 与扩展名、活动日志 `detail`。
5. **命名映射**：StreamFn 包装（请求改写、流回写）、提示词改写。
6. **可见性**：healthz、面板、启动日志。
7. **测试**（第 10 节）与 e2e。

## 10. 测试

- **配置**：各类非法配置都被拒绝，报错信息指明出错的项。
- **沙箱命令工具**（真 bwrap）：
  - 参数经 stdin 送达；
  - 和 bash 用的是同一个容器（同一个 `/tmp`）；
  - `env` 只加在这一条命令上，之后 bash 的 `env` 里没有；
  - stdout 与 stderr 分开，非 0 退出码被标记为错误；
  - 超时处理和 bash 一致；
  - 容器被回收后再次调用会重新拉起。
- **替换**：替换 websearch 后，注册表里只有一个 websearch，调用走到扩展。
- **Go 扩展**：示例扩展包装内置 webfetch，白名单外的地址被拒绝，白名单内的委托给内置实现。
- **映射**：
  - 用假 provider 抓请求：工具声明、历史消息、提示词都是模型名；
  - 流回来的 tool call 被改回规范名，transcript 里是规范名；
  - 同一会话换一个模型后，名字随之切换。
- **e2e**：9988 + 假模型，模型用 `Bash` / `WebSearch` 调用，活动日志和 transcript 显示规范名。

## 11. 风险

- **提示词改写不完整**：上游以后加的提示如果用别的写法提到工具名，就不会被改写。影响只是提示和声明的叫法不一致，模型通常仍能对上。
- **命令工具的 env 对模型可见**：运行期间同容器进程能读到（见 4.3）。真正的密钥要用 Go 扩展。
- **和 bash 共用一个 shell 通道**：常驻 shell 没有加锁，能安全，是因为 bash 声明了 `ToolExecutionSequential`，循环会把整批调用串行执行。命令工具必须同样声明成 sequential，否则两条命令同时写进同一个 shell，输出会串。代价是：只要一批调用里有命令工具，整批都串行。
- **Go 扩展运行在宿主上**：不受沙箱约束，安全边界由扩展自己负责。这是选择 Go 的代价，文档里写明。
