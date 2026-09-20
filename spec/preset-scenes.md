# 预设场景（Scenes）

状态：已实现（P1～P3），已在 9988 上用假模型和测试工具走通

## 1. 目标

管理员预先定义一批「场景」，每个场景用一段提示词让模型专门做一类事，例如「查询某地区药品/诊疗项目的医保自付比例」。场景用到的数据不放进提示词里，也不上传资料文件：查数据的能力注册成工具（tools.yaml），由场景提示词告诉模型什么时候调用、怎么调用，类似 skill 的做法。

同一个场景有两种用法：

| 用法 | 入口 | 效果 |
|---|---|---|
| 场景会话 | 欢迎页卡片、「新会话」下拉 | 新建一个会话；场景提示词追加到 system prompt，整个会话按场景工作，并带上场景的工具和默认模型 |
| 单次命令 | 在任意会话输入 `/slug 问题` | 只作用于这一轮：提示词展开成本轮的用户消息，场景的工具只在本轮可用 |

## 2. 已确认的决定

| 问题 | 决定 |
|---|---|
| 数据来源 | 注册成工具（tools.yaml 命令工具或 Go 扩展），提示词指示模型调用；也可让模型用 web_search / web_fetch 联网 |
| 用法 | 场景会话和单次命令都要 |
| 谁能建 | 第一版只做公共场景，由管理员维护 |
| 参数输入 | 只用自由文本，不做表单；提示词里约定信息不全时向用户追问 |
| 输出格式 | 开放：由提示词自行约定，程序不做结构校验 |
| 模型与工具 | 场景给出默认模型和推理强度，会话里仍可切换；工具范围固定为场景声明的集合 |
| 提示词可见性 | 公开，普通用户能看到完整提示词 |
| 入口 | 欢迎页卡片、「新会话」按钮下拉 |
| 命令名 | 英文 slug（如 `/yibao`），列表里显示中文名，中文也能搜到 |
| builtinskills | 暂不导入 |

## 3. 数据模型

场景存放在 settings 文档里，和 `ModelPrices`、`ModelParams` 一样，由单独的接口维护：

```go
type scene struct {
    Slug        string   `json:"slug"`        // 命令名，[a-z0-9-]{2,32}，唯一，不能与内置命令重名
    Name        string   `json:"name"`        // 中文名，如「医保自付查询」
    Icon        string   `json:"icon"`        // 一个 emoji 或 1～2 个字
    Description string   `json:"description"` // 一句话，卡片和列表上展示
    Prompt      string   `json:"prompt"`      // 提示词正文（Markdown）
    Examples    []string `json:"examples"`    // 示例问法，卡片上点击即填入输入框
    Tools       []string `json:"tools"`       // 场景可用的工具；空 = 与普通会话相同
    Model       string   `json:"model"`       // 默认模型，可空（用部署默认）
    Provider    string   `json:"provider"`
    Thinking    string   `json:"thinking"`    // 默认推理强度，可空
    Disabled    bool     `json:"disabled"`    // 停用：不再出现在入口，已有会话不受影响
    Order       int      `json:"order"`       // 卡片排序
    UpdatedAt   time.Time `json:"updatedAt"`
}
```

### 场景专用工具

tools.yaml 的工具新增字段 `scope`：

```yaml
tools:
  - name: yibao_query
    scope: scene          # 只在声明了它的场景里出现，普通会话看不到
    description: 查询某地区某药品/诊疗项目的医保类别与个人自付比例
    command: /opt/pigo-tools/bin/yibao-query
    schema: { ... }
```

- `scope` 为空或 `all`：和现在一样，所有会话都有。
- `scope: scene`：只有 `Tools` 里列了它的场景（会话或单次命令）才会注册给模型。
- 场景保存时校验 `Tools` 里的名字都存在（内置工具或 tools.yaml 里的工具）；tools.yaml 改动后找不到的工具，在场景列表里标红提示。

## 4. 场景会话

- `POST /api/sessions` 请求体新增 `scene`（slug）。服务端把场景当时的内容快照进 `sessionMeta.Scene`（slug、名称、提示词、工具、快照时间）。之后修改场景不影响已有会话，前缀缓存也保持稳定。
- 默认模型：请求没指定模型时，优先用场景的默认模型，其次用部署默认。
- system prompt：在现有提示词末尾追加一段：

  ```
  ## 当前场景：医保自付查询
  <场景提示词>
  ```

- 工具：`hostTools` 在现有流程（内置 → 扩展 → -tools 选择）之后，按快照里的 `Tools` 过滤，并加入场景声明的 `scope: scene` 工具。
- 设置 → 会话 的信息里显示场景名，场景会话的欢迎页显示场景说明和示例；历史会话恢复时从快照重建，不读取最新的场景定义。

## 5. 单次命令

- 输入 `/yibao 北京 职工医保 阿莫西林胶囊`：`handleMessage` 在内置命令之前先查场景（场景名不能与内置命令重名），把本轮用户消息展开为：

  ```
  <scene slug="yibao" name="医保自付查询">
  以下是本轮任务的场景说明，请按它完成用户的问题。

  <场景提示词>
  </scene>

  用户问题：北京 职工医保 阿莫西林胶囊
  ```

- 场景声明的工具只在本轮加入工具集，本轮结束后恢复。
- 只打 `/yibao` 不带问题：回复场景说明和示例问法，不调用模型。
- transcript 里保存展开后的消息。历史接口据此把它还原成原始命令 `/yibao …` 并带上 `scene` 字段；聊天界面的用户气泡显示原始命令和「场景：名称」标记，完整提示词在 设置 → 场景 查看。
- 场景命令出现在 `/help` 和现有的命令列表里（`source: "scene"`）。

## 6. 接口

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/api/scenes` | 登录用户 | 已启用的场景，含提示词（公开），以及可选的工具列表 |
| GET | `/api/admin/scenes` | 管理员 | 全部场景，含停用的；`missingTools` 标出工具配置里已没有的工具 |
| PUT | `/api/admin/scenes/{slug}` | 管理员 | 路径为 `new` 时新建；否则保存到该场景上，请求体里的 slug 与路径不同即改名。停用/启用、排序也走这里 |
| DELETE | `/api/admin/scenes/{slug}` | 管理员 | 删除，已有会话的快照不受影响 |
| POST | `/api/sessions` | 登录用户 | 请求体新增 `scene` |

## 7. 前端

- **欢迎页卡片**：新会话的空白页展示已启用场景的卡片（图标、名称、描述、1～3 个示例问法）。
  - 点击卡片：以该场景新建会话。
  - 点击示例问法：新建会话，并把问法填入输入框（不自动发送）。
- **「新会话」下拉**：按钮保持一键新建普通会话，旁边的下拉箭头列出场景。
- **设置 → 场景**：
  - 所有用户可浏览场景和完整提示词。
  - 管理员可以编辑：名称、slug、图标、描述、提示词、示例、工具（多选，场景专用工具单独分组）、默认模型/推理强度（复用部署页的模型选择器）。
  - 提示词下方可展开预览：场景会话追加到 system prompt 的内容。
  - 「从示例创建」把医保示例填进编辑器。
- **会话内**：场景会话的欢迎页显示场景说明，示例问法点击即填入输入框；设置 → 会话 的信息里显示当前场景；场景命令的用户气泡带「场景：名称」标记。

## 8. 示例场景：医保自付查询

- slug `yibao`，工具 `yibao_query`（场景专用，数据接口由部署方提供）以及 `websearch`、`webfetch`。
- 提示词要点：
  1. 需要的信息：地区（省/市）、参保类型（职工/居民）、药品或诊疗项目名称；缺什么就先问，不猜。
  2. 先调 `yibao_query`；查不到或结果可能过期时，再联网查当地医保局的官方文件。
  3. 回答要写清：医保类别（甲/乙/丙）、个人先行自付比例、报销限制（限定支付范围、限二级以上医院等），以及依据来源和日期。
  4. 声明结果仅供参考，以当地医保经办机构为准。
- 真实数据接口由部署方按 `spec/tool-extensions-cmd-guide.md` 接成 `scope: scene` 的命令工具。

## 9. 开发计划

| 阶段 | 内容 | 验证 |
|---|---|---|
| P1 服务端 | scene 数据结构、校验与存储；admin/公共接口；tools.yaml `scope`；创建会话带场景：快照、默认模型、system prompt、工具集；单次命令的展开与本轮工具；`/help` 列出场景 | 单元测试：slug 校验、工具校验、快照不随修改变化、scene 工具在普通会话不可见、单次命令本轮加工具下一轮消失、fake LLM 收到的 system prompt 与 tools |
| P2 前端 | api.ts 类型与方法；欢迎页卡片；新会话下拉；设置 → 场景（浏览/编辑）；用户气泡显示原始命令；会话信息显示场景 | tsc、build；9988 上截图检查 |
| P3 示例与文档 | 设置 → 场景 的「从示例创建」按钮，把「医保自付查询」示例填进编辑器（由管理员确认后保存，不在启动时自动写库）；`scope` 写进 tools.yaml 文档；更新本文档状态 | 9988 上用假模型和测试版 yibao_query 走通：卡片示例 → 场景会话（场景模型、提示词、三个工具）→ 调工具 → 回答；`/yibao` 单次命令只在本轮带工具；历史还原命令 |

不做（第一版）：个人场景、参数表单、结构化输出、资料文件上传、builtinskills 导入、场景使用统计。

## 10. 实现时的取舍

1. 场景命令出现在 `/help` 和命令浏览器（「场景」分组），不单独做补全 UI。
2. `scope: scene` 的工具只在场景里出现，管理员的普通会话也没有。
3. 示例场景的数据工具 `yibao_query` 由部署方提供；示例里勾选它的前提是 tools.yaml 里有它，没有时「从示例创建」只保留 websearch / webfetch。
4. 从欢迎页卡片开启场景会话会新建一个会话，当前的空会话保留在历史里（和「新会话」按钮的行为一致），不自动删除。

## 11. 代码索引

| 位置 | 内容 |
|---|---|
| `cmd/pigo-server/scenes.go` | 场景结构、校验、存储、快照、system prompt 段、单次命令的展开与解析、HTTP 接口 |
| `cmd/pigo-server/tools_config.go` | `scope` 字段 |
| `cmd/pigo-server/ext_tools.go` | `applyExtensions` 跳过场景工具；`sceneTools` 按名构建 |
| `cmd/pigo-server/hostloop.go` | `hostTools` 场景过滤；system prompt 追加；本轮的场景工具 |
| `cmd/pigo-server/main.go` | 建会话带场景、`handleMessage` 的场景命令、`/help`、命令列表、`sessionJSON` |
| `cmd/pigo-server/history.go` | 历史里还原场景命令 |
| `frontend/src/scenes.tsx` | 设置 → 场景 |
| `frontend/src/App.tsx` | 欢迎页卡片、场景会话欢迎页、「新会话」下拉、用户气泡的场景标记 |
