# openobserve connector

OpenObserve connector 通过 OpenObserve 的 REST API（HTTP Basic Auth）接入，实现为纯 `net/http` 客户端，无外部驱动依赖（符合 `CGO_ENABLED=0` 基线）。命令名 `openobserve`，别名 `o2`。第一版覆盖连接管理、stream 查看、SQL 搜索与原生请求透传；摄入（ingest）、`_values`、users/functions/metrics 等管理类 API 不在本期范围。

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

### search

```
o2 search [<sql> | --sql-file <path|->] [--start-time T] [--end-time T]
          [--from N] [--size N] [--last 枚举]
```

`POST /api/{org}/_search`，请求体 `{query:{sql, start_time, end_time, from, size}, timeout}`：

- SQL 来源二选一：位置参数 或 `--sql-file`（`-` 读 stdin；stdin 是 TTY 时报 `MISSING_ARGUMENT`），互斥且必选其一。
- `--start-time` / `--end-time` 对应 API 的 `start_time` / `end_time`（微秒），接受四种写法：Unix 微秒、RFC3339、负相对量（`-1h`、`-90m`，相对当前时刻）、`now`。
- `--last` 为常用时间窗枚举快捷方式：`5m|15m|30m|1h|3h|6h|12h|24h|2d|7d`（对齐 O2 UI 相对时间档），与 `--start-time/--end-time` 互斥；缺省等效 `--last 1h`。
- `--from`（默认 0）、`--size`（默认 100）与 API 字段同名同义。
- 全局 `--timeout` 同时作为 HTTP 客户端超时与 API 的 `timeout` 字段（秒）。

**渲染**：

- `--json`：envelope data 为 `_search` 响应原样（`{took, hits, total, from, size, scan_size}`），不加工。
- 文本模式（table/plain/tsv）：从 hits 构建动态列宽表——`_timestamp` 固定首列并渲染为本地可读时间（毫秒精度），其余列按在 hits 中首次出现的顺序追加（JSON 文档序）；嵌套 object/array 序列化为紧凑 JSON 字符串；null 显示为空。行受全局 `--limit` 截断（`meta.truncated`）。
- 查询统计（took/total/scan_size）不进文本表格，保留在 JSON 原样响应中。

**错误透传**：服务端结构化错误体 `{code, message, hint?, suggestions?}`（如 20004 字段不存在、20005 函数未定义）映射为 `QUERY_ERROR`，`hint`/`suggestions` 拼入 envelope 的 `error.hint`，便于人与 AI 调用方自纠错。

### request

```
o2 request <method> <path> [--file <path|->]
```

原生请求透传（curl 语义）：

- method 枚举 `GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS`（大小写不敏感）；path 必须以 `/` 开头，原样拼接实例 baseURL（用户自写 `/api/{org}/...` 全路径）。
- `--file` 提供请求体（`-` 读 stdin）；有 body 时默认 `Content-Type: application/json`。
- **完成的 HTTP 交换不论状态码都原样报告**：data 为 `{status, headers, body}`（body 按响应 Content-Type 解析为 JSON 值，否则字符串）；文本模式输出 `HTTP <status>` + 美化 body。只有传输层失败（连接失败/超时）才产生错误。
- **readonly 连接仅允许 GET/HEAD**，其余方法报 `READONLY_VIOLATION`（退出码 5）。`request` 可触达写/删除端点，readonly 是唯一拦截；显式调用即意图，不设 allowDangerous。

## 错误映射

| 场景 | 错误码 | 退出码 |
|---|---|---|
| HTTP 401 / 403 | `AUTH_FAILED` | 4 |
| 连接拒绝、无此主机 | `CONNECT_FAILED` | 3 |
| 超时（客户端或 context deadline） | `TIMEOUT` | 3 |
| 其余非 2xx（含 search 的 20001~20013） | `QUERY_ERROR` | 5 |
| readonly 连接的写方法（request） | `READONLY_VIOLATION` | 5 |
| request 的非 2xx 完成交换 | 不报错（data 原样报告 status） | 0 |

## 已知限制

- 摄入（`_json`/`_multi`/`_bulk`）、`stream values`（`_values`）、users/functions/metrics 管理 API 不在第一版；需要时可用 `request` 透传。
- `search` 未暴露 `search_type` / `agent_options` 参数（分区模式、csv/md_table 输出）；muxcat 自身的渲染器覆盖表格需求，其余可用 `request` 透传。
- 文本模式动态列以 hits 首现顺序为准；超宽行（几十列）第一版不做列裁剪，受全局 `--limit` 控制行数。
- TLS 由 url scheme 决定，不支持自定义 CA / 跳过证书校验（后续迭代按需加）。
