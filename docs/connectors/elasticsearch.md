# elasticsearch connector

Elasticsearch connector 通过 Elasticsearch REST API 接入，实现为纯 `net/http` 客户端，无外部驱动依赖（符合 `CGO_ENABLED=0` 基线）。命令名 `elasticsearch`，别名 `es`。**支持 Elasticsearch 7.x/8.x/9.x**。覆盖：连接管理（conn 组）、搜索（search）、索引/文档/集群只读检查（index、doc、cluster 组）与原生请求透传（request）。写入/管理类操作（索引创建、文档写入等）不在本期范围——经 `request` 透传。

## 配置模型（elasticsearch.json）

```json
{
  "version": 1,
  "instances": {
    "prod": { "url": "https://es.example.com:9200" }
  },
  "connections": {
    "prod": {
      "instance": "prod",
      "username": "elastic",
      "password": "enc:v1:...",
      "insecureSkipVerify": true,
      "timeout": "30s"
    }
  },
  "defaultConnection": "prod"
}
```

- `instances`：名字 → `{url}`。url 是服务器根地址，必须含 scheme（`http`/`https`），**不得内嵌 user:password@**（违反报 `CONFIG_INVALID`）。
- `connections`：名字 → `{instance, username?, password?, apiKey?, readonly?, timeout?, insecureSkipVerify?}`。`password` 与 `apiKey` 以 `enc:v1:` 加密落盘，永不在任何输出通道回显；`timeout` 覆盖全局 `--timeout`。
- `conn add <name>` 同名创建 instance 与 connection；`conn rm` 删除连接时，同名 instance 无其他引用则一并删除。无默认连接时首个连接自动设为默认。

## 认证与 TLS

- **两种认证互斥**：basic auth（`username`+`password`）与 API key（`apiKey`）二选一；`conn add` 同时给出报 `CONFIG_INVALID`。
- **apiKey 语义**：存储用户给出的 base64(id:api_key) 字符串本身，请求时以 `Authorization: ApiKey <apiKey>` 原样发送，不做二次编码。
- **明文 flag**：`--password` / `--apikey` 为明文凭据入口，使用时打 stderr 警告，加密后才落盘；TTY 缺参走 huh 表单（密码/API key 不回显输入）。
- **insecureSkipVerify**：Elasticsearch 8/9 默认启用 TLS 且使用自签名 CA，直连自签名部署会因证书校验失败报 `CONNECT_FAILED`（hint 指向该选项）。连接开启 `insecureSkipVerify` 后构建 `tls.Config{InsecureSkipVerify: true}` 的 Transport 跳过校验；这是显式用户选择，仅对自签名场景使用。不支持自定义 CA（后续迭代按需加）。

## 版本兼容性

- **默认媒体类型即跨版本可用**：`request` 默认发送/接受普通 `application/json`，Elasticsearch 7.x/8.x/9.x 均接受，无需客户端版本适配层。
- **`--compat <7|8>`**：发送版本化媒体类型 `application/vnd.elasticsearch+json;compatible-with=N`（Accept 恒带；有 body 时 Content-Type 同步）。兼容头仅跨一个大版本有效：8.x 接受 `compatible-with=7|8`，9.x 仅接受 `=8`。客户端不校验 compat 值与服务端版本的匹配关系，不匹配时服务端返回 406，按 raw 语义原样呈现为 data。
- **版本探测**：`conn test` 通过 `GET /` 的 `version.number` 与 `cluster_name` 读取服务端版本与集群名。
- **X-Elastic-Product**：Elasticsearch 7.14+ 的所有响应携带 `X-Elastic-Product: Elasticsearch` 头；缺失（如 OpenSearch 分支或更老版本）时 `conn test` 不失败，降级为在结果中附 warning 字段。

## 命令

### conn 组

| 命令 | 说明 |
|---|---|
| `es conn add <name> --url <u> [--username u] [--password p \| --apikey k] [--insecure-skip-verify] [--readonly] [--timeout 30s] [--set-default]` | 新增连接（同名创建 instance）。非 TTY 缺 `--url` 报 `MISSING_ARGUMENT`；TTY 缺参走 huh 表单补全。`--password`/`--apikey` 为明文凭据参数，使用时 stderr 警告；两者互斥 |
| `es conn ls` | 列出连接（name / url / user / auth(basic\|apikey\|none) / tls / insecure / readonly / default 标记）；不回显任何凭据 |
| `es conn show <name>` | 连接详情；Value map 无 password/apiKey 字段，仅 `auth` 指示是否配置了认证 |
| `es conn rm <name> [--yes]` | 删除连接；非 TTY 必须 `--yes`，TTY 弹确认 |
| `es conn default <name>` | 设为默认连接 |
| `es conn test [name]` | `GET /` 探测：认证检查 + 读 `version.number`/`cluster_name`，返回 `{ok, latency_ms, version, cluster_name}`（缺 `X-Elastic-Product` 头时附 warning，不失败）；name 缺省用默认连接 |

### search

| 命令 | 参数 / flag | data 形状 |
|---|---|---|
| `es search <index>` | `-q/--query`（Lucene query_string）、`--file <path\|->`（完整 DSL body，`-` 读 stdin，stdin 为 TTY 时拒绝）、`--size`（默认 10）、`--from`（默认 0）、`--sort <field:asc\|desc>`（可重复，缺省 order 为 asc） | 始终 `POST /<index>/_search`。无 flag 时 match_all；`-q` 模式由客户端用 encoding/json 构造 DSL（不做字符串拼接）。`--query` 与 `--file` 互斥；`--file` 与 `--size/--from/--sort` 互斥（DSL body 自带分页排序）——均报 `CONFIG_INVALID`。文本模式渲染表格：`_id`、`_score` + 各 hit `_source` 顶层标量键的有序并集（上限 15 列；非标量值渲染为紧凑 JSON，缺失键为空格）。`hits.total` 兼容两种形态（7.x 裸数字与 8/9 `{"value","relation"}` 对象）；`relation=gte` 时消息为 `total >= N`。`--json` 为原始响应体；`meta.truncated` 在返回数 < total 时置位 |

### index / doc / cluster 组

| 命令 | 说明 |
|---|---|
| `es index ls` | `GET /_cat/indices?format=json&h=...&s=index`；表格 `name,health,status,docs,size,pri,rep`（_cat 数值字段为字符串，渲染前转 int；size 保留人类可读串） |
| `es index show <name>` | `GET /<index>`；文本 Value 为摘要（settings 的 shards/replicas、mappings 顶层字段列表与计数、aliases 列表）；`--json` 为原始 body |
| `es doc get <index> <id>` | `GET /<index>/_doc/<id>`；文本为 `_index/_id/_version` 头部行 + pretty `_source`；404 报 `QUERY_ERROR` + "document not found" hint（服务端 reason 透传） |
| `es cluster health` | `GET /_cluster/health`；Value 为稳定字段集（status、cluster_name、node/shard 计数、timed_out 等） |
| `es cluster nodes` | `GET /_cat/nodes?format=json&h=...&s=name`；表格 `name,ip,role,master,version,heap%,ram%,cpu,load_1m`（数值转换同上） |

以上全部为只读命令，readonly 连接均可执行（search 的 POST 是查询语义，不受 readonly 的 GET/HEAD 限制约束——该约束仅作用于 request 透传）。

### request

| 命令 | 参数 / flag | 说明 |
|---|---|---|
| `es request <method> <path>` | `--file <path\|->`（`-` 读 stdin，stdin 为 TTY 时拒绝）、`--compat <7\|8>` | curl 语义透传：path 必须以 `/` 开头、原样拼到实例根地址；method 枚举 GET\|POST\|PUT\|PATCH\|DELETE\|HEAD\|OPTIONS。任何完成的 HTTP 交换（含 4xx/5xx）都返回 `{status, headers, body}` 作为 data，只有传输层失败才是错误。readonly 连接仅允许 GET/HEAD |

## 错误映射

| 场景 | 错误码 | 退出码 |
|---|---|---|
| HTTP 401 / 403（认证失败、权限不足；消息取服务端 error.reason） | `AUTH_FAILED` | 4 |
| 其余非 2xx（conn test 路径；request 下非 2xx 是 data 不是错误） | `QUERY_ERROR`（body 截断 512 字符） | 5 |
| 连接拒绝、无此主机 | `CONNECT_FAILED` | 3 |
| TLS 证书校验失败（自签名） | `CONNECT_FAILED`（hint 指向 --insecure-skip-verify） | 3 |
| 超时（客户端或 context deadline） | `TIMEOUT` | 3 |
| `--url` 缺 scheme / 内嵌凭据、password 与 apikey 同给、`--compat` 非 7\|8、method/path 非法、search 的 `--query` 与 `--file` 或分页 flag 冲突 | `CONFIG_INVALID` | 2 |
| readonly 连接的非 GET/HEAD 请求 | `READONLY_VIOLATION` | 5 |
| 非 TTY 缺 `--url`、非 TTY `conn rm` 缺 `--yes`、`--file -` 但 stdin 是 TTY | `MISSING_ARGUMENT` | 2 |

## 已知限制

- 无客户端版本适配层：本期覆盖的 API（_search、_cat、_cluster/health、GET /<index>、_doc）在 ES 7.x/8.x/9.x 字节级一致；其余写入/管理类操作经 `request` 透传，请求体语义由用户按服务端版本把握。
- `search` 文本表格的 `_source` 列上限 15 列（取键名排序并集），宽文档用 `--json` 取原始响应；`--file` 模式的分页/排序由 DSL body 自带，`--size/--from/--sort` 不与其组合。
- `--compat` 不在客户端与服务端版本对账；不兼容时依赖服务端 406 按 raw 语义呈现。
- 单次响应体上限 64MB；错误消息中的 body 截断 512 字符。
- 大结果集需用户侧分页（`--size/--from`、search_after、scroll、PIT 等），`search` 仅做 size/from 分页，深度翻页用 `request` 自行组织。
- TLS 支持系统 CA 或 `--insecure-skip-verify`，不支持钉选自定义 CA 证书。
