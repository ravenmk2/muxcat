# emqx connector

EMQX connector 通过 EMQX 5.x 的 HTTP API（`/api/v5`）接入，实现为纯 `net/http` 客户端，无外部驱动依赖（符合 `CGO_ENABLED=0` 基线），已实测 5.8.6（最后的开源版本）。覆盖：连接管理、集群状态概览、节点/监听器浏览、客户端查询与踢出、订阅与主题（路由表）查询、消息发布、metrics/stats 计数器、告警、黑名单增删、保留消息管理。

API 核对的权威来源：EMQX 5.8.6 开源版实例实测 + 官方 HTTP API 文档。

## 配置模型（emqx.json）

```json
{
  "version": 1,
  "instances": {
    "local": { "url": "http://127.0.0.1:18083" }
  },
  "connections": {
    "local": {
      "instance": "local",
      "username": "admin", "password": "enc:v1:...",
      "apiKey": "<key>", "apiSecret": "enc:v1:...",
      "readonly": false, "timeout": "30s"
    }
  },
  "defaultConnection": "local"
}
```

- `instances`：名字 → `{url}`，纯端点属性。url 必须含 scheme（`http`/`https`），**不得内嵌 user:password@**（防明文凭据落盘，违反报 `CONFIG_INVALID`）。
- `connections`：名字 → `{instance, username?, password?, apiKey?, apiSecret?, readonly?, timeout?}`。**双凭据模型**：dashboard 账号对（username+password）与 API Key 对（apiKey+apiSecret）至少配置一对完整凭据；`username↔password`、`apiKey↔apiSecret` 必须成对（schema 以 `anyOf` + `dependentRequired` 强制）。`password`/`apiSecret` 以 `enc:v1:` 加密落盘，永不在输出中回显；`apiKey` 视同用户名不加密；`timeout` 为 Go duration 字符串，覆盖全局 `--timeout`。
- **多用户/多凭据**：凭据是 connection 属性——同一 instance 可挂多个连接（不同 dashboard 用户或 API Key）。
- `conn add <name>` 同名创建 instance 与 connection；`conn rm` 删除连接时，同名 instance 无其他引用则一并删除。

## 命令

### conn 组

| 命令 | 说明 |
|---|---|
| `emqx conn add <name> --url <u> [--username u --password p] [--api-key k --api-secret s] [--readonly] [--timeout 30s] [--set-default]` | 新增连接。非 TTY 缺 `--url` 报 `MISSING_ARGUMENT`；TTY 缺参走 huh 表单补全（url/凭据类型选择 dashboard 或 apikey 或两者/readonly，密码类输入 EchoModePassword）。`--password`/`--api-secret` 为明文凭据参数，使用时 stderr 警告后加密落盘。无默认连接时自动设为默认 |
| `emqx conn ls` | 列出连接（name / url / auth / username / readonly / default 标记）；auth 列取值 `dashboard` / `apikey` / `dashboard+apikey` |
| `emqx conn show <name>` | 连接详情；password/apiSecret 不回显（Value map 无这两个字段），apiKey 视同用户名可见 |
| `emqx conn rm <name> [--yes]` | 删除连接；非 TTY 必须 `--yes`，TTY 弹确认 |
| `emqx conn default <name>` | 设为默认连接 |
| `emqx conn test <name>` | 对配置的**每对凭据各验证一次**（apiKey 对 → `GET /api/v5/nodes` Basic；dashboard 对 → login + `GET /api/v5/nodes` Bearer），返回 `{ok, auth:{dashboard, apiKey}, version, edition, latency_ms}`。version 取自 login 响应或 nodes[0].version |

### status / node / listener

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `emqx status` | — | `GET /api/v5/status` + `GET /api/v5/nodes` + `GET /api/v5/monitor_current` 聚合 | 结构化 Value `{version, edition, nodes:[{node,status,uptime,connections,live_connections}], cluster:{monitor_current 原样}}` |
| `emqx node ls` | — | `GET /api/v5/nodes` | 列 `node, version, edition, uptime, connections, live_connections` |
| `emqx node show [node]` | node 缺省取集群第一个节点 | `GET /api/v5/nodes/:node` | 服务端响应原样（Value） |
| `emqx listener ls` | — | `GET /api/v5/listeners` | 列 `id, type, name, bind, running, current_connections, max_connections` |

### client 组

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `emqx client ls` | `--clientid` / `--username` / `--like-clientid` / `--like-username`（直通服务端过滤）、全局 `--limit` | `GET /api/v5/clients`（分页 `page`/`limit` + `meta.hasnext`） | 列 `clientid, username, node, ip_address, port, connected, connected_at` |
| `emqx client show <clientid>` | — | `GET /api/v5/clients/:clientid`（返回单元素数组，取首项） | 服务端响应原样（Value） |
| `emqx client kick <clientid>` | — | `DELETE /api/v5/clients/:clientid` | `{clientid, kicked: true}` + Message；**写管控** |

clientid 一律 `url.PathEscape` 后入 path。

### sub / topic

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `emqx sub ls` | `--clientid` / `--topic`（服务端过滤） | `GET /api/v5/subscriptions`（分页） | 列 `clientid, topic, qos, node` |
| `emqx topic ls` | `--topic`（服务端过滤） | `GET /api/v5/topics`（分页） | 列 `topic, node` |

EMQX 5.x 没有 `/api/v5/routes`（404）；路由表即 `/api/v5/topics`。

### pub

```
emqx pub <topic> (--payload s | --file f) [--qos 0|1|2] [--retain]
```

- `--payload` 与 `--file` 二选一，`-` 读 stdin（TTY 下拒绝并报 `MISSING_ARGUMENT`）。请求体 `{topic, payload, qos, retain, payload_encoding}` → `POST /api/v5/publish`；payload 默认 `payload_encoding:"plain"`，**非 UTF-8 二进制自动转 base64**（`payload_encoding:"base64"`，5.8.6 的 `/publish` 会校验 plain payload 为 UTF-8，否则 400）。
- **202 + `{"message":"no_matching_subscribers","reason_code":16}` 属正常结果**（无订阅者匹配），渲染为 data（含 `status`、`message`、`reason_code`），不报错；200 同理（响应体字段并入 data）。
- **写管控**：readonly 连接报 `READONLY_VIOLATION`。

### metric / alarm

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `emqx metric ls` | `--node` / `--match <substr>`（均客户端过滤） | `GET /api/v5/metrics` + `GET /api/v5/stats`（同为 `[{node, <metrics\|stats>:{name:value}}]`，合并） | 列 `node, kind(metric\|stat), name, value` |
| `emqx alarm ls` | `--history`（列出已解除的历史告警，默认列出激活中） | `GET /api/v5/alarms?activated=true\|false` | 列 `name, node, message, activate_at, deactivate_at` |

### banned 组

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `emqx banned ls` | — | `GET /api/v5/banned`（分页） | 列 `as, who, by, reason, at, until` |
| `emqx banned add <who> --as clientid\|username\|peerhost [--reason r] [--until ts]` | `--until` 为 unix 秒时间戳 | `POST /api/v5/banned`，body `{as, who, reason?, until?}` | `{as, who, banned: true}` + Message；**写管控** |
| `emqx banned rm <who> --as ...` | — | `DELETE /api/v5/banned/:as/:who`（均 path escape） | `{as, who, banned: false}` + Message；**写管控** |

### retained 组

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `emqx retained ls` | — | `GET /api/v5/mqtt/retainer/messages`（分页） | 列 `topic, msgid, from_clientid, from_username, publish_at` |
| `emqx retained show <topic>` | — | `GET /api/v5/mqtt/retainer/message/:topic`（path escape） | 服务端响应（Value）；**payload 为 base64，UTF-8 文本自动解码显示**（非 UTF-8 保持 base64 并标注），截断到 4KB 时标注 `payload_truncated/note` |
| `emqx retained rm <topic>` | — | `DELETE /api/v5/mqtt/retainer/message/:topic` | `{topic, deleted: true}` + Message；**写管控** |

## 双凭据认证选路

client 持两对凭据（内存明文，来自 `enc:v1:` 解密），`send(ctx, method, path, params, body, requireDashboard)` 按请求选路：

1. `requireDashboard=true`（机制预留，当前命令集不使用）：无 dashboard 凭据 → `AUTH_FAILED` + hint「该命令需要 dashboard 账号」；有 → 走 JWT 路径。
2. 否则：有完整 API Key 对 → **每请求 HTTP Basic**（零 login 开销，首选）；无 → dashboard username/password **惰性 login**（`POST /api/v5/login` 换 Bearer JWT，缓存约 55 分钟，仅内存），401 时重登重试一次。

v1 命令集内所有端点两对凭据均可用；仅 `/api_key`、`/users` 管理端点强制 dashboard JWT（均在 v1 范围外）。

## 错误映射

| 场景 | 错误码 | 退出码 |
|---|---|---|
| HTTP 401 / 403 | `AUTH_FAILED`（hint 按所用凭据区分：apiKey/apiSecret 或 dashboard username/password） | 4 |
| login 失败（401/403） | `AUTH_FAILED`（hint 检查 username/password） | 4 |
| HTTP 404 | `QUERY_ERROR`（消息含 "not found"，hint 提示检查 node/clientid/topic/as+who 寻址） | 5 |
| 连接拒绝、无此主机 | `CONNECT_FAILED` | 3 |
| 超时（客户端或 context deadline） | `TIMEOUT` | 3 |
| 其余非 2xx | `QUERY_ERROR`（解析 EMQX 错误体 `{code, message}` 的 message，body 截断 512 字符） | 5 |
| readonly 连接的写操作（pub、client kick、banned add/rm、retained rm） | `READONLY_VIOLATION` | 5 |
| `pub` 的 202 no_matching_subscribers | 不报错（渲染为 data） | 0 |
| 凭据对不完整 / url 含 userinfo / `--qos` 越界 | `CONFIG_INVALID` / `MISSING_ARGUMENT` | 2 |

分页：列表端点统一 `page`/`limit` 参数 + `meta{count,limit,page,hasnext}`；单页取 `min(剩余, 100)`，按 `hasnext` 翻页至全局 `--limit`，截断时 envelope `meta.truncated=true`。

## 已知限制

- `/api/v5/routes` 不存在（404），路由表以 `topic ls`（`/api/v5/topics`）呈现。
- API Key 凭据不能管理 `/api_key` 与 `/users`（这两类端点强制 dashboard JWT；v1 不涉及）。
- EMQX 5.8.6 创建 API Key 不传 `desc` 触发 INTERNAL_ERROR bug（v1 不涉及 apikey 管理，仅记录）。
- HTTP API 无消息消费能力（订阅/拉取消息需 MQTT 客户端，不在本 connector 范围）。
- `retained show` 的 payload 服务端为 base64 编码：UTF-8 文本自动解码显示，非 UTF-8 保持 base64 并附 note；超 4KB 截断并标注，防大二进制刷屏。
- 单次响应体上限 64MB。
- 写操作守卫只有 readonly 连接（`READONLY_VIOLATION`），不设 `--yes` 确认（显式命令即意图）。
- TLS 由 url scheme 决定，不支持自定义 CA / 跳过证书校验（后续迭代按需加）。
