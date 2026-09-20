# SPEC: 沙箱工具链挂载（专用 Python 解释器及其它工具）

- 状态：已实现（2026-09-19）。第 4 节按建议确认；构建脚本（第 5 节）与挂载（第 6 节）均已完成并端到端验证
- 范围：pigo-server 的 bwrap 沙箱（`cmd/pigo-server/sandbox.go`）。CLI/TUI 不涉及，不改上游 `internal/` 代码。
- v2 的变化：v1 为了兼容 venv，要求与宿主机同路径挂载，并顺着软链接补挂底层解释器。实测发现可以直接构建一份**可整体拷贝、可任意路径挂载**的独立 Python 环境，于是改为：工具链必须是这样的独立目录，统一只读挂到 `/opt/<名字>`。挂载逻辑因此大幅简化，也不再向沙箱暴露宿主机的目录结构。

## 1. 背景与现状

模型的 `bash` 工具跑在每个会话一个的 bwrap 沙箱里。沙箱当前的挂载（`sandbox.go` 的 `mountArgs`）：

| 沙箱内路径 | 来源 | 权限 |
| --- | --- | --- |
| `/usr` `/bin` `/lib` `/lib64` | 宿主机同名目录 | 只读 |
| `/etc/ssl` `/etc/resolv.conf` 等少量配置 | 宿主机 | 只读 |
| `/workspace` | 本会话工作区 | 读写 |
| `/home/pigo` | 本会话独立 home | 读写 |
| `/tmp` | tmpfs | 读写 |

`PATH=/tmp:/usr/local/bin:/usr/bin:/bin`，环境变量全部清空后只注入少数几个。

2026-09-19 在本机用同样的参数实测：

- 能用 Python，但只有系统自带的 `/usr/bin/python3`（3.10.12）。标准库齐全，第三方包只有 apt 装的约 80 个，**没有 pandas、PDF 解析等常用包**。
- 管理员自己用 uv/pyenv/fnm/go 安装的工具都在宿主机 home 下。home 没有挂进沙箱，所以**沙箱里看不到这些工具**。
- 沙箱没有隔离网络，`pip install --user` 能联网装到本会话的 `/home/pigo`。但每个会话要各装一遍，版本也不受控。

## 2. 目标

1. 提供一个**专用 Python 环境**，预装解析 Excel、PDF、Word 等文件的常用包，出现在每个沙箱里，并排在 PATH 最前面。
2. 同一机制可以挂载**其它工具链**（node、go 等自带 `bin/` 的独立目录），不用每加一种就改一次代码。
3. 除了明确指定的目录，宿主机的 home 以及其中的密钥、凭据，沙箱仍然一概看不到。

## 3. 非目标

- 不在网页上配置。挂载属于主机权限，和 `-tools` 一样只由启动参数控制。
- pigo-server 不负责安装或更新工具链；Python 环境由第 5 节的脚本构建，其它工具链由管理员准备。
- 不做按用户或按会话选择不同的环境（见 4.3）。
- 不支持 venv 等依赖外部路径的环境（见 6.2 的可移植性检查）。

## 4. 决定（已按建议确认）

| # | 问题 | 选项 | 建议 |
| --- | --- | --- | --- |
| 4.1 | 沙箱里能不能往专用环境装包 | A. 只读，常用包由管理员预装；B. 每个会话在 `/home/pigo` 下叠一层可写的环境 | **A**。环境统一、可复现，实现简单。模型临时需要的包可以 `pip install --user` 装进本会话的 home（见 6.4） |
| 4.2 | 支持几个工具链 | A. 只支持 Python；B. 多个具名挂载（`python=…`、`node=…`） | **B**。机制相同，代价很小 |
| 4.3 | 能否按用户或会话选环境 | A. 全站统一；B. 可选 | **A**，本期不做选择 |
| 4.4 | 挂载位置 | A. 与宿主机同路径；B. 统一挂到 `/opt/<名字>` | **B**（v1 为 A）。工具链要求可整体移动，挂在中性的固定路径即可，模型看不到宿主机的用户名和目录结构 |
| 4.5 | 配置写错时怎么办 | A. 拒绝启动；B. 记日志后忽略 | **A**。管理员明确配置的东西静默失效，比启动失败更难排查 |

## 5. 专用 Python 环境的构建（已完成）

### 5.1 为什么不用 venv

venv 的 `bin/python` 是指向 uv 缓存（`~/.local/share/uv/python/…`）的软链接，`pyvenv.cfg` 也用绝对路径记着底层解释器，离开本机或换个路径就失效。

uv 管理的 CPython（python-build-standalone 版本）本身是一整套独立安装。2026-09-19 实测 3.12.13：

- 目录里 1048 个软链接全部是内部的相对链接（如 `python3 -> python3.12`），没有指向目录外的；
- 运行时只依赖系统 glibc（`libc`、`libm`、`libpthread` 等，沙箱已挂载 `/lib`）。

所以直接复制这份安装、把包装进去，就得到一个可整体拷贝的环境。

### 5.2 构建脚本

`cmd/pigo-server/sandbox-python/build.sh`，包清单在同目录的 `requirements.txt`。

```
cmd/pigo-server/sandbox-python/build.sh [-p 3.12] [-o 目标目录] [-r 包清单] [-f]
```

| 步骤 | 做什么 |
| --- | --- |
| 1 | `uv python install` 准备 CPython，定位它的真实安装目录 |
| 2 | 复制到目标目录旁的临时目录里构建，失败不留半成品 |
| 3 | 删除 uv 加的 `EXTERNALLY-MANAGED` 标记（这份复制品不再归 uv 管理） |
| 4 | `uv pip install --system --link-mode=copy -r requirements.txt`。`copy` 保证文件真正复制进来，不硬链接到 uv 缓存 |
| 5 | 改写 `bin/` 下命令脚本（`pip`、`pdfplumber` 等）的开头：uv 写进去的是解释器的绝对路径，改为按脚本自身所在位置查找。uv 有两种写法：路径短时是一行 `#!/路径/python`；路径超过内核对这一行的 127 字节限制时，改用 `/bin/sh` 包一层。两种都要处理 |
| 6 | 可移植性检查：有指向目录外的软链接，或者还有文本文件写着构建路径，就报错退出 |
| 7 | 把实际装上的包及其版本写进产物的 `PIGO-PACKAGES.txt`，方便复现 |
| 8 | 换上新环境。旧环境不立刻删除，留作 `<目标>.prev`：正在运行的沙箱挂载的是旧目录，删掉会让那些会话里的 Python 突然消失。下次重建时再删 |
| 9 | 在最终位置做冒烟测试：导入关键包、运行改写过的命令 |

- 默认目标目录：`$PIGO_SANDBOX_PYTHON`，未设置时为 `~/.local/share/pigo/sandbox-python`。放到 `/opt` 下需要先给上级目录写权限。
- 以后加包：改 `requirements.txt`，带 `-f` 重新构建。

### 5.3 预装的包

表格：`pandas`、`openpyxl`、`xlrd`、`xlsxwriter`；PDF：`pdfplumber`、`pypdf`、`pymupdf`；Office：`python-docx`、`python-pptx`；网页与文本：`lxml`、`beautifulsoup4`、`requests`、`chardet`、`tabulate`。不锁版本，实际版本见每次构建产物的 `PIGO-PACKAGES.txt`。

### 5.4 实测结果（2026-09-19）

- 构建约 11 秒（包走阿里云镜像），产物 373MB。
- 把产物移到别的路径，再只读挂进 bwrap 的 `/opt/pigo-python`（只挂系统目录和这一个目录）：
  - 解释器从 `/opt/pigo-python/bin/python3` 启动；
  - pandas 读写中文 Excel、pdfplumber 和 pypdf 提取 PDF 文字、python-docx 读写 Word 都正常；
  - 改写过的 `tabulate`、`chardetect`、`pdfplumber` 命令都能直接运行；
  - 沙箱里看不到宿主机的 home。
- 已存在时不加 `-f` 会拒绝；加 `-f` 重建会把旧环境留作 `.prev`，不留临时目录。
- PyMuPDF 推荐 `import pymupdf`；旧的 `import fitz` 仍可用，但会打印弃用提示。

## 6. 挂载设计

### 6.1 配置

启动参数，可以重复写多次：

```
-sandbox-tool python=/home/devai/.local/share/pigo/sandbox-python
-sandbox-tool node=/opt/node-v22
```

也可以写在环境变量里，用逗号分隔：`PIGO_SANDBOX_TOOLS="python=…,node=…"`。

- 名字限定为小写字母、数字和短横线，决定沙箱内的挂载点 `/opt/<名字>`（Python 固定叫 `python` 时就是 `/opt/python`）。
- PATH 按配置的顺序，依次把各个 `/opt/<名字>/bin` 排在默认 PATH 前面。

### 6.2 启动时的检查（不通过就拒绝启动，见 4.5）

1. 目录存在，且其下有 `bin/`。
2. 不是过宽的目录：`/`、宿主机用户的 home 本身、`/home/<任意用户>` 本身，一律拒绝。
3. **可移植性**：目录里不能有绝对路径的软链接，也不能有指向目录外的软链接。这样的环境（比如 venv）挂到 `/opt/<名字>` 后会失效，应改用第 5 节的方式构建。遍历一次目录树即可；Python 环境约 1000 个软链接，耗时可以忽略。
4. 名字不重复。

### 6.3 挂载与环境变量

每个工具链：`--ro-bind <宿主机目录> /opt/<名字>`。

| 变量 | 值 | 原因 |
| --- | --- | --- |
| `PATH` | `/opt/<名字>/bin:…:/tmp:/usr/local/bin:/usr/bin:/bin` | 专用工具优先于系统同名命令 |
| `PYTHONDONTWRITEBYTECODE` | `1`（挂了含 `bin/python3` 的工具链时） | 挂载是只读的，不设的话每次 import 都会尝试写 `.pyc` |

不再需要 v1 的 `VIRTUAL_ENV`，也不需要补挂底层解释器。

### 6.4 在沙箱里装包的行为（按 4.1 选 A）

- 专用环境是只读的，包不会装进去。
- 已验证：构建时删掉了 `EXTERNALLY-MANAGED`，所以直接 `pip install 包名` 时，pip 发现专用环境不可写，会自动改为用户安装，装到本会话 home 的 `~/.local` 下，只对这个会话生效。系统提示里的工具链说明告诉了模型这个用法，并列出预装的包（取自构建产物的 `PIGO-PACKAGES.txt`）。

### 6.5 可观测

- 启动日志逐项打印：名字、宿主机目录、挂载点。
- `/healthz` 和设置页"运行状态"显示已挂载的工具链。

## 7. 改动清单

| 文件 | 改动 |
| --- | --- |
| `cmd/pigo-server/sandbox-python/build.sh`、`requirements.txt` | **已完成**：构建专用 Python 环境 |
| `cmd/pigo-server/sandbox_tools.go`（新） | 解析配置、启动时检查（6.2）、生成挂载参数和 PATH |
| `cmd/pigo-server/sandbox.go` | `Sandbox` 增加工具链列表；`mountArgs` 追加只读挂载、环境变量，拼接 PATH |
| `cmd/pigo-server/main.go` | 增加 `-sandbox-tool` 参数和 `PIGO_SANDBOX_TOOLS`；在 `newAPIServer` 里解析，出错则拒绝启动；打印启动日志；`/healthz` 增加对应字段 |
| `frontend/src/App.tsx` | "运行状态"面板显示已挂载的工具链 |
| `cmd/pigo-server/sandbox_tools_test.go`（新） | 见第 8 节 |
| 部署说明 | 构建脚本的用法、`-sandbox-tool` 的配置示例 |

不改上游 `internal/` 下的任何文件。

## 8. 测试计划

| 层 | 测什么 |
| --- | --- |
| 解析（单元） | 名字与路径的解析；非法名字、重名、缺少 `bin/` 时报错；拒绝 `/`、home、`/home/x`；含绝对路径软链接或指向目录外的软链接时报错；普通内部相对软链接放行 |
| 参数（单元） | 生成的 bwrap 参数含 `--ro-bind <目录> /opt/<名字>`；PATH 顺序正确；只在有 Python 时设置 `PYTHONDONTWRITEBYTECODE` |
| 真实沙箱（集成，没有 bwrap 时跳过） | 用一个小的假工具链（`bin/` 下放一个脚本）验证挂载和 PATH；往挂载目录写文件失败；宿主机 home 不可见 |
| 构建脚本（手工，已做） | 见 5.4 |
| 端到端（手工） | 用构建好的环境起一个验证实例，在网页里让模型处理一个 Excel 和一个 PDF；验证 `pip install --user` 的行为（6.4） |

## 9. 实施顺序

1. 构建脚本（**已完成**）。
2. 解析、检查与挂载（第 7 节第 2～3 项），以及对应的单元测试和集成测试。
3. 启动参数、日志、`/healthz`。
4. "运行状态"面板、部署说明，端到端验证。

## 10. 风险与注意

- **暴露面**：被挂载的目录对模型只读可见。管理员要确认这些目录里没有密钥，比如不要把含 `.env` 的项目目录当成工具链。
- **更新时的旧环境**：重建时旧环境留作 `.prev`，正在运行的沙箱继续使用它，新启动的沙箱用新环境。确认旧沙箱都已回收（`-idle` 默认 30 分钟，或重启服务）后再删 `.prev`。**不要直接在原目录里升级包**：正在运行的沙箱会看到半更新的状态。
- **架构与系统**：构建产物只能用于 x86_64 Linux（与构建机相同的架构）。python-build-standalone 对 glibc 版本要求很低，常见发行版都能用。
- **体积**：产物约 370MB，每台服务器一份，所有会话共用。
- **现有的 `/usr/local/bin` 暴露**：它现在整个对沙箱可见，本机里有 47 个工具。这和本计划无关，但属于同一类问题，可以另行评估是否收紧。

## 11. 沙箱环境变量（管理员可配置，2026-09-20）

容器以 `--clearenv` 启动，只有 `HOME`、`PIGO_HOME`、`PATH`、`USER`、`TERM` 由沙箱自己设置——服务端的环境变量（包括各家 Provider 的 Key）不会进去。需要代理或内网地址的工具因此也拿不到配置，于是加一份管理员维护的注入列表。

- 存放在 settings 文档的 `sandboxEnv`（`cmd/pigo-server/sandbox_env.go`），`sandboxSpec` 把它作为 `RunSpec.Env` 传给 `mountArgs`，逐条变成 `--setenv`。
- 校验：变量名 `[A-Za-z_][A-Za-z0-9_]{0,63}`；值非空、不含换行和空字符、不超过 4096 字符；`HOME`、`PIGO_HOME`、`PATH`、`LD_PRELOAD`、`LD_LIBRARY_PATH`、`LD_AUDIT` 拒绝（前三个由沙箱设置，后三个会改变容器里每条命令的加载行为）。
- 接口（管理员）：`GET /api/admin/sandbox-env`、`PUT /api/admin/sandbox-env`（按名新增或覆盖）、`DELETE /api/admin/sandbox-env/{name}`。返回里的 `runningOld` 是仍在运行的容器数（它们保留启动时的环境变量），`processHint` 是服务端自己的 `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY`，前端做成一键填入。
- UI：设置 → 部署 → 「沙箱环境变量」。写清楚"任何用户都能在自己的沙箱里用 `env` 读到"——这里不能放 Key。
- 生效范围：新建的容器立即生效；已运行的容器要等闲置回收或重启。命令工具（tools.yaml）在同一个容器里跑，自然继承；工具自己的 `env:` 优先级更高。
- 典型用途：`HTTPS_PROXY=http://192.168.50.42:10808` + `NO_PROXY=localhost,127.0.0.1,192.168.0.0/16`，让容器里的 curl / 抓取类工具能出网。代理地址必须带端口，curl 对省略端口的代理默认用 1080。
