# mongodb connector

MongoDB connector（MongoDB 6 / 7 / 8，优先覆盖 8），基于官方驱动 `go.mongodb.org/mongo-driver/v2`，纯 Go 实现，CI 与 release 的 `CGO_ENABLED=0` 基线不受影响。

Phase 1 只覆盖连接管理（`conn` 命令组）；查询命令在后续阶段落地。

## 配置模型（mongodb.json）

```json
{
  "version": 1,
  "instances": {
    "local": { "hosts": ["127.0.0.1:27017"], "tls": false, "replicaSet": "rs0" }
  },
  "connections": {
    "local": {
      "instance": "local",
      "username": "app",
      "password": "enc:v1:...",
      "authSource": "admin",
      "database": "shop",
      "timeout": "5s"
    }
  },
  "defaultConnection": "local"
}
```

- `instances`：名字 → `{hosts, tls?, replicaSet?}`，纯端点属性。`hosts` 为 `host:port` 数组，phase 1 只支持单条目；条目可省略端口，读取时默认补 27017。`tls: true` 要求 TLS 加密连接（最低 TLS 1.2）。`replicaSet` 保留字段，目前仅在 `--uri` 携带 `replicaSet` 参数时写入。
- `connections`：名字 → `{instance, username?, password?, authSource?, database?, timeout?}`；`password` 以 `enc:v1:` 加密 blob 落盘、永不在输出中回显；`authSource` 为认证库，设置 username 而未指定 authSource 时默认 `admin`；`database` 为空表示不选默认库；`timeout` 为 Go duration 字符串（如 `5s`），覆盖全局 `--timeout`。
- `defaultConnection`：缺省连接名；`-c/--conn` 未指定时使用，两者皆无报 `CONN_NOT_FOUND`。
- `conn add <name>` 同名创建 instance 与 connection（instance 名 = 连接名）；`conn rm` 删除连接时，同名 instance 无其他引用则一并删除。

## 命令

| 命令 | 说明 |
|---|---|
| `mongodb conn add <name> --host <host> [--port 27017] [--username U] [--password P] [--auth-source DB] [--database DB] [--tls] [--timeout 5s] [--set-default] [--uri mongodb://...]` | 新增连接。非 TTY 缺 `--host`（且无 `--uri`）报 `MISSING_ARGUMENT`；两者皆空且是 TTY 时走 huh 表单补全（其余字段只补全未显式给出的）。`--password` 明文传参会打 stderr 警告后以 `enc:v1:` 加密落盘；`--uri` 内含明文密码同样告警。无默认连接时自动设为默认 |
| `mongodb conn ls` | 列出连接（name / addr / user / authSource / database / tls / default 标记），不泄露密码 blob |
| `mongodb conn show <name>` | 连接详情（不回显密码；`replicaSet` 仅在设置时展示） |
| `mongodb conn rm <name> [--yes]` | 删除连接；非 TTY 必须 `--yes`，TTY 弹确认；同名 instance 无其他引用时级联删除，默认连接被删后自动重选 |
| `mongodb conn default <name>` | 设为默认连接 |
| `mongodb conn test <name>` | ping + `buildInfo` + `hello`，返回 `{ok, latency_ms, version, topology, maxWireVersion}`；topology 取值 `standalone` / `replicaset` / `sharded` |

### URI 输入说明

`conn add --uri "mongodb://[user:pass@]host[:port][/db][?authSource=..&tls=..&replicaSet=..]"` 解析出的 host / port / username / password / authSource / database / tls / replicaSet 只作**默认值**，显式 flag 优先。解析基于驱动的 `x/mongo/driver/connstring.ParseAndValidate`。

- `mongodb+srv://` 拒绝：`CONFIG_INVALID`，hint 提示改用 `--host/--port`。
- 多 host（逗号分隔的副本集种子）拒绝：`CONFIG_INVALID`，hint 说明暂不支持。
- 解析失败的错误消息不回显 URI 中的 userinfo（密码脱敏为 `***`）。

## 连接构建

不拼接含凭据的 URI 字符串：`options.Client()` + `SetHosts`（规范化后的 host:port）+ `SetAuth(options.Credential{Username, Password, AuthSource})`（密码从 `enc:v1:` blob 解密后仅存内存）+ `SetTLSConfig`（`tls: true` 时，最低 TLS 1.2）+ `SetReplicaSet`；连接级 `timeout` 同时设置 ConnectTimeout / Timeout / ServerSelectionTimeout。`mongo.Connect(opts)` 后经 `Ping`（read preference primary）验证可达性，失败归类走错误码映射。

## 错误码映射

| 条件 | 错误码 |
|---|---|
| CommandError 13（Unauthorized）/ 18（AuthenticationFailed），或消息含 "auth" | `AUTH_FAILED` |
| dial tcp / connection refused / no such host（含 server selection 包裹的拨号错误） | `CONNECT_FAILED` |
| `mongo.IsTimeout` / `context.DeadlineExceeded` | `TIMEOUT` |
| 其余 server selection 错误 | `CONNECT_FAILED` |
| 其他驱动错误 | `QUERY_ERROR` |

## 示例

```sh
muxcat mongodb conn add local --host 127.0.0.1 --set-default
muxcat mongodb conn add prod --host db.example.com --username app --password s3cret --auth-source admin --database shop --tls --set-default
muxcat mongodb conn add rs --uri "mongodb://app:s3cret@db.example.com:27018/shop?replicaSet=rs0&tls=true"
muxcat mongodb conn ls
muxcat mongodb conn show prod --json
muxcat mongodb conn test prod
muxcat mongodb conn rm local --yes
```

## 已知限制

- 单 host：`hosts` 数组只取一个条目，多 host URI 与 `mongodb+srv://` 均拒绝。
- 无 readonly 策略（phase 1 无查询路径）。
- 查询命令在 phase 2 落地，当前仅有 `conn` 命令组。
