# amqp connector

amqp connector 走 AMQP **wire 协议**（数据面），与 rabbitmq connector（Management HTTP API，管理面）互补：管理找 `rmq`，收发找 `amqp`。单一 connector、独立配置文件 `amqp.json`、**无别名**，**同时支持 AMQP 0.9.1 与 AMQP 1.0**：协议是 instance 属性，命令面统一，按 instance 的 `protocol` 字段分派到两套客户端实现。

**已验证**（2026-09-28，对 RabbitMQ 4.3.1 实测）：5672 单端口双协议（协议头协商，无需插件）；两条协议均完成 连接→声明队列→发布→消费→确认→purge→删除 全链路。

## 依赖与版本基线

| 协议 | 客户端库 | License | 基线 |
|---|---|---|---|
| 0.9.1 | `github.com/rabbitmq/amqp091-go` v1.15.0 | BSD-2 | RabbitMQ 3.8 ~ 4.x 及任意 0.9.1 broker |
| 1.0 | `github.com/rabbitmq/rabbitmq-amqp-go-client` v1.4.0（官方，wrap `Azure/go-amqp` v1.7.0） | Apache-2.0 / MIT | **RabbitMQ ≥ 4.0**（management 接口为 RabbitMQ 专有） |

- 均纯 Go，符合 `CGO_ENABLED=0` 基线。
- 1.0 客户端的 `AmqpManagement` 提供 DeclareQueue/DeclareExchange/DeleteQueue/DeleteExchange/Bind/Unbind/PurgeQueue/QueueInfo（含 typed spec：Classic/Quorum/Stream）——0.9.1 的协议能力 1.0 基本全覆盖，这是单 connector 双实现成立的前提。
- 1.0 地址格式用 **v2**（`/exchanges/:e/:rk`、`/queues/:q`）；v1 在 4.0 已 deprecated，不实现。

## 配置模型（amqp.json）

```json
{
  "version": 1,
  "instances": {
    "local": { "url": "amqp://127.0.0.1:5672", "protocol": "1.0" }
  },
  "connections": {
    "local": {
      "instance": "local", "username": "admin",
      "password": "enc:v1:...", "vhost": "/",
      "readonly": false, "timeout": "30s", "tlsSkipVerify": false
    }
  },
  "defaultConnection": "local"
}
```

- `instances`：`{url, protocol}`。**protocol 是 instance 属性**（端点方言）：同一 broker 需要双协议时建两个 instance（url 可重复，按名字索引）。`protocol` 严格枚举 `"0.9.1"` / `"1.0"`（默认 `"1.0"`），其他值报 `CONFIG_INVALID` 并列出合法值。url scheme 为 `amqp`/`amqps`，**不得内嵌 user:password@**（违反报 `CONFIG_INVALID`）。
- `connections`：`{instance, username?, password?, vhost?, readonly?, timeout?, tlsSkipVerify?}`。**vhost 是 connection 属性**（握手时选定的会话级租户，类比 mysql 的 database；权限按 (user, vhost) 授予，与凭据同级），缺省 `/`。
- `tlsSkipVerify`：仅对 `amqps://` 有意义；`amqp://` 下设置为 true 报 `CONFIG_INVALID`。每次以 skip-verify 建连时 stderr 打一行安全警告。后续自定义 CA 用 `tlsCaCert`（首版不实现）。
- 凭据、`enc:v1:`、readonly、timeout 语义同 rabbitmq connector。schema：`schema/amqp.schema.json`。

## 命令

**`--vhost` 全局约定**：所有数据面命令（queue/exchange/binding/publish/consume/get/show 各 leaf 及 `conn test`）接受 `--vhost` 覆盖连接默认（help 统一 "vhost (overrides connection default)"）。CLI 每条命令新建会话，覆盖零成本。优先级 `--vhost` > connection `vhost` > `/`。两条协议的 vhost 编码路径不同（0.9.1 走 URI path、`/` → `%2F`；1.0 映射 open frame `vhost:` 前缀），拼 dial URI 的逻辑收敛为一个函数。

### conn 组

与 rabbitmq connector 同构：`conn add/ls/show/rm/default/test`。

- `conn add <name> --url <u> [--protocol 0.9.1|1.0] [--username u] [--password p] [--vhost /] [--readonly] [--timeout 30s] [--tls-skip-verify] [--set-default]`。`--url`/`--protocol` 建 instance，其余建 connection；`--vhost` 在此是**写入配置默认值**。
- `conn ls` 文本列 `name, url, protocol, username, vhost, readonly, default`。
- `conn test <name>`：真实握手 + server properties 探测（两条协议都能拿到 `product`/`version`），data `{ok, latency_ms, product, version, protocol, vhost}` + Message。

### queue 组

| 命令 | 说明 |
|---|---|
| `amqp queue declare <name>` | `--type classic\|quorum\|stream`（1.0 用 typed spec；0.9.1 映射 `x-queue-type`）`--durable=true --auto-delete --exclusive --args '<json>'`。data `{queue, declared: true}`。**`--durable` 仅作用于 0.9.1**：1.0 的 typed spec 无 durable 字段（classic 由 broker 声明为 durable），help 已注明 |
| `amqp queue delete <name>`（别名 `del`/`rm`） | `--if-empty --if-unused`。0.9.1 原生支持；1.0 `DeleteQueue` 无此选项 → 客户端先 `QueueInfo` 自检再删（CLI 语义足够，接受竞态） |
| `amqp queue purge <name>` | 两协议均返回 purge 条数，data `{queue, purged: N}` |
| `amqp queue show <name>` | 0.9.1：passive declare → `{messages, consumers}`；1.0：`QueueInfo` → `{messages, consumers, type}`。**探测失败即对象不存在**，映射 404（0.9.1 下 channel 会被服务端关闭——实现上每条命令独立连接/短生命周期 channel，不复用） |
| `amqp queue get <name>` | 单条拉取，调试设施。`--ackmode peek\|ack\|reject`（默认 `peek`：0.9.1 = Get+Nack(requeue)，1.0 = Receive+Release，均非破坏、readonly 可用；`ack`/`reject` 破坏语义，readonly 拦截 `READONLY_VIOLATION`）。payload 展示/截断/`--file` 落盘惯例同 rmq `queue get`。**1.0 等待窗口封顶 5s**：1.0 没有 basic.get 即时语义，Receive 会阻塞到 deadline；空队列最多等 5s 返回 "queue is empty"，不吃满整个命令超时 |

**不提供 `queue ls`**：两条协议都没有 list 原语。hint 引导 `rmq queue ls`。

### exchange 组

| 命令 | 说明 |
|---|---|
| `amqp exchange declare <name>` | `--type direct\|fanout\|topic\|headers --durable=true --auto-delete --args` |
| `amqp exchange delete <name>`（别名 `del`/`rm`） | `--if-unused` **仅 0.9.1**：1.0 无 exchange usage 查询原语（QueueInfo 只覆盖 queue），客户端自检不成立，用 `--if-unused` 报 `UNSUPPORTED_OPERATION` + hint |
| `amqp exchange show <name>` | **仅 0.9.1**（passive declare 探测）。1.0 不可实现：4.3.1 的 AMQP 1.0 management 对 `GET /exchanges/{name}` 直接 `function_clause` 崩溃，重声明探测又有创建副作用——报 `UNSUPPORTED_OPERATION`，hint 引导 `rmq exchange show` |
| `amqp exchange publish <name>` | 见下 |

不提供 `exchange ls`（同 queue ls）。

### publish（旗舰命令）

```
amqp exchange publish <name> --routing-key K (--payload P | --payload-file F)
      [--count N] [--persistent] [--headers k=v,...] [--props '<json>'] [--encoding string|base64]
```

- 比 `rmq exchange publish` 强在：原生 publisher confirm（0.9.1 confirm mode）/ settlement outcome（1.0），delivery mode（`--persistent`），标准属性与自定义头。
- 统一 data 形状 `{exchange, routing_key, sent, confirmed, unroutable}`：
  - 0.9.1：confirmed=ack 计数，unroutable=returned 计数（`mandatory`）。
  - 1.0：confirmed=Accepted 计数，unroutable=Released（不可路由）计数；Rejected（队列拒绝，4.x 带 queue+reason）进 Message/hint。
- `--count` 循环发布（同一连接内），首发即失败 sent=0 直接报错；中途失败走 `RenderPartial` 报已发数量。大 `--count` 时 stderr 打进度。

**`--props` 设计**（输入侧仅 `exchange publish` 一个命令消费）：只收**可移植子集**，客户端校验（合法 JSON + key 白名单 + 值类型），未知 key 报 `CONFIG_INVALID` 并列出合法 key；`app_id` 在 1.0 下无对应，丢弃 + Message 警告。

| `--props` key | 0.9.1 映射 | 1.0 映射 |
|---|---|---|
| `message_id` | MessageId | message-id |
| `correlation_id` | CorrelationId | correlation-id |
| `content_type` | ContentType | content-type |
| `content_encoding` | ContentEncoding | content-encoding |
| `reply_to` | ReplyTo | reply-to |
| `type` | Type | subject |
| `expiration`（ms 字符串） | Expiration | ttl header |
| `priority` | Priority (0-255) | priority header |
| `timestamp` | Timestamp | creation-time |
| `user_id` | UserId | user-id |
| `app_id` | AppId | （丢弃 + 警告） |

分层职责：`--persistent`（delivery_mode=2 快捷 flag）> `--props`（标准属性全量入口）> `--headers k=v,...`（自定义业务头：0.9.1 进 headers table，1.0 进 application-properties）。不为单个属性开独立 flag（避免与 `--props` 优先级打架）。1.0 的 to/reply-to/group-id 由寻址逻辑控制，不开放 `--props` 设置。

### consume

```
amqp consume <queue> [--count N（默认 1）] [--timeout 30s] [--ack|--reject|--requeue（默认）] [--file <dir>]
```

- 一次性模式：收满 N 条或超时退出，envelope 一次返回；文本列 `exchange, routing_key, redelivered, payload`（payload 截断同 rmq 惯例）。**空队列超时**：一条都没收到且超时 → `TIMEOUT`（exit 3）；收到部分消息则正常返回已收部分。
- 定居语义默认 `--requeue`（非破坏，readonly 可用）；`--ack`/`--reject` 破坏语义，readonly 拦截。
- 1.0 消费时 message annotations 带 `x-exchange`/`x-routing-key`；0.9.1 从 delivery 字段取，输出列对齐。
- **`--follow` 流式模式**：首版不实现。其 NDJSON 契约属全局输出契约（见下）。

### binding 组

| 命令 | 说明 |
|---|---|
| `amqp binding bind --exchange E --queue Q [--routing-key K] [--args]` | 1.0 `Bind` 返回 binding path（unbind 凭据）；0.9.1 无 path 概念，unbind 按 (exchange, queue, routing-key, args) 匹配 |
| `amqp binding unbind --exchange E --queue Q [--routing-key K]` | 见上 |

不提供 `binding ls`（协议无 list 原语，hint 引导 `rmq binding ls`）。

## readonly 分界

写操作（declare/delete/purge/publish/bind/unbind、`queue get --ackmode ack|reject`、`consume --ack|--reject`）在 readonly 连接上拦截为 `READONLY_VIOLATION`；`queue show`、`queue get --ackmode peek`、`consume --requeue`、`exchange show`、`conn test` 只读放行。**拦截提前到 dial 之前**（比 rabbitmq connector 更强）：readonly 连接的写操作离线也立即拒绝，不产生任何网络动作。

## 流式输出契约（experimental，全局）

`--follow` 类持续输出打破 envelope 一次性契约，是全局输出契约的修订而非 amqp 私货（后续 `redis subscribe`、`etcd watch` 同样需要）。契约最小集记入 `muxcat help output` 并标记 experimental：NDJSON 每行一个消息对象；非 JSON 模式按行渲染；Ctrl+C 干净退出（exit 0）；中途错误走部分输出语义。amqp `consume --follow` 为契约的第一个实现，等第二个消费者出现时再修订。首版两者都不做。

## 错误映射

| 场景 | 错误码 | 退出码 |
|---|---|---|
| 0.9.1 `403 ACCESS_REFUSED` / 1.0 `amqp:unauthorized-access`（认证失败） | `AUTH_FAILED` | 4 |
| vhost 不存在或无权限，0.9.1（实测 4.3.1：两种情形均为 `403 ACCESS_REFUSED "no access to this vhost"`，530 分支保留兜底） | 按 reason 文本分流：含该文案 → `AUTH_FAILED`，hint 提示检查 vhost 名与 (user, vhost) 权限（可引导 `rmq permission ls`）；其他 403 → 凭据 hint | 4 |
| vhost 不存在，1.0（实测 4.3.1：open 阶段服务端直接强制关闭 TCP，错误链无 condition） | 按握手期 TCP reset 识别 → `CONNECT_FAILED`，hint 提示检查 vhost 名 | 3 |
| 0.9.1 `404 NOT_FOUND` / 1.0 `amqp:not-found`（对象不存在） | `QUERY_ERROR`（"not found"） | 5 |
| 0.9.1 `405 RESOURCE_LOCKED` / 1.0 `amqp:resource-locked`（exclusive 占用） | `QUERY_ERROR` | 5 |
| 0.9.1 `406 PRECONDITION_FAILED` / 1.0 `amqp:precondition-failed`（重复声明参数不一致） | `QUERY_ERROR`（hint 提示参数冲突） | 5 |
| 0.9.1 `541 INTERNAL_ERROR` 且 reason 含 deprecated feature | `QUERY_ERROR`（reason 进 hint，见下） | 5 |
| 1.0 连接旧版 RabbitMQ（< 4.0，握手失败/SASL 拒绝） | `CONNECT_FAILED`（hint 提示 RabbitMQ ≥ 4.0 或改用 `--protocol 0.9.1`） | 3 |
| TCP 拒绝 / 无此主机 | `CONNECT_FAILED` | 3 |
| 超时 | `TIMEOUT` | 3 |
| readonly 写操作 | `READONLY_VIOLATION` | 5 |

**4.x deprecated feature 实测**：0.9.1 声明 `durable=false, exclusive=false` 的瞬态队列被 4.3 默认拒绝（`transient_nonexcl_queues`，541 + 长 reason）。`queue declare` 因此默认 `--durable=true`，且 hint 文案要把 deprecated feature reason 透出来。

## 已知限制

- **无 list 原语**：`queue ls` / `exchange ls` / `binding ls` 均不提供，hint 引导 `rmq`（Management API）。两 connector 分工：管理面 `rmq`，数据面 `amqp`。
- **1.0 管理接口 RabbitMQ 专有**：其他 AMQP 1.0 broker（ActiveMQ Artemis、Qpid 等）只能 publish/consume，declare/show/purge 不可用（报错带 hint）。首版不做通用 1.0 适配。
- **跨协议消息格式**：1.0 发布的消息以 1.0 格式存储，0.9.1 消费有协议转换（headers 保真度有损），反之亦然；stream 以 1.0 格式原生存储。
- 1.0 不支持事务、link resume/exactly-once；0.9.1 事务不封装。
- 1.0 高阶能力未封装：stream filter expressions、single active consumer 通知、WebSocket（Tanzu）——后续迭代按需。
- TLS 由 scheme 决定；`tlsSkipVerify` 已支持，自定义 CA（`tlsCaCert`）待后续迭代（与 rmq 的 TLS 欠债一起）。
- `consume --follow` 与其 NDJSON 契约首版均不实现（见「流式输出契约」）。
- 每条命令独立短连接（短进程模型），无连接池；`--count` 循环内复用同一连接。
- 实现注记：rabbitmqamqp 客户端的 slog 默认会把含 username 的 dial URI 打进 stderr，已用 `SetSlogHandler(io.Discard)` 抑制（muxcat 自己渲染错误，且错误出口统一 `sanitizeErr` 剥凭据）。
