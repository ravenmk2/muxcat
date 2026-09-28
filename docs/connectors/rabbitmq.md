# rabbitmq connector

RabbitMQ connector 通过 RabbitMQ Management HTTP API（HTTP Basic Auth：username + password）接入，实现为纯 `net/http` 客户端，**不是 AMQP 驱动**（符合 `CGO_ENABLED=0` 基线）。命令名 `rabbitmq`，别名 `rmq`。覆盖：连接管理、broker overview 与 whoami、节点、队列、交换机、绑定、AMQP 客户端连接、channel、consumer、stream、vhost、policy、user、permission、definitions 导入导出、健康检查、feature flags / deprecated features、原生请求透传（request）。其余管理端点可用 `rmq request` 透传。

版本基线 **RabbitMQ 3.8 ~ 4.x**，三条纪律：

1. **宽容解析**：所有 API 响应按缺省字段解析（缺字段即零值，不用 strict decoding）。3.8 有镜像队列字段（`slave_pids` 等）、无 `type` 字段；4.x 相反；3.13 砍过一批 metrics 字段——两个方向都不炸。
2. **版本探测**：`conn test` 读 `GET /api/overview` 的 `rabbitmq_version`/`product_name` 并展示。
3. **门控能力不做客户端版本判断**：版本专属端点直接请求，404 时映射为 `UNSUPPORTED_OPERATION` + hint 说明所需最低版本（映射表见「版本兼容」）。

## 配置模型（rabbitmq.json）

```json
{
  "version": 1,
  "instances": {
    "local": { "url": "http://127.0.0.1:15672" }
  },
  "connections": {
    "local": {
      "instance": "local", "username": "admin",
      "password": "enc:v1:...", "readonly": false, "timeout": "30s"
    }
  },
  "defaultConnection": "local"
}
```

- `instances`：名字 → `{url}`，纯端点属性（Management API 地址，默认端口 15672）。url 必须含 scheme（`http`/`https`），**不得内嵌 user:password@**（防明文凭据落盘，违反报 `CONFIG_INVALID`）。
- `connections`：名字 → `{instance, username?, password?, readonly?, timeout?}`。`password` 以 `enc:v1:` 加密落盘，永不在输出中回显；`timeout` 为 Go duration 字符串，覆盖全局 `--timeout`。
- **多用户**：凭据是 connection 属性——同一 instance 可挂多个连接（不同权限用户）。
- `conn add <name>` 同名创建 instance 与 connection；`conn rm` 删除连接时，同名 instance 无其他引用则一并删除。
- vhost `/` 在 API 路径中编码为 `%2F`（`url.PathEscape` 已覆盖）；所有 vhost/name 进路径前统一转义。

## 命令

### conn 组

| 命令 | 说明 |
|---|---|
| `rmq conn add <name> --url <u> [--username u] [--password p] [--readonly] [--timeout 30s] [--set-default]` | 新增连接。非 TTY 缺 `--url` 报 `MISSING_ARGUMENT`；TTY 缺参走 huh 表单补全（url/username/password/readonly）。`--password` 为明文凭据参数，使用时 stderr 警告。无默认连接时自动设为默认 |
| `rmq conn ls` | 列出连接（name / url / username / readonly / default 标记） |
| `rmq conn show <name>` | 连接详情；password 不回显（Value map 无 password 字段） |
| `rmq conn rm <name> [--yes]` | 删除连接；非 TTY 必须 `--yes`，TTY 弹确认 |
| `rmq conn default <name>` | 设为默认连接 |
| `rmq conn test <name>` | `GET /api/whoami` 验证认证 + `GET /api/overview` 版本探测，返回 `{ok, user, latency_ms, version, product}`；版本探测失败时附 `note: "version unavailable"`（不影响 ok） |

### overview / node

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `rmq overview` | — | `GET /api/overview` | `--json` 原样；文本为 metric/value 摘要表（product/version/cluster/erlang + object_totals + queue_totals） |
| `rmq node ls` | — | `GET /api/nodes` | `--json` 原样；文本列 `name, running, type, mem_used, disk_free, fd_used, sockets_used, uptime` |
| `rmq node show <name>` | — | `GET /api/nodes/<name>` | 全量 JSON |

### queue 组

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `rmq queue ls` | `--vhost V`（限定 vhost） | `GET /api/queues[/<v>]` | `--json` 原样数组；文本列 `name, vhost, type, state, messages, ready, unacked, consumers, memory` |
| `rmq queue show <name>` | `--vhost` | `GET /api/queues/<v>/<name>` | 全量 JSON |
| `rmq queue declare <name>` | `--vhost --durable=true --auto-delete --type classic\|quorum\|stream --args '<json>'` | `PUT /api/queues/<v>/<name>`，body `{durable, auto_delete, arguments}`（`--type` 映射 `x-queue-type`，`--args` 合并进 arguments） | `{queue, vhost, declared: true}` + Message |
| `rmq queue delete <name>`（别名 `del`/`rm`） | `--vhost --if-empty --if-unused` | `DELETE /api/queues/<v>/<name>`（两个 if-* 为 query 参数） | `{queue, vhost, deleted: true}` |
| `rmq queue purge <name>` | `--vhost` | `DELETE /api/queues/<v>/<name>/contents` | `{queue, vhost, purged: true}` |
| `rmq queue get <name>` | `--vhost --limit 1..50（默认 1） --ackmode（默认 ack_requeue_true） --file <path>` | `POST /api/queues/<v>/<name>/get`，body `{count, ackmode, encoding:"auto", truncate:50000}` | 默认：`--json` 原样消息数组（payload 保持服务端编码）；文本列 `exchange, routing_key, redelivered, message_count, payload`，base64 payload 解码后展示、单条截断 1KiB 并标注。`--file`：payload 落盘（单条写 `<path>`，多条写 `<path>.0/.1...`，base64 先解码成原始字节，0600），`--json` 改报 `{files, count}` |

- type 列跨版本解析：4.x 读 `type` 字段 → 否则 `arguments.x-queue-type` → 否则 `classic`（3.8 默认）。
- `queue get` 是**调试设施**，不走高吞吐场景。`--ackmode` 四值分两组：peek 语义 `ack_requeue_true`（默认）/`reject_requeue_true`（消息回队，非破坏，readonly 连接可用）；破坏语义 `ack_requeue_false`/`reject_requeue_false`（消息出队/丢弃，help 中有警告，readonly 连接按 `READONLY_VIOLATION` 拦截）。

### exchange 组

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `rmq exchange ls` | `--vhost` | `GET /api/exchanges[/<v>]` | 文本列 `name, vhost, type, durable, auto_delete, internal` |
| `rmq exchange show <name>` | `--vhost` | `GET /api/exchanges/<v>/<name>` | 全量 JSON |
| `rmq exchange declare <name>` | `--vhost --type direct\|fanout\|topic\|headers --durable=true --auto-delete --args` | `PUT /api/exchanges/<v>/<name>` | `{exchange, vhost, declared: true}` |
| `rmq exchange delete <name>`（别名 `del`/`rm`） | `--vhost --if-unused` | `DELETE /api/exchanges/<v>/<name>` | `{exchange, vhost, deleted: true}` |
| `rmq exchange publish <name>` | `--vhost --routing-key --payload / --payload-file（互斥、必有其一，文件上限 1MB） --payload-encoding string\|base64 --count 1..10000 --props '<json>'` | `POST /api/exchanges/<v>/<name>/publish`（`--count` 循环发 N 次；首发即失败 sent=0 直接报错，已发过至少一条才停止并走 RenderPartial 报已发数量） | `{exchange, routing_key, sent, routed}`（routed = 服务端 routed=true 的次数）；routed=0 时 Message 提示无队列绑定 |

- `exchange publish` 是**调试设施**：同步、无 confirm，不适合大消息或高频率。`--payload-file` 内容在 `--payload-encoding base64` 时做 base64，string 时原样。

### binding 组

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `rmq binding ls` | `--vhost --exchange --queue` 组合过滤 | `GET /api/bindings`、`/api/bindings/<v>`、`/api/bindings/<v>/e/<e>`、`/api/bindings/<v>/q/<q>`、`/api/bindings/<v>/e/<e>/q/<q>` | 文本列 `vhost, source, destination, type, routing_key, props`（props = properties_key） |
| `rmq binding bind` | `--vhost --exchange --queue --routing-key --args`（`--exchange`/`--queue` 必填） | `POST /api/bindings/<v>/e/<e>/q/<q>` | `{vhost, exchange, queue, routing_key, bound: true}` |
| `rmq binding unbind` | `--vhost --exchange --queue --props`（后三个必填） | `DELETE /api/bindings/<v>/e/<e>/q/<q>/<props>` | `{vhost, exchange, queue, unbound: true}` |

- `unbind` 需要 binding 的 properties key：取自 `binding ls` 的 props 列（空 routing key 显示为 `~`）。

### connection / channel / consumer 组（AMQP 侧，只读为主）

注意与 `conn` 组区分：`conn` 管理 muxcat 存储的连接配置，`connection` 是 broker 上的 AMQP 客户端连接。

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `rmq connection ls` | — | `GET /api/connections` | 文本列 `name, user, vhost, state, channels, peer, protocol` |
| `rmq connection show <name>` | — | `GET /api/connections/<name>` | 全量 JSON |
| `rmq connection close <name>` | — | `DELETE /api/connections/<name>`（写操作，受 readonly 保护） | `{connection, closed: true}` |
| `rmq channel ls` / `channel show <name>` | — | `GET /api/channels[/<name>]` | 文本列 `name, vhost, user, state, mode(tx/confirm), unacked, prefetch, consumers` |
| `rmq consumer ls` | `--vhost` | `GET /api/consumers[/<v>]` | 文本列 `queue, vhost, channel, consumer_tag, ack_required, prefetch, active` |

- connection/channel 的 `<name>` 含空格和 ` -> `，进路径前整体转义。

### stream 组（只读）

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `rmq stream ls` | `--vhost` | `GET /api/queues`（客户端过滤 type=stream） | 文本列 `name, vhost, state, messages, consumers, memory, super` |
| `rmq stream show <name>` | `--vhost` | `GET /api/queues/<v>/<name>` | queue 视角全量 JSON（stream 特有字段 segments/offset/members 在其中） |
| `rmq stream connection ls` | `--vhost` | `GET /api/stream/connections[/<v>]` | 文本列 `name, vhost, user, state, node` |
| `rmq stream publisher ls` | `--vhost` | `GET /api/stream/publishers[/<v>]` | 文本列 `stream, reference, id, connection, published, confirmed, errored` |
| `rmq stream consumer ls` | `--vhost` | `GET /api/stream/consumers[/<v>]` | 文本列 `stream, subscription_id, connection, credits` |

- SUPER 列是**启发式**：分区 stream 命名约定 `<super-stream>-<数字>`，按此后缀归属所属超流（普通 stream 恰好叫 `foo-1` 会误判，见「已知限制」）。

### vhost 组

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `rmq vhost ls` | — | `GET /api/vhosts` | 文本列 `name, tracing, messages, ready, unacked, description, tags` |
| `rmq vhost show <name>` | — | `GET /api/vhosts/<name>` | 全量 JSON |
| `rmq vhost add <name>` | `--description --tags a,b`（3.13+ 字段，仅设置时才发送） | `PUT /api/vhosts/<name>` | `{vhost, created: true}` |
| `rmq vhost delete <name>`（别名 `del`/`rm`） | — | `DELETE /api/vhosts/<name>` | `{vhost, deleted: true}` |

### whoami

| 命令 | 端点 | data 形状 |
|---|---|---|
| `rmq whoami` | `GET /api/whoami` | Value `{name, tags}`（`--json` 为服务端原样） |

### policy 组

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `rmq policy ls` | `--vhost --operator` | `GET /api/policies[/<v>]`（`--operator` 切 `/api/operator-policies`） | 文本列 `name, vhost, pattern, apply_to, priority, definition`（definition 紧凑 JSON） |
| `rmq policy show <name>` | `--vhost --operator` | `GET /api/policies/<v>/<name>`（`--operator` 切 `/api/operator-policies/<v>/<name>`） | 全量 JSON |
| `rmq policy set <name>` | `--vhost --pattern --definition '<json>'（均必填） --priority 0 --apply-to all\|queues\|exchanges\|classic_queues\|quorum_queues\|streams --operator` | `PUT /api/policies/<v>/<name>`（`--operator` 切 `/api/operator-policies/<v>/<name>`），body `{pattern, definition, priority, apply-to}` | `{policy, vhost, set: true}` |
| `rmq policy delete <name>`（别名 `del`/`rm`） | `--vhost --operator` | `DELETE /api/policies/<v>/<name>`（`--operator` 切 `/api/operator-policies/<v>/<name>`） | `{policy, vhost, deleted: true}` |

### user 组

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `rmq user ls` | — | `GET /api/users` | 文本列 `name, tags, is_internal`（tags 字符串/数组两种形状均容忍） |
| `rmq user show <name>` | — | `GET /api/users/<name>` | 全量 JSON |
| `rmq user add <name>` | `--password --tags a,b` | `PUT /api/users/<name>`，body `{password, tags}`（tags 逗号串；空密码省略字段）。**create-only**：先 GET 探测，用户已存在时报 `CONFIG_INVALID`，不会 upsert/清密码 | `{user, tags, created: true}` + Message（无密码时提示用户暂不能登录） |
| `rmq user passwd <name>` | `--password` | 先 `GET /api/users/<name>` 取现有 tags 随 PUT 回传（改密码不洗角色），再 `PUT /api/users/<name>` 覆盖密码；空密码报 `CONFIG_INVALID`（否则服务端走 clear_password 静默清密码） | `{user, password_changed: true}` |
| `rmq user delete <name>`（别名 `del`/`rm`） | — | `DELETE /api/users/<name>` | `{user, deleted: true}` |

- 密码处理与 conn add 同规：`--password` 明文 flag 打 stderr 警告；TTY 下无 flag 走 huh 密码框；任何输出不回显密码。`user passwd` 非交互缺 `--password` 报 `MISSING_ARGUMENT`。

### permission 组

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `rmq permission ls` | `--vhost --user`（客户端过滤，仅作用于文本表格；`--json` 恒为未过滤原数组） | `GET /api/permissions` | 文本列 `user, vhost, configure, write, read` |
| `rmq permission set <user>` | `--vhost --configure --write --read`（三段正则，默认空串） | `PUT /api/permissions/<v>/<user>` | `{user, vhost, granted: true}` |
| `rmq permission delete <user>`（别名 `del`/`rm`） | `--vhost` | `DELETE /api/permissions/<v>/<user>` | `{user, vhost, revoked: true}` |

### definitions 组

环境间拓扑迁移 / 种子数据：整 broker（或单 vhost）的用户、vhost、权限、队列、交换机、绑定、策略打成一个 JSON 文档导出/导入。

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `rmq definitions export` | `--vhost --file` | `GET /api/definitions[/<v>]` | 默认 stdout：`--json` 原样文档，文本模式 pretty JSON；`--file` 写文件（0600），Value `{file, bytes}` + Message |
| `rmq definitions import` | `--vhost --file <path\|->（缺省/- 读 stdin）` | `POST /api/definitions[/<v>]` | `{imported: true, bytes}` + Message |

- import 先本地 `json.Valid` 校验（不合法报 `CONFIG_INVALID`，不发请求）；导入是合并语义，不删除文件中不存在的对象；写操作，readonly 拦截。

### featureflags / deprecatedfeatures（只读）

| 命令 | 端点 | data 形状 |
|---|---|---|
| `rmq featureflags ls` | `GET /api/feature-flags` | 文本列 `name, state, stability, provided_by, description` |
| `rmq deprecatedfeatures ls` | `GET /api/deprecated-features`（3.13+，404 走门控映射） | 文本列 `name, state, desc`（state 缺省回退 deprecation_phase） |
| `rmq deprecatedfeatures used` | `GET /api/deprecated-features/used` | 同上，仅列出实际使用中的 |

- deprecated-features 响应裸数组与对象包装（`deprecated_features`/`used`）两种形状均容忍。

### health

```
rmq health [check] [--aliveness --vhost V]
```

- 默认 check 为 `alarms`；合法值：`alarms, local-alarms, port-listener, protocol-listener, virtual-hosts, node-is-quorum-critical, certificate-expiration, is-in-service, ready-to-serve-clients`（后两个 4.x 专属，旧服务器 404 → `UNSUPPORTED_OPERATION`）。端点 `GET /api/health/checks/<check>`。
- `--aliveness` 走 `GET /api/aliveness-test/<vhost>`（默认 vhost `/`）。
- 健康时输出 `status: ok`；检查失败服务端应答 503 + `{"status":"failed","reason":...}`，映射为 `QUERY_ERROR`（reason 进消息/hint）。

### request

```
rmq request <method> <path> [--file <path|->] [--content-type <mime>]
```

原生请求透传（curl 语义）：method 枚举 `GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS`；path 以 `/` 开头原样拼接 baseURL。**完成的 HTTP 交换不论状态码都原样报告**（data 为 `{status, headers, body}`）；只有传输层失败才产生错误。readonly 连接仅允许 GET/HEAD。

## 版本兼容（3.8 ~ 4.x 门控表）

| 能力 | 最低版本 | 说明 |
|---|---|---|
| 基础端点（overview/nodes/queues/exchanges/bindings/connections/channels/consumers/vhosts/health alarms 系） | 3.8 | 宽容解析跨版本字段差异（3.8 镜像字段、3.13 metrics 裁剪、4.x type 字段） |
| `/api/stream/*`（stream connection/publisher/consumer） | 3.9 | 404 → `UNSUPPORTED_OPERATION`，hint 含 3.9 |
| super stream（SUPER 列归属） | 3.11 | 仅命名启发式，无额外端点 |
| vhost description/tags | 3.13 | `vhost add` 仅在 flag 设置时发送这两个字段 |
| `/api/deprecated-features`（deprecatedfeatures ls/used） | 3.13 | 404 → `UNSUPPORTED_OPERATION`，hint 含 3.13 |
| `health is-in-service` / `ready-to-serve-clients` | 4.0 | 404 → `UNSUPPORTED_OPERATION`，hint 含 4.0 |
| 分页参数（page/page_size） | 3.12 | **不使用**，见「已知限制」 |

门控实现：client 内置 endpoint 前缀 → 最低版本映射表；`classifyStatus` 对 404 命中映射时返回 `UNSUPPORTED_OPERATION`，hint 说明所需最低版本。不做 `conn test` 版本号解析判断。

## 错误映射

RabbitMQ 错误体为 `{"error":"...","reason":"..."}`，classifyStatus 解析后 reason 进消息/hint。

| 场景 | 错误码 | 退出码 |
|---|---|---|
| HTTP 401 / 403 | `AUTH_FAILED`（hint 提醒：默认 guest/guest 账号仅允许 localhost 登录） | 4 |
| HTTP 404（命中版本门控映射） | `UNSUPPORTED_OPERATION`（hint 含所需最低版本） | 5 |
| HTTP 404（其余） | `QUERY_ERROR`（消息含 "not found"，hint 提示检查 vhost/对象名及 %2F 编码） | 5 |
| 健康检查失败（503 + status:failed） | `QUERY_ERROR`（reason 进消息/hint） | 5 |
| 连接拒绝、无此主机 | `CONNECT_FAILED` | 3 |
| 超时（客户端或 context deadline） | `TIMEOUT` | 3 |
| 其余非 2xx（如 400 bad_request） | `QUERY_ERROR`（reason 进消息与 hint） | 5 |
| readonly 连接的写操作（declare/delete/purge/publish/bind/unbind/close/vhost add/delete、queue get 破坏性 ackmode、policy set/delete、user add/passwd/delete、permission set/delete、definitions import、request 非 GET/HEAD） | `READONLY_VIOLATION` | 5 |
| request 的非 2xx 完成交换 | 不报错（data 原样报告 status） | 0 |

## 已知限制

- **无分页参数**：Management API 分页（page/page_size）3.12+ 才有，且响应形状从数组变对象。所有 list 命令直接请求 plain 端点按数组解析，行数由全局 `--limit` 在客户端截断（meta.truncated 标注）。超大集群（数万队列）会有大响应体，可配合 `rmq request` 自行传分页参数。
- **单次响应体上限 64MB**。
- **stream 门控**依赖 404 映射，不做预检；3.8 服务器上 `stream ls`（复用 /api/queues）仍可用但恒为空列表，`stream connection/publisher/consumer` 报 `UNSUPPORTED_OPERATION`。
- **SUPER 列为命名启发式**：按 `<super-stream>-<数字>` 后缀归属，普通 stream 若恰好以此模式命名会误归属。
- **`exchange publish` / `queue get` 仅调试用**：同步、无 confirm、无高吞吐；payload 服务端截断 50000 字节，文本展示再截 1KiB（`--json` 保留服务端原值）；publish `--payload-file` 上限 1MB。
- 写操作守卫只有 readonly 连接（`READONLY_VIOLATION`），不设 `--yes` 确认（显式命令即意图）。
- TLS 由 url scheme 决定，不支持自定义 CA / 跳过证书校验（后续迭代按需加）。
- federation/shovel/limits 等其余管理面未封装，用 `rmq request` 透传可达。
