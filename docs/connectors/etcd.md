# etcd connector

etcd connector 接入 etcd v3 API（仅 v3），驱动为 `go.etcd.io/etcd/client/v3`（gRPC，纯 Go，符合 `CGO_ENABLED=0` 基线）。第一版范围：KV 基础（get/put/del）与有界 watch。lease 管理、集群维护（member/alarm/defrag）、auth 管理不做，代码结构留扩展位。

## 配置模型（etcd.json）

```json
{
  "version": 1,
  "instances": {
    "local": { "endpoints": ["127.0.0.1:2379"], "tls": false }
  },
  "connections": {
    "local": {
      "instance": "local", "username": "", "password": "enc:v1:...",
      "readonly": false, "allowDangerous": false, "timeout": "5s"
    }
  },
  "defaultConnection": "local"
}
```

- `instances`：名字 → `{endpoints, tls?, cacert?, cert?, key?}`，纯端点属性。`endpoints` 为 `host:port` 列表（逗号分隔解析，逐条校验端口范围 1-65535）；TLS 开启时 `cacert`/`cert`/`key` 为证书文件路径（`cert` 与 `key` 必须成对），用 crypto/tls 构造 `clientv3.Config.TLS`。
- `connections`：名字 → `{instance, username?, password?, readonly?, allowDangerous?, timeout?}`。`password` 以 `enc:v1:` 加密落盘，永不在输出中回显；`timeout` 为 Go duration 字符串，覆盖全局 `--timeout`。
- 同一 instance 可挂多个连接（如 admin / 只读账号），凭证与策略都是 connection 属性。
- `defaultConnection`：缺省连接名；`-c/--conn` 未指定时使用，两者皆无报 `CONN_NOT_FOUND`。
- `conn add <name>` 同名创建 instance 与 connection；`conn rm` 删除连接时，同名 instance 无其他引用则一并删除。

## 命令

### conn 组

| 命令 | 说明 |
|---|---|
| `etcd conn add <name> --endpoints <h:port>[,<h:port>...] [--username u] [--password p] [--tls --cacert --cert --key] [--readonly] [--allow-dangerous] [--timeout 5s] [--set-default]` | 新增连接。非 TTY 缺 `--endpoints` 报 `MISSING_ARGUMENT`；TTY 缺参走 huh 表单补全（endpoints/username/password/tls/readonly，证书路径仅走 flag）。`--password` 为明文凭据参数，使用时 stderr 警告。无默认连接时自动设为默认 |
| `etcd conn ls` | 列出连接（name / endpoints / user / tls / readonly / default 标记） |
| `etcd conn show <name>` | 连接详情；password 不回显 |
| `etcd conn rm <name> [--yes]` | 删除连接；非 TTY 必须 `--yes`，TTY 弹确认 |
| `etcd conn default <name>` | 设为默认连接 |
| `etcd conn test <name>` | 对 `endpoints[0]` 调 maintenance Status，返回 `{ok, latency_ms, version}` |

### KV 与 watch

以下命令均支持 `-c/--conn` 与全局 `--timeout`（`watch` 的 `--timeout` 为本地 flag，见下）。

| 命令 | 参数 / flag | data 形状 |
|---|---|---|
| `etcd get <key>` | `--prefix`、`--keys-only`（需配合 `--prefix`）、`--rev N`、`--limit N`（全局，默认 1000，0 不截断） | 单 key：`{value}`（key 不存在 → `value: null`），文本模式裸输出值；`--prefix`：columns `key, value, create_rev, mod_rev, version, lease`（按 key 排序，`--keys-only` 省略 value 列），命中 limit 时 `meta.truncated: true` |
| `etcd put <key> <value>` | `--lease-id N`（挂到已有 lease；lease 管理本身不做） | `{value: "OK"}`，裸输出 OK。写操作，readonly 连接拒绝 |
| `etcd del <key>` | `--prefix`（危险操作，需连接开 allowDangerous） | `{deleted: N}`。写操作，readonly 连接拒绝 |
| `etcd watch <key>` | `--prefix`、`--rev N`、`--max-events N`（默认 10）、`--timeout <duration>`（本地 flag，默认 10s，遮蔽全局 `--timeout`） | 有界收集：凑满 max-events 或超时后一次性输出 events 数组 `[{type: "PUT"\|"DELETE", key, value, mod_revision}, ...]`（DELETE 事件无 value）；文本模式输出缩进 JSON（按 json 高亮），JSON envelope 的 data 即该数组 |

## 拦截规则

- `readonly: true`：拒绝一切写（put/del 及未来新增写命令），报 `READONLY_VIOLATION`（退出码 5），hint 指向换用可写连接。
- `allowDangerous: false`（默认）：拒绝 `del --prefix`，报 `UNSUPPORTED_OPERATION`（退出码 2，与 output 中央映射一致，属用法类），hint 指向连接配置。
- 拦截先于网络拨号：readonly 与 dangerous 检查在 dial 之前完成，无服务端也生效。两类拦截叠加时 readonly 优先。

## 错误映射

| 场景 | 错误码 | 退出码 |
|---|---|---|
| auth failed / invalid auth token / permission denied | `AUTH_FAILED` | 4 |
| dial 失败、连接拒绝 | `CONNECT_FAILED` | 3 |
| 超时（context deadline、gRPC DeadlineExceeded） | `TIMEOUT` | 3 |
| 其余 etcd 错误（revision compacted 等） | `QUERY_ERROR` | 5 |

## 已知限制

- 仅 v3 API；lease 管理、集群维护（member/alarm/defrag）、auth 管理不在第一版范围（`put --lease-id` 可挂已有 lease）。
- 拦截是"防误操作"而非安全边界，强制约束需服务端 auth/权限。
- `get --prefix` 的 `--limit` 是服务端 limit（`WithLimit`），截断以 `resp.More` 为准并回显 `meta.truncated`。
- `watch` 是有界收集器而非持续 follower：必然自行终止；收集窗口内连接断开按错误处理，超时则输出已收集的事件（可能为空数组）。
- TLS 支持 CA / 客户端证书文件路径，不支持在线配置证书内容。
