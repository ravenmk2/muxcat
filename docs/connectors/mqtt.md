# mqtt connector

mqtt connector 走 MQTT **wire 协议**（数据面），与 emqx connector（Management HTTP API，管理面）互补：管理找 `emqx`，收发找 `mqtt`。单一 connector、独立配置文件 `mqtt.json`、**无别名**，**同时支持 MQTT 3.1.1 与 MQTT 5.0**：协议版本是 instance 属性，命令面统一，按 instance 的 `protocolVersion` 字段分派到两套客户端实现。传输支持 TCP（`mqtt://`）、TLS（`mqtts://`）、WebSocket（`ws://`/`wss://`）。

**已验证**（2026-09-28，对 EMQX 5.x 实测）：无认证实例上，v3/v5 × mqtt/ws 完成 pub→sub 回路全链路。

## 依赖与版本基线

| 协议 | 客户端库 | License | 基线 |
|---|---|---|---|
| 3.1.1 | `github.com/eclipse/paho.mqtt.golang` v1.5.1 | EPL-2.0 / EDL-1.0 | 任意 3.1.1 broker（EMQX、Mosquitto、HiveMQ 等） |
| 5.0 | `github.com/eclipse/paho.golang` v0.23.0 | EPL-2.0 / EDL-1.0 | 任意 5.0 broker |

- 均纯 Go，符合 `CGO_ENABLED=0` 基线。WebSocket 传输依赖 `gorilla/websocket`（传递依赖）。
- 单测数据面用 mochi-mqtt（`github.com/mochi-mqtt/server/v2`，纯 Go）内嵌 broker，v3/v5 各一条真实回路，不依赖外部服务。

## 配置模型（mqtt.json）

```json
{
  "version": 1,
  "instances": {
    "prod": { "url": "mqtts://broker.example.com:8883", "protocolVersion": 5 }
  },
  "connections": {
    "prod-admin": {
      "instance": "prod", "username": "admin",
      "password": "enc:v1:...", "clientId": "muxcat-ops",
      "readonly": false, "timeout": "10s", "tlsSkipVerify": false
    }
  },
  "defaultConnection": "prod-admin"
}
```

- `instances`：`{url, protocolVersion}`。**protocolVersion 是 instance 属性**（端点方言）：严格枚举 `3` / `5`（默认 `3`，缺省不写盘），其他值报 `CONFIG_INVALID` 并列出合法值。url 规范 scheme 为 `mqtt`/`mqtts`/`ws`/`wss`（业界惯例，同 mqtt.org URI scheme / MQTT.js / MQTTX）；`conn add` 时把别名归一化后存储（`tcp://`→`mqtt://`、`ssl://`/`tls://`→`mqtts://`），配置里永远是规范 scheme。缺省端口自动补齐：mqtt 1883、mqtts 8883、ws 80、wss 443。ws/wss 可带 path（如 `ws://host:8083/mqtt`）。**不得内嵌 user:password@**（违反报 `CONFIG_INVALID`，hint 引导 `--username/--password`）。
- `connections`：`{instance, username?, password?, clientId?, readonly?, timeout?, tlsSkipVerify?}`。`clientId` 缺省为每次会话随机生成 `muxcat-<pid>-<rand>`；会话固定 clean start（批模式短会话，无 cleanStart 配置项）。数据命令（`pub`/`sub`/`conn test`）支持 `--client-id` 命令级覆盖：flag > connection.clientId > 随机。**注意**：MQTT 规范要求 broker 断开同 clientID 的旧会话——并发跑多个命令时不要复用同一固定 ID，否则会互相踢断。
- `tlsSkipVerify`：仅对 `mqtts://`/`wss://` 有意义；`mqtt://`/`ws://` 下设置为 true 报 `CONFIG_INVALID`。每次以 skip-verify 建连时 stderr 打一行安全警告。
- 凭据仅活于 connection；密码经 client options（内存）传给库，**不进 URL、不进错误消息**；`enc:v1:` 落盘；`conn ls/show` 不回显。schema：`schema/mqtt.schema.json`。
- dial 层按规范 scheme 直接拨号：v5 实现按 `mqtt`/`mqtts`/`ws`/`wss` 分派 net/tls/websocket 拨号（原生支持，无需翻译）；v3 库只认 `tcp`/`ssl`/`ws`/`wss`，由 dial 层做 `mqtt`→`tcp`、`mqtts`→`ssl` 映射（仅建连参数，不回写配置）。

## 命令

### conn 组

与 amqp connector 同构：`conn add/ls/show/rm/default/test`。

- `conn add <name> --url <u> [--protocol-version 3|5] [--username u] [--password p] [--client-id id] [--readonly] [--timeout <dur>] [--tls-skip-verify] [--set-default]`。`--url`/`--protocol-version` 建 instance，其余建 connection。TTY 缺参走 huh 表单（密码 EchoModePassword）；非 TTY 缺 `--url` 报 `MISSING_ARGUMENT`。
- `conn ls` 文本列 `name, url, protocol, username, readonly, default`。
- `conn test <name>`：真实 dial + CONNACK，data `{ok, latency_ms, protocol, transport}` + Message。MQTT 的 CONNACK 不携带 broker product/version，故只报协议版本与传输方式。

### pub

```
mqtt pub <topic> (--payload P | --payload-file F | --payload -) [--qos 0|1|2] [--retain] [--count N（默认 1，上限 100000）] [--client-id id]
```

- payload 三选一互斥：`--payload` 直接给、`--payload-file` 读文件（上限 1MB）、`--payload -` 读 stdin；缺省报 `MISSING_ARGUMENT`。
- QoS 1/2 等 PUBACK/PUBCOMP（v3 token 等待 / v5 PublishResponse）后才返回；失败计入错误。
- 统一 data 形状 `{topic, qos, retain, sent}`。`--count` 循环发送（同一连接内），首发即失败 sent=0 直接报错；中途失败走 `RenderPartial` 报已发数量；大 `--count` 时 stderr 打进度。
- pub 是写操作：readonly 连接上 **dial 前** 拦截 `READONLY_VIOLATION`。

### sub（批模式）

```
mqtt sub <topic-filter> [--count N（默认 1，上限 10000）] [--timeout 30s] [--qos 0|1|2（默认 1）] [--file <dir>] [--client-id id]
```

- 一次性模式：收满 N 条或超时退出，envelope 一次返回；文本列 `topic, qos, retained, payload`（payload 截断 1KiB，非 UTF-8 报大小；`--json` 全量，二进制 base64 + `payload_encoding`）。**0 条超时** → `TIMEOUT`（exit 3），hint 提示放宽 `--timeout` 或检查 topic filter；收到部分消息则正常返回已收部分。
- `--file <dir>` 每消息落盘 0600（`<filter>-0.bin`…，filter 中非 `[a-zA-Z0-9-_.]` 字符转 `_`），envelope 给文件路径列表。
- 会话为 clean start + 随机 clientID，批模式只收订阅窗口内的新消息（及 retained 消息）；窗口内断连不做重订阅（短会话模型，库 auto-reconnect 关闭）。

## readonly 分界

`pub` 在 readonly 连接上拦截为 `READONLY_VIOLATION`（**dial 之前**，离线也立即拒绝）；`sub`、`conn test` 只读放行。

## 错误映射

| 场景 | 错误码 | 退出码 |
|---|---|---|
| v3 connack 4/5（bad username/password、not authorized）/ v5 reason 0x86/0x87/0x8A | `AUTH_FAILED` | 4 |
| v3 connack 1（protocol refused）/ v5 reason 0x84 | `CONNECT_FAILED`（hint 提示检查 --protocol-version） | 3 |
| v3 connack 2（id rejected）/ v5 reason 0x85 | `CONNECT_FAILED`（hint 提示检查 clientId） | 3 |
| v3 connack 3（server unavailable）/ v5 reason 0x88/0x89 | `CONNECT_FAILED` | 3 |
| v5 其他未知 reason code | `CONNECT_FAILED`（带十六进制 reason code） | 3 |
| TCP 拒绝 / 无此主机 | `CONNECT_FAILED` | 3 |
| 超时（dial、操作、sub 0 条） | `TIMEOUT` | 3 |
| readonly 写操作 | `READONLY_VIOLATION` | 5 |
| 其他操作期错误（publish/subscribe） | `QUERY_ERROR` | 5 |

所有错误出口统一 `sanitizeErr` 剥凭据（明文及 percent-encoded 形式）；错误消息不含含凭据的连接串。

## 已知限制

- **无管理原语**：无 session/retained/订阅查询——管理面找 `emqx`（EMQX）或对应 broker 的管理 connector。
- **`--follow` 流式订阅不实现**：其 NDJSON 契约属全局输出契约（见 docs/connectors/amqp.md「流式输出契约」），等第二个消费者出现时再修订。
- MQTT 5 专属属性（User Property、Topic Alias、Message Expiry 等）不做命令暴露——v5 仅作为协议版本选项。
- 遗嘱消息（LWT）、keep-alive 自定义不开放（v3 用库默认值，v5 固定 30s）；clean start 固定 true，无持久会话。
- 每条命令独立短连接（短进程模型），无连接池；`--count` 循环内复用同一连接。
- 共享订阅（`$share/`）语义由 broker 处理，topic filter 原样透传，不做专门封装。
- TLS 由 scheme 决定；`tlsSkipVerify` 已支持，自定义 CA（`tlsCaCert`）待后续迭代。
