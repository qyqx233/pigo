# 交接文档：用外部命令接入工具

面向要给 pigo-server 加工具的人。读完能写出一个命令工具，挂上去、调通、排错。设计背景和取舍见 [tool-extensions.md](tool-extensions.md)。本文只讲怎么用。

## 1. 一句话说明

在 YAML 配置里声明一个工具（名字、描述、参数 schema、一条命令）。模型调用它时，pigo 在**该会话的沙箱容器**里执行这条命令：

- 参数 JSON 写到命令的 **stdin**；
- 命令打印到 **stdout** 的内容就是给模型的结果；
- **退出码非 0** 表示失败。

任何语言都行，只要容器里能跑。

## 2. 完整示例（已在 9988 上跑通）

目标：加一个 `xlsx_summary` 工具，告诉模型一个 Excel 文件有哪些 sheet、多少行、哪些列、前几行内容。

### 2.1 写脚本，放进一个工具链目录

脚本要在容器里能被找到。推荐做法是放进一个自建目录的 `bin/` 下，用 `-sandbox-tool` 只读挂进容器，它的 `bin/` 会自动排在 PATH 前面。

```
/srv/pigo-tools/
└── bin/
    └── xlsx_summary        # chmod +x
```

```python
#!/usr/bin/env python3
"""xlsx_summary: a pigo command tool.

pigo writes the call's arguments to stdin as JSON, e.g. {"path": "data/a.xlsx"}.
Whatever this prints to stdout is the tool result the model sees; a non-zero
exit marks the call as failed, and stderr is shown with it.
"""
import json
import sys

import pandas as pd

args = json.load(sys.stdin)
path = args["path"]
rows = int(args.get("rows", 5))

try:
    sheets = pd.read_excel(path, sheet_name=None)
except FileNotFoundError:
    print(f"no such file: {path}", file=sys.stderr)
    sys.exit(2)

for name, df in sheets.items():
    print(f"## {name}: {len(df)} rows x {len(df.columns)} columns")
    print("columns:", ", ".join(map(str, df.columns)))
    print(df.head(rows).to_string(index=False))
    print()
```

`#!/usr/bin/env python3` 在容器里会找到沙箱专用 Python，前提是也挂了它（见 2.3），pandas 已经预装在里面。

### 2.2 写配置

```yaml
# /etc/pigo/tools.yaml
tools:
  - name: xlsx_summary
    description: |
      Summarize an Excel workbook in the workspace: each sheet's size, columns
      and first rows. Use it before reading a workbook in full.
    schema:
      type: object
      properties:
        path: { type: string, description: Path of the .xlsx file, relative to the workspace }
        rows: { type: integer, description: How many rows to show per sheet (default 5) }
      required: [path]
    command: xlsx_summary        # 在 PATH 上（/opt/tools/bin），直接写名字
    timeout: 60s
    detail: "{path}"             # 活动日志里这次调用显示成 path 的值
```

### 2.3 启动

```sh
./pigo-server -tools all \
  -sandbox-tool python=$HOME/.local/share/pigo/sandbox-python \
  -sandbox-tool tools=/srv/pigo-tools \
  -tools-config /etc/pigo/tools.yaml \
  ...其余参数
```

- `-sandbox-tool` 的先后决定 PATH 顺序：python 在前，`#!/usr/bin/env python3` 找到的就是它。
- 也可以用环境变量：`PIGO_SANDBOX_TOOLS=python=…,tools=…`、`PIGO_TOOLS_CONFIG=/etc/pigo/tools.yaml`。

启动日志里应该看到：

```
pigo-server: sandbox tool python: …/sandbox-python mounted read-only at /opt/python
pigo-server: sandbox tool tools: /srv/pigo-tools mounted read-only at /opt/tools
pigo-server: extension tool xlsx_summary adds (command: xlsx_summary)
```

### 2.4 效果

模型调用 `xlsx_summary {"path": "t.xlsx"}`，拿到：

```
## Sheet1: 2 rows x 2 columns
columns: 项目, 价格
   项目  价格
 流感抗原  50
呼吸道六联 180
```

文件不存在时，脚本 `exit 2`，模型拿到的是：

```
tool "xlsx_summary" failed: xlsx_summary: command exited with code 2
no such file: nope.xlsx
```

## 3. 配置字段

| 字段 | 必填 | 说明 |
|------|------|------|
| `name` | 是 | 工具名：小写字母开头，只含小写字母、数字、下划线，最长 64。和内置工具同名即**替换**内置（见第 6 节） |
| `description` | 是 | 给模型看的说明。写清楚什么时候该用它，模型靠它决定调不调 |
| `schema` | 否 | 参数的 JSON Schema（用 YAML 写）。不写就是"无参数对象"；替换内置时必填。参数在到达命令之前就会按它校验 |
| `command` | 是 | 一行 shell 命令，由容器里的 `/bin/sh -c` 执行，可以带参数、管道 |
| `timeout` | 否 | Go 时长格式（`30s`、`2m`），缺省 2m，上限 10m |
| `detail` | 否 | 活动日志里这次调用显示什么：`{参数名}` 会被替换成该参数的值（字符串只取第一行）。不写就显示第一个字符串参数 |
| `env` | 否 | 只给这条命令的环境变量。值可以写 `${VAR}`，取自 **pigo-server 进程**的环境变量；引用的变量不存在则启动失败 |

配置是部署级的（所有用户、所有会话都一样），改了要重启。`-tools all` 会包含所有扩展工具；如果用 `-tools a,b,c` 列举，要把扩展工具名也写进去。

## 4. 命令的运行环境

| 项 | 内容 |
|----|------|
| 在哪跑 | 该会话的 bwrap 容器，和模型的 `bash` 是**同一个容器**。容器没在运行时会先拉起 |
| 工作目录 | `/workspace`（会话工作区）。相对路径都相对它 |
| 用户 / HOME | `pigo` / `/home/pigo`（会话私有，持久化）。`pip install --user` 装的包也在这里 |
| stdin | 参数 JSON（完整的一个对象，末尾没有换行） |
| 环境变量 | `PIGO_TOOL_NAME`、`PIGO_SESSION_ID`、`PIGO_TOOL_ARGS`（参数 JSON，超过 32KB 时不设，只走 stdin）、配置里的 `env`，加上容器原有的 `HOME`、`PATH`、`USER=pigo`、`TERM=dumb` |
| PATH | 各 `-sandbox-tool` 的 `bin/`（按参数顺序），然后 `/tmp:/usr/local/bin:/usr/bin:/bin` |
| 能看到的文件 | `/workspace`、`/home/pigo`、`/tmp`（容器私有，和 bash 共享）、只读的 `/usr` `/bin` `/lib*` `/opt/<工具链>` |
| 看不到的 | 宿主的 home 目录、其它会话、任何 provider API Key |
| 网络 | 可以访问（和宿主同一网络） |
| 并发 | 串行：一批调用里只要有命令工具，整批依次执行（和 bash 相同，因为共用容器里的一个 shell） |

## 5. 输出和出错

- **结果**：stdout，去掉末尾换行。超过 30000 字节时，只保留头 12000 和尾 12000 字节，中间标 `[truncated]`。把输出控制在模型用得上的量，大块数据写到工作区文件里，再告诉模型路径。
- **失败**：退出码非 0。模型看到 `command exited with code N`，加上 stdout 和 stderr 末尾（最多 16KB）。stderr 用来写给模型看的错误原因。
- **成功时的 stderr 会被丢弃**，不进结果，也不进日志。调试输出想留下来，就写到文件里。
- **超时**：超过 `timeout` 时，和 bash 超时一样，**整个容器会被停掉**，下次调用再拉起新的。容器里的后台进程和 `/tmp` 里的内容会随之丢失；`/workspace`、`/home/pigo` 不受影响。
- **运行中的输出**：stdout 每 0.3 秒左右推一次到网页的活动日志，所以长任务可以边跑边打印进度。

## 6. 替换内置工具

用内置工具的名字即可替换：`read`、`write`、`edit`、`grep`、`find`、`bash`、`todo`、`webfetch`、`websearch`。

```yaml
tools:
  - name: websearch
    description: Search the company knowledge base.
    schema:
      type: object
      properties:
        query: { type: string }
      required: [query]
    command: company-search      # 自己的实现
    env:
      SEARCH_ENDPOINT: ${PIGO_EXT_SEARCH_ENDPOINT}
```

- 替换时 `description` 和 `schema` 都必须自己写。内置工具的参数定义不会沿用，模型只按你的定义调用。
- 活动日志的显示沿用内置规则（`websearch` 显示 query，`bash` 显示命令……）。写了 `detail` 就以 `detail` 为准。
- 系统提示词里提到这个工具的地方（比如 "use the read tool"）仍然指向同名工具，也就是你的实现。参数语义差别大的话，在 `description` 里写清楚。

## 7. 密钥：`env` 能放什么

`env` 只加在这一条命令上，模型之后执行的 `bash` 继承不到。但**这条命令运行期间**，模型的 bash 和它在同一容器、同一用户下，通过 `/proc/<pid>/environ`、`ps` 能读到这些值。所以：

- **可以放**：服务地址、非敏感配置、泄露了影响不大的 token（比如只读、有限额的）。
- **不要放**：真正不能让模型接触的密钥。这类需求用 Go 扩展（`cmd/pigo-server/ext`，运行在宿主进程里，不进容器）。示例见 `ext/allowfetch`，设计见 [tool-extensions.md](tool-extensions.md) 第 5 节。
- 命令本身也不要把密钥打印到 stdout/stderr，那会直接成为模型看到的结果。

## 8. 给不同模型换个叫法（可选）

有的模型更习惯 `XlsxSummary` 这样的名字。在同一个配置文件里加命名 profile 即可，扩展工具和内置工具一样可以改名：

```yaml
naming:
  profiles:
    claude:
      bash: Bash
      xlsx_summary: XlsxSummary
  rules:
    - model: "*claude*"       # * 匹配任意字符（含 /），不区分大小写
      profile: claude
```

只影响发给模型的名字。transcript、活动日志、`-tools`、`detail` 里一律还是 `xlsx_summary`。

## 9. 开发和调试

**在宿主上先跑通脚本**。它就是个读 stdin 的程序：

```sh
cd 某个有 t.xlsx 的目录
echo '{"path":"t.xlsx"}' | ~/.local/share/pigo/sandbox-python/bin/python3 /srv/pigo-tools/bin/xlsx_summary
echo $?
```

**在容器里验证**：让模型（或你在对话里）用 bash 执行 `command -v xlsx_summary`、`echo '{"path":"t.xlsx"}' | xlsx_summary`，确认容器里的 PATH 和依赖都对。

**上线后检查**：

- 启动日志有 `extension tool <name> adds/replaces …`；
- `curl -s http://<host>/healthz` 的 `extensions.tools` 里有它；
- 网页 设置 → 运行状态 → "扩展工具" 行有它；
- 对话里活动日志显示它的调用和 `detail`。

**启动失败的常见报错**（配置有任何问题，服务端都会拒绝启动，报错里有出错项的名字）：

| 报错片段 | 原因 |
|----------|------|
| `field xxx not found` | 字段名写错（配置是严格解析的） |
| `needs command: or go:` / `pick one` | 没写 `command`，或和 `go` 同时写了 |
| `name must be lowercase …` | 名字不合规（大写、横线、数字开头） |
| `a command tool needs a description` | 缺 `description` |
| `… replacing a built-in needs its own schema` | 替换内置工具但没写 `schema` |
| `schema: …` | schema 不是合法的 JSON Schema |
| `timeout … exceeds the 10m0s limit` | 超时超过 10 分钟 |
| `env K: the server's environment has no VAR` | `${VAR}` 引用的环境变量在 pigo-server 进程里不存在 |
| `configured twice` | 两项同名 |
| `sandbox tool …: not a directory` / `no bin/ directory` / `absolute symlink` / `points outside` | `-sandbox-tool` 指的工具链目录不合格：要有 `bin/`，不能有绝对路径或指向目录外的软链接 |

**运行时的常见问题**：

| 现象 | 排查 |
|------|------|
| `command exited with code 127` | 容器里找不到命令：检查 `-sandbox-tool` 是否挂了、脚本在不在 `bin/`、有没有执行权限、shebang 解释器在容器里是否存在 |
| `ModuleNotFoundError` | 依赖不在沙箱 Python 里：加进 `cmd/pigo-server/sandbox-python/requirements.txt` 后重建（`build.sh -f`），或改用会话内 `pip install --user`（只对该会话有效） |
| 读不到文件 | 相对路径以 `/workspace` 为准；宿主路径在容器里不存在 |
| 写 `/opt/...` 失败 | 工具链目录是只读挂载；输出写到 `/workspace` 或 `/tmp` |
| 结果被截断 | 输出超过 30000 字节；改成写文件、只打印摘要 |

## 10. 上线前自查

- [ ] 脚本读 stdin 的 JSON，不依赖参数顺序和额外换行
- [ ] 成功时 stdout 简洁、对模型有用；失败时 `exit` 非 0 且 stderr 说明原因
- [ ] 输出量可控（远小于 30000 字节），大数据落文件
- [ ] 能在 `timeout` 内完成；长任务打印进度
- [ ] 不打印密钥；`env` 里没有不能让模型看到的值
- [ ] 只写 `/workspace`、`/home/pigo` 或 `/tmp`
- [ ] `description` 写明了何时使用、参数含义；`schema` 标了 `required`
- [ ] 在宿主和容器里都手动跑过一次

## 11. 代码索引（维护者）

| 位置 | 内容 |
|------|------|
| `cmd/pigo-server/tools_config.go` | 配置解析与校验、命名 profile 与规则 |
| `cmd/pigo-server/ext_tools.go` | 命令工具的执行（`runInSession`、`extCommandTool`）、工具集装配（`applyExtensions`）、活动日志 `detail` |
| `cmd/pigo-server/naming.go` | 发给模型时的改名（`nameStream`） |
| `cmd/pigo-server/sandbox_tools.go` | `-sandbox-tool` 工具链挂载 |
| `cmd/pigo-server/ext/` | Go 扩展注册表；`ext/allowfetch` 是示例 |
| `cmd/pigo-server/*_test.go` | `tools_config_test`、`ext_tools_test`（真 bwrap）、`naming_test` |
| `spec/tool-extensions.md` | 设计与决策 |
