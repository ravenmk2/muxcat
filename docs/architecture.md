# muxcat 总体架构

muxcat 是一个面向基础设施后端的统一命令行客户端：以内置 Connector 方式接入各类基础设施（数据库、缓存、消息队列、可观测性平台等），提供一致的连接管理、查询与结构化输出体验。本文是设计契约的落盘版，与代码不一致时以修改契约并同步本文为准。

## 三层模型

- **Connector（类型）**：一种基础设施后端的实现，如 `sqlite`。每个 connector 注册到 `internal/connector` 的注册表（名字 → cobra 命令树工厂），root 组装时遍历挂载为 `muxcat <name>` 子命令。
- **Instance（端点）**：一个具体的数据库端点，如某个 SQLite 文件路径。
- **Connection（会话，用户面缩写 `conn`）**：一条指向 instance 的使用配置（只读、超时等），是命令 `-c/--conn` 选择的对象。

## 配置

- 配置目录：`os.UserHomeDir()/.config/muxcat/`，全平台统一；环境变量 `MUXCAT_HOME` 覆盖。
- `config.json` 为主配置，`version` + `props` 两层；`props.defaults` 存 `output`/`color`/`timeout`/`limit` 等默认值。`config get/set` 的点路径对 config.json 隐式补 `props.` 前缀。
- 每个 connector 各自一个 JSON 文件（如 `sqlite.json`），`version` + `instances` / `connections` / `defaultConnection` 扁平结构。
- 文件权限 0600（Windows 上尽力而为）；写入一律临时文件 + rename 原子写，JSON 两空格缩进。
- `muxcat config validate [file...]` 用 `go:embed` 进二进制的 JSON Schema（Draft 2020-12，`schema/` 目录）校验，无参时校验配置目录下全部已知文件。

## 安全

- 主密钥随机生成存 OS keychain（`zalando/go-keyring`，service `muxcat`、key `master-key`）；`MUXCAT_KEY`（base64 编码的 32 字节）为兜底来源；两者皆无时报 `KEY_UNAVAILABLE` 并提示 `muxcat config key init`。
- 秘密加密：AES-256-GCM，随机 nonce，密文为 `enc:v1:` + base64(nonce|ciphertext)。秘密在内存中用 `[]byte` 持有。
- `--password` 类明文凭据参数保留但应 stderr 警告（后续 connector 适用；sqlite 无密码字段）。

## 输出契约

- Envelope 固定形状：
  `{ok, data, meta:{connector, connection, elapsed_ms, truncated}, error:{code, message, hint?}}`。
  顶层字段恒定——`data`/`error` 缺省时输出 `null` 而非省略；`hint` 可选；query 类命令的 data 为 `columns`（带 type）+ `rows` 二维数组 + `row_count` + `rows_affected`（查询路径为 `0`，DML 路径为影响行数）。
- 输出模式：`--output auto|table|plain|tsv|json`，`--json` 为快捷方式且优先于一切；解析优先级 flag > `props.defaults.output` > auto；auto 时 TTY→table、非 TTY→plain。
- 裸值渲染：`Result.Value` 为单键 map 且标记 `Bare` 时，文本模式（plain/tsv/table）只输出值本身、不带 `key:` 标签（如 `redis get` 直接输出值），nil 值输出空行；JSON envelope 不受影响。
- 单元格格式化：文本模式（table/plain/tsv）经 `Result.CellStyle` 格式化单元格——这是 connector 的可选声明（opt-in）；未声明（nil）时为 legacy 默认：nil → 空串、`[]byte` → string、其余 `fmt.Sprint`、无类别着色。声明后按配置渲染：`NullText`（如 `NULL`）、`BinaryHex`（二进制类型列的 `[]byte` → `0x` 大写 hex，类型取自 `Result.ColumnTypes` 的 `DatabaseTypeName()`）、`DateLayout`/`TimeLayout`（`time.Time` 布局）、`Palette`（按类别着色）。table 模式总宽超过终端宽度时从最宽列压缩单元格（`…` 尾缀，列下限保护），plain/tsv/json 不截断。
- 降级：stdin/stdout 任一为管道视为非 TTY，auto 降级 plain 且**全局禁止交互**（缺必填参数直接报 `MISSING_ARGUMENT`，绝不等待输入）。
- 颜色：`props.defaults.color`（auto|always|never）+ `--no-color` + `NO_COLOR` 环境变量共同决定；非 TTY 强制无色。真彩色靠 lipgloss/termenv 自动检测，不加独立配置。颜色开启时，文本模式下 `Result.Syntax` 指定的 chroma lexer 会对输出做语法高亮（如 redis get 的 JSON 值）。
- degraded result（携带部分数据的失败）：命令可产出部分结果仍以错误结束（如 `etcd endpoint status` 部分 endpoint 失败）。实现：`cli.RenderPartial` —— 文本模式先正常渲染部分结果到 stdout、再返回错误由 stderr 输出 `Error:`/`Hint:`；JSON 模式不渲染，把 `Result.Payload()` 挂到 `*output.Error.Data`，root 的 exitError 将其写入 failure envelope 的 `data`（ok:false + data + error）。

## 退出码与错误码

退出码集中定义于 `internal/output`：

| 码 | 含义 |
|---|---|
| 0 | 成功 |
| 1 | 通用错误 |
| 2 | 用法与缺参 |
| 3 | 连接失败 |
| 4 | 认证失败 |
| 5 | 执行错误 |

错误码枚举：`MISSING_ARGUMENT`、`CONN_NOT_FOUND`、`CONFIG_INVALID`、`KEY_UNAVAILABLE`、`CONNECT_FAILED`、`AUTH_FAILED`、`TIMEOUT`、`QUERY_ERROR`、`READONLY_VIOLATION`、`UNSUPPORTED_OPERATION`、`CONNECTOR_UNKNOWN`、`UPGRADE_FAILED`。

命令返回结构化 `*output.Error`，root 统一渲染（`--json` 时 envelope 到 stdout，否则 stderr 输出 `Error:`/`Hint:` 行，TTY 下标签高亮——Error 红色加粗、Hint 蓝色，消息体不染色）并按错误码映射退出码。映射规则：缺参/用法类 → 2；连接类（含 `TIMEOUT`）→ 3；认证类 → 4；执行类（`QUERY_ERROR`、`READONLY_VIOLATION`、`UPGRADE_FAILED`）→ 5；其余 → 1。

## 工程约定

- module `github.com/ravenmk2/muxcat`；`CGO_ENABLED=0` 为构建/测试/发布基线。
- 版本注入：`-ldflags "-s -w -X main.version=<ver>"`，代码中 `var version = "dev"` 兜底；release 由 goreleaser 按 tag 构建。
- SQLite 双驱动：`modernc.org/sqlite`（纯 Go，`!cgo`）与 `mattn/go-sqlite3`（`cgo`）由 build constraint 自动切换；DSN 只用 `_pragma=` 公共交集。

## 帮助信息约定

目标：AI Agent（与人）无需额外文档，仅凭各级 `--help` 即可学会用法。分层约定：

- **root Long**：产品定位、三步上手（conn add → 数据命令 → `-c` 切换）、全局契约指引、安全承诺一句话。
- **全局契约**用 cobra additional help topic 承载（`internal/cli/topics.go`）：`muxcat help output`（envelope、输出模式、meta 字段）、`muxcat help errors`（错误形状、错误码、退出码）。新增全局契约优先加 topic，不塞进 root Long。
- **connector / 命令组 Long**：2-4 行简介 + 首连 quickstart + 该层特有规则（如 redis 的拦截策略）。
- **leaf 命令**：`Example` 必填（1-4 条完整调用，不带 `$` 提示符，可用 `#` 注释行）；语义不直观的命令再补 `Long`（如 redis scan 的单轮模式、exec 的拦截与透传）。
- 写作原则：help 写用法、docs 写设计，互不复制；help 文本统一英文。
- 强制：规约测试遍历命令树——每个命令 `Short` 非空、命令组 `Long` 非空、runnable leaf 有 `Example` 或 `Long`；新增命令漏写即测试失败。全量树（含全部 connector）的检查在 `cmd/muxcat` 的测试里（connector 经 main.go blank import 注册，测试内含 connector 挂载断言防空转）；`internal/cli` 的同名检查覆盖框架树。


## 目录结构与模块职责

```
muxcat/
├── cmd/muxcat/            # 程序入口：组装启动、blank import 注册 connector
├── internal/
│   ├── cli/               # cobra 命令树：root、config 组、connector 挂载、统一错误出口、渲染管线
│   ├── config/            # 配置目录解析、Load/Save（原子写）、点路径 Get/Set
│   ├── secret/            # 主密钥（keychain/MUXCAT_KEY）、AES-256-GCM 加解密
│   ├── output/            # Renderer（json/tsv/table/plain）、envelope、TTY/颜色决策、退出码与错误码
│   ├── upgrade/           # 自更新：GitHub release 查询、带重试下载、checksum 校验、自替换（见 docs/upgrade.md）
│   └── connector/
│       ├── registry.go    # connector 注册表
│       ├── etcd/          # etcd connector（见 docs/connectors/etcd.md）
│       ├── jenkins/       # Jenkins connector（见 docs/connectors/jenkins.md）
│       ├── mysql/         # MySQL connector（见 docs/connectors/mysql.md）
│       ├── openobserve/   # OpenObserve connector（见 docs/connectors/openobserve.md）
│       ├── redis/         # Redis connector（见 docs/connectors/redis.md）
│       └── sqlite/        # SQLite connector（见 docs/connectors/sqlite.md）
├── schema/                # JSON Schema（go:embed）+ 校验实现
├── docs/                  # 设计文档
└── scripts/build.sh       # 本地多平台构建（--install 到 ~/.local/bin）
```
