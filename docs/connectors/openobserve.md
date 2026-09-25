# openobserve connector

OpenObserve connector 通过 OpenObserve 的 REST API（HTTP Basic Auth）接入，实现为纯 `net/http` 客户端，无外部驱动依赖（符合 `CGO_ENABLED=0` 基线）。命令名 `openobserve`，别名 `o2`。覆盖连接管理、stream 查看、数据查询（SQL / 上下文 around / 字段值 values）、日志摄入（ingest logs）、原生请求透传（request）与 OpenAPI spec 探索（apidoc）；`_bulk`、metrics/traces 摄入、users/functions/metrics 等管理类 API 不在本期范围。

API 核对的权威来源：官方文档（存在若干与实际不符之处，文中均已注明）+ 实例 Swagger UI `/swagger/index.html`（OpenAPI spec：`/api-doc/openapi.json`）+ 源码。

## 配置模型（openobserve.json）

```json
{
  "version": 1,
  "instances": {
    "local": { "url": "http://127.0.0.1:5080" }
  },
  "connections": {
    "local": {
      "instance": "local", "org": "default", "username": "root@example.com",
      "password": "enc:v1:...", "readonly": false, "timeout": "30s"
    }
  },
  "defaultConnection": "local"
}
```

- `instances`：名字 → `{url}`，纯端点属性。url 必须含 scheme（`http`/`https`，https 即 TLS），**不得内嵌 user:password@**（防明文凭据落盘，违反报 `CONFIG_INVALID`）。
- `connections`：名字 → `{instance, org?, username?, password?, readonly?, timeout?}`。`org` 缺省为 `default`；`password` 以 `enc:v1:` 加密落盘，永不在输出中回显；`timeout` 为 Go duration 字符串，覆盖全局 `--timeout`。
- **多租户/多用户**：org 与凭据都是 connection 属性——同一 instance 可挂多个连接（不同 org、不同权限用户）。
- `conn add <name>` 同名创建 instance 与 connection；`conn rm` 删除连接时，同名 instance 无其他引用则一并删除。

## 命令

### conn 组

| 命令 | 说明 |
|---|---|
| `o2 conn add <name> --url <u> [--org default] [--username u] [--password p] [--readonly] [--timeout 30s] [--set-default]` | 新增连接。非 TTY 缺 `--url` 报 `MISSING_ARGUMENT`；TTY 缺参走 huh 表单补全（url/org/username/password/readonly）。`--password` 为明文凭据参数，使用时 stderr 警告。无默认连接时自动设为默认 |
| `o2 conn ls` | 列出连接（name / url / org / username / readonly / default 标记） |
| `o2 conn show <name>` | 连接详情；password 不回显 |
| `o2 conn rm <name> [--yes]` | 删除连接；非 TTY 必须 `--yes`，TTY 弹确认 |
| `o2 conn default <name>` | 设为默认连接 |
| `o2 conn test <name>` | `GET /api/{org}/streams` 验证认证，返回 `{ok, latency_ms, version}`。version 尽力探测：`GET /version`（旧版服务端）→ `GET /api/_meta/node/list`（新版服务端，首个节点的 version）；均不可用（或无 `_meta` 权限）时留空并附 note，不影响测试结论 |

### stream 组

以下命令均支持 `-c/--conn` 与全局 `--timeout` / `--limit`。

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `o2 stream ls` | `--type logs\|metrics\|traces`（默认 logs）、`--fetch-schema` | `GET /api/{org}/streams` | `--json` 为响应原样 `{list: [...]}`；文本列 `name, stream_type, storage_type, doc_num, storage_size, compressed_size` |
| `o2 stream schema <name>` | `--type` | `GET /api/{org}/streams/{name}/schema` | `--json` 为响应原样单对象；文本列 `name, type`（schema 字段数组） |

flag 命名与取值对齐官方 API 的 query 参数（`type` / `fetchSchema`）。

### query 组

`query` 是数据查询命令组（别名 `search`：`o2 search ...` 与 `o2 query ...` 完全等效，含子命令）。

#### query（SQL）

```
o2 query [<sql> | --sql-file <path|->] [--start-time T] [--end-time T]
         [--from N] [--size N] [--last 枚举]
```

`POST /api/{org}/_search`，请求体 `{query:{sql, start_time, end_time, from, size}, timeout}`：

- SQL 来源二选一：位置参数 或 `--sql-file`（`-` 读 stdin；stdin 是 TTY 时报 `MISSING_ARGUMENT`），互斥且必选其一。
- `--start-time` / `--end-time` 对应 API 的 `start_time` / `end_time`（微秒），接受四种写法：Unix 微秒、RFC3339、负相对量（`-1h`、`-90m`，相对当前时刻）、`now`。
- `--last` 为常用时间窗枚举快捷方式：`5m|15m|30m|1h|3h|6h|12h|24h|2d|7d`（对齐 O2 UI 相对时间档），与 `--start-time/--end-time` 互斥；缺省等效 `--last 1h`。
- `--from`（默认 0）、`--size`（默认 100）与 API 字段同名同义。
- 全局 `--timeout` 同时作为 HTTP 客户端超时与 API 的 `timeout` 字段（秒）。
- `_search` 实际还有 query 参数 `type` / `is_ui_histogram` / `is_multi_stream_search` / `validate`（HTML 文档漏写，以实例 Swagger `/api-doc/openapi.json` 为准），本期未使用。

**渲染**：

- `--json`：envelope data 为 `_search` 响应原样（`{took, hits, total, from, size, scan_size}`），不加工。
- 文本模式（table/plain/tsv）：从 hits 构建动态列宽表——`_timestamp` 固定首列并渲染为本地可读时间（毫秒精度），其余列按在 hits 中首次出现的顺序追加（JSON 文档序）；嵌套 object/array 序列化为紧凑 JSON 字符串；null 显示为空。行受全局 `--limit` 截断（`meta.truncated`）。
- 查询统计（took/total/scan_size）不进文本表格，保留在 JSON 原样响应中。

**错误透传**：服务端结构化错误体 `{code, message, hint?, suggestions?}`（如 20004 字段不存在、20005 函数未定义）映射为 `QUERY_ERROR`，`hint`/`suggestions` 拼入 envelope 的 `error.hint`，便于人与 AI 调用方自纠错。

#### query around

```
o2 query around <stream> --key <µs|RFC3339> [--size 10] [--type logs|metrics|traces]
```

`GET /api/{org}/{stream}/_around?key=&size=`：以 `--key` 为锚点取上下文日志。服务端固定 **±15 分钟**窗口，`size` 为总预算、新旧两侧各取 `size/2`（整除）。**quirk**：`size=1` 时 `size/2=0` 被服务端视为不限量（返回 ±15m 内全部记录），故客户端强制 `size >= 2`（违反报 `CONFIG_INVALID`）。`--key` 接受微秒/RFC3339/负相对量/`now`（锚点不要求命中真实记录）。渲染与错误透传同 query（SQL）。

#### query values

```
o2 query values <stream> --fields f1[,f2...] [--size 10] [--from 0] [--keyword k]
                [--no-count] [--type logs|metrics|traces] [时间窗 flags]
```

`GET /api/{org}/{stream}/_values`：枚举字段在指定时间窗内的取值（日志排查高频操作，如"level 都有哪些值"）。`--fields` 必填，逗号分隔（同官方）；时间窗 flags 与 query 相同（缺省 1h）；`--keyword` 过滤取值、`--no-count` 省略出现次数、`--from` 翻页。响应 `{hits:[{field, values:[{zo_sql_key, zo_sql_num}]}]}`：`--json` 原样；文本合并为一张表 `field, value, count`（`zo_sql_key`→value、`zo_sql_num`→count，no-count 时 count 为空），适合管道处理。

### ingest 组

```
o2 ingest logs <stream> [--file <path|->] [--format json|multi]
```

摄入日志记录：`--format json` 走 `POST /api/{org}/{stream}/_json`（JSON 数组），`--format multi` 走 `_multi`（NDJSON，每行一个对象）。

- **数据源优先级**：`--file <path>` / `--file -`（stdin）> 无 `--file` 时 stdin 非 TTY 自动读（管道/重定向直接可用）> stdin 是 TTY 时报 `MISSING_ARGUMENT`。
- **客户端预检**：json 必须是合法 JSON 数组；multi 逐非空行必须是合法 JSON（失败报行号）。明显坏数据本地报 `CONFIG_INVALID`，不打到服务端。
- **readonly 连接拒绝**（`READONLY_VIOLATION`）。
- 响应 `{code, status:[{name, successful, failed, error}]}`：`--json` 原样；文本列 `stream, successful, failed, error`。**failed 合计 > 0 时退出码仍为 0**（完成的交换即报告），文本模式额外向 stderr 打一行警告。

**演进预留**：`ingest` 是组命令，`logs` 为显式子命令；metrics/traces 槽位留给后续迭代——metrics 可走已存在的 `POST /api/{org}/ingest/metrics/_json`（JSON 数组 `[{__name__, __type__, 标签..., _timestamp, value}]`），traces 走 OTLP `POST /api/{org}/v1/traces`。本期需要时可 `o2 request` 透传。

### request

```
o2 request <method> <path> [--file <path|->]
```

原生请求透传（curl 语义）：

- method 枚举 `GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS`（大小写不敏感）；path 必须以 `/` 开头，原样拼接实例 baseURL（用户自写 `/api/{org}/...` 全路径）。
- `--file` 提供请求体（`-` 读 stdin）；有 body 时默认 `Content-Type: application/json`。
- **完成的 HTTP 交换不论状态码都原样报告**：data 为 `{status, headers, body}`（body 按响应 Content-Type 解析为 JSON 值，否则字符串）；文本模式输出 `HTTP <status>` + 美化 body。只有传输层失败（连接失败/超时）才产生错误。
- **readonly 连接仅允许 GET/HEAD**，其余方法报 `READONLY_VIOLATION`（退出码 5）。`request` 可触达写/删除端点，readonly 是唯一拦截；显式调用即意图，不设 allowDangerous。
- 不知道有哪些端点可调时，先用 `o2 apidoc ls` / `o2 apidoc show` 探索实例的 OpenAPI spec（见下节）。

### apidoc 组

从实例的 OpenAPI spec（`GET /api-doc/openapi.json`，位于服务端根路径而非 `/api/{org}` 下）做结构化探索——"发现端点 → 看参数 → 用 `request` 调"的工作流入口：

| 命令 | 说明 |
|---|---|
| `o2 apidoc ls [--keyword k]` | 列出 spec 全部端点，表格 `method, path, summary`；`--keyword` 对 path/summary 做大小写不敏感过滤；`--json` 输出结构化条目数组 |
| `o2 apidoc show <path>` | 输出单个端点的 spec 片段（方法、参数、schema，JSON 原样）。path 支持三种写法：spec 原样（`/api/{org_id}/_search`）、参数名变体（`/api/{org}/_search`）、具体值（`/api/default/_search`）——按段匹配，spec 中的 `{param}` 段匹配任意具体段；多义时报错并列出候选。文本模式末尾附可直接执行的 `request` 调用示例（org 替换为连接的真实值，写方法附 `--file` 提示；`--json` 只含片段） |

服务端无 spec 端点（旧版）时报 `QUERY_ERROR` 并附官方文档链接 hint。

## 错误映射

| 场景 | 错误码 | 退出码 |
|---|---|---|
| HTTP 401 / 403 | `AUTH_FAILED` | 4 |
| 连接拒绝、无此主机 | `CONNECT_FAILED` | 3 |
| 超时（客户端或 context deadline） | `TIMEOUT` | 3 |
| 其余非 2xx（含 _search 的 20001~20013） | `QUERY_ERROR` | 5 |
| readonly 连接的写操作（request 非 GET/HEAD、ingest） | `READONLY_VIOLATION` | 5 |
| request 的非 2xx 完成交换 | 不报错（data 原样报告 status） | 0 |

## _meta 组织

OpenObserve 内置系统组织 `_meta`，身兼两职：自监控数据存储（服务端自身的 logs/metrics/traces 流），以及一组集群管理端点的强制宿主。以下端点在 handler 层强制 `org_id == _meta`（OSS 源码 `META_ORG_ID` 检查），对其他 org 调用返回 403 "only available for the _meta organization"：

| 端点 | 用途 |
|---|---|
| `GET /api/_meta/cluster/info` | 集群各 region/节点的待压缩任务数。官方文档写作 `/api/{org}/cluster_info`，org 与路径两处均与实际不符 |
| `GET /api/_meta/node/list` | 节点列表（含 `version`、角色、资源指标）；`conn test` 的版本 fallback 数据源 |
| `GET/PUT /api/_meta/announcements/config` | 公告横幅配置；横幅面向所有组织渲染，故发布权收归 _meta，且另需 _meta 管理员角色 |
| `POST /api/_meta/organizations/assume_service_account` | 扮演服务账号（企业版） |
| `GET/PUT /api/_meta/domain_management` | 域名管理（企业版） |
| `GET/PUT /api/_meta/settings/password_policy` | 密码策略 |

易混淆但**不限** `_meta`：`GET /api/{org}/announcements`（任何 org 可读当前生效横幅，数据物理存在 _meta 但读取开放）；`GET /api/{org}/password_complexity`（登录页需要，源码刻意保持各 org 可读）。

使用方式：

- 查自监控流：建一个 `_meta` 组织的连接（`o2 conn add meta --url ... --org _meta --username ...`），之后 `o2 -c meta stream ls` / `o2 -c meta query ...` 与常规流无异。
- 调集群端点：`o2 request GET /api/_meta/node/list`（request 的 path 自由书写，无需专门连接）。
- 注意 `_meta` 端点在 org 检查之上还叠加角色校验（如 announcements/config 需 _meta 管理员）；此时 403 映射为 `AUTH_FAILED`，语义实为"权限不足"而非"凭据无效"。

## 已知限制

- `_bulk`（ES 兼容批量）、metrics/traces 摄入、users/functions/metrics 管理 API 不在本期；需要时可用 `request` 透传。
- `query` 未暴露 `search_type` / `agent_options` 参数（分区模式、csv/md_table 输出）；muxcat 自身的渲染器覆盖表格需求，其余可用 `request` 透传。
- 文本模式动态列以 hits 首现顺序为准；超宽行（几十列）本期不做列裁剪，受全局 `--limit` 控制行数。
- TLS 由 url scheme 决定，不支持自定义 CA / 跳过证书校验（后续迭代按需加）。
