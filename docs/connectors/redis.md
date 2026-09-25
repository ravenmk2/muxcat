# redis connector

Redis connector 接入 Redis standalone 实例（Redis 6+，含 Redis 8），驱动为 `github.com/redis/go-redis/v9`（纯 Go，符合 `CGO_ENABLED=0` 基线）。第一版不做 Cluster / Sentinel / Pub-Sub / Monitor。

## 配置模型（redis.json）

```json
{
  "version": 1,
  "instances": {
    "local": {
      "host": "127.0.0.1", "port": 6379,
      "username": "", "password": "enc:v1:...",
      "db": 0, "tls": false
    }
  },
  "connections": {
    "local": { "instance": "local", "db": 0, "readonly": false, "allowDangerous": false, "timeout": "5s" }
  },
  "defaultConnection": "local"
}
```

- `instances`：名字 → `{host, port, username?, password?, db?, tls?}`。`username` 可空，空表示 default 用户（对接 requireauth 与 ACL 均可）；`password` 以 `enc:v1:` 加密落盘，永不在输出中回显。
- `connections`：名字 → `{instance, db?, readonly?, allowDangerous?, timeout?}`；`db` 覆盖 instance 的逻辑库；`timeout` 为 Go duration 字符串，覆盖全局 `--timeout`。
- `defaultConnection`：缺省连接名；`-c/--conn` 未指定时使用，两者皆无报 `CONN_NOT_FOUND`。
- `conn add <name>` 同名创建 instance 与 connection；`conn rm` 删除连接时，同名 instance 无其他引用则一并删除。

## 命令

### conn 组

| 命令 | 说明 |
|---|---|
| `redis conn add <name> --host <h> [--port 6379] [--db 0] [--username u] [--password p] [--tls] [--readonly] [--allow-dangerous] [--timeout 5s] [--set-default]` | 新增连接。非 TTY 缺 `--host` 报 `MISSING_ARGUMENT`；TTY 缺参走 huh 表单补全（host/port/db/username/password/tls/readonly）。`--password` 为明文凭据参数，使用时 stderr 警告。无默认连接时自动设为默认 |
| `redis conn ls` | 列出连接（name / addr / db / tls / readonly / default 标记） |
| `redis conn show <name>` | 连接详情；password 不回显 |
| `redis conn rm <name> [--yes]` | 删除连接；非 TTY 必须 `--yes`，TTY 弹确认 |
| `redis conn default <name>` | 设为默认连接 |
| `redis conn test <name>` | PING + 读 `INFO server` 的 redis_version，返回 `{ok, latency_ms, version}` |

### 通用数据命令

以下命令均支持 `-c/--conn` 与全局 `--timeout`。

| 命令 | 参数 / flag | data 形状 |
|---|---|---|
| `redis exec <cmd> [args...]` | `--binary hex\|base64`（默认 hex）、`--max-bytes N`（默认 4096）、`--highlight` | `{type, value}`，type ∈ string/integer/double/boolean/array/map/null |
| `redis get <key>` | `--binary`、`--max-bytes`、`--highlight` | `{value}`，key 不存在 → `value: null` |
| `redis set <key> [value]` | `--ttl <duration>`（如 30s）、`--nx`、`--xx`、`--file <path>`（从文件读值，与位置参数互斥；二进制安全；`-` 读 stdin） | `{value}`，OK / null（nx/xx 条件不满足） |
| `redis del <key> [key...]` | — | `{deleted: N}` |
| `redis keys [pattern]` | `--limit N`（默认 1000，0 不截断） | columns/rows 单列 `key`；SCAN 实现，绝不使用 KEYS |
| `redis type <key>` | — | `{value}`，不存在 → "none" |
| `redis ttl <key>` | `--ms`（改用 PTTL） | `{value}` 秒/毫秒，-1 无过期 / -2 不存在 |
| `redis info [section]` | — | 嵌套 map `{section: {field: value}}`；无参用 INFO 默认段 |
| `redis dbsize` | — | `{value: N}` |

### 数据结构读命令

均支持 `--binary` / `--max-bytes`；多行结果（hgetall/lrange/smembers/zrange/config get）受全局 `--limit`（默认 1000，0 不截断）约束，hget 为单值不适用。

| 命令 | 参数 / flag | data 形状 |
|---|---|---|
| `redis hget <key> <field>` | — | `{value}` |
| `redis hgetall <key>` | — | columns `field, value` |
| `redis lrange <key> <start> <stop>` | — | columns `index, value`（index = start + 偏移量；负 start 保留负数下标） |
| `redis smembers <key>` | — | 单列 `member` |
| `redis zrange <key> <start> <stop>` | `--rev` | columns `member, score`（默认带 score） |

写操作（HSET/LPUSH/SADD/ZADD 等）与未覆盖的命令统一走 `redis exec` 直通。

### Lua 与配置读取

| 命令 | 参数 / flag | data 形状 |
|---|---|---|
| `redis eval <script>` | `--file <path>`（从文件读脚本，与位置参数二选一）、`--key`（可重复，顺序即 KEYS[]）、`--arg`（可重复，顺序即 ARGV[]）、`--binary`、`--max-bytes`、`--highlight` | `{type, value}`，同 exec |
| `redis config get [pattern]` | `--binary`、`--max-bytes` | columns `field, value`；pattern 默认 `*` |

exec/eval 的 `type` 枚举为 string/integer/double/boolean/array/map/null：go-redis 的通用应答不区分 simple string 与 bulk string，统一报 `string`；RESP3 的 double 报 `double`、bool 报 `boolean`，big number 以十进制字符串形式归入 `integer`。

## 拦截规则

connector 侧命令名分类，exec 与结构化命令共用一张分类表：

- `readonly: true`：仅允许读命令（get/mget/getrange/strlen/exists/ttl/pttl/type/scan/sscan/hscan/zscan/randomkey/hget 族/lrange 族/smembers 族/zrange 族/xinfo/xrange/xlen/dbsize/info/config get/ping/echo/object/memory usage），违反报 `READONLY_VIOLATION`（退出码 5）。`eval` 属写操作，readonly 连接拒绝。
- `allowDangerous: false`（默认）：拒绝 `FLUSHALL FLUSHDB SHUTDOWN DEBUG KEYS RESET FAILOVER REPLICAOF SLAVEOF SWAPDB SCRIPT` 以及 `CONFIG` 的非 GET 子命令，报 `UNSUPPORTED_OPERATION`（退出码 2，与 output 中央映射一致，属用法类），hint 指向连接配置。`CONFIG GET` 是唯一按子命令拆分放行的命令。

负数位置参数（如 `lrange queue 0 -1`、`exec ZRANGE board 0 -1`）是 Redis 惯例写法：exec / lrange / zrange 已关闭 flag 与位置参数的交错解析，flag 需置于位置参数之前，位置参数之后的一律按参数处理。

## 值渲染

- 合法 UTF-8 且所有 rune 可打印（或为 `\n`/`\t`/`\r`）的值原样输出；其余（非法 UTF-8、含控制字符）按 `--binary` 编码（默认 hex，可选 base64）。
- 单值超过 `--max-bytes`（默认 4096）时截断并置 `meta.truncated: true`；`--max-bytes 0` 不截断。
- 渲染规则对 exec/eval 返回值递归生效（array/map 内每个字符串元素独立判定）。
- integer/double/boolean 等标量返回原样输出，不受渲染规则影响。
- 文本模式（plain/tsv/table）下单值结果裸输出不带标签：get/set/ttl/type/dbsize/del/hget 直接打印值，key 不存在输出空行；exec/eval 的标量返回（string/integer/double/boolean/null）同样裸输出，复合返回（array/map）输出缩进 JSON。JSON envelope 的 data 形状不变。

## 语法高亮

- 仅文本模式且颜色开启（TTY）时生效；非 TTY 强制无色，管道输出永远干净，`--json` 不染。
- get 与 exec/eval 的字符串标量返回：**自动检测 JSON**（首个非空白字符为 `{`/`[` 且 `json.Valid` 通过才染色，避免误判）；`--highlight json|yaml|toml`（别名 `--hl`）显式指定（yaml/toml 无可靠特征，不自动检测），`--highlight none` 关闭。
- exec/eval 的复合返回（array/map）以缩进 JSON 输出，恒按 JSON 高亮。
- 实现：chroma（纯 Go lexer，非 parser），formatter 随终端色彩能力（truecolor/256/8）自适应，样式按终端背景明暗选 github/github-dark；先按 `--max-bytes` 截断再高亮。

## 错误映射

| 场景 | 错误码 | 退出码 |
|---|---|---|
| NOAUTH / WRONGPASS / NOPERM | `AUTH_FAILED` | 4 |
| dial 失败、连接拒绝 | `CONNECT_FAILED` | 3 |
| 超时（context deadline） | `TIMEOUT` | 3 |
| 服务端 READONLY（replica 写入等） | `READONLY_VIOLATION` | 5 |
| 其余 Redis 错误（WRONGTYPE 等） | `QUERY_ERROR` | 5 |

## 已知限制

- 仅 standalone；Cluster / Sentinel / Pub-Sub / Monitor 不在第一版范围。
- 拦截是"防误操作"而非安全边界：Lua 内 `redis.call(...)` 可绕过 connector 侧拦截，强制约束需服务端 ACL（如 `+@read` 或禁用 script 命令类）。
- `keys` 用 SCAN 实现，大库上遍历耗时长，受 `--limit` 与 `--timeout` 约束。
- `hgetall`/`smembers`/`zrange`/`config get` 为全量拉取后按 `--limit` 截断展示，`--limit` 不限制传输量，大集合上注意；`keys` 不受此限（SCAN 逐批拉取）。
- 表格类命令的 data 形状为 `{columns, rows}`（列均为 string，无 `row_count`/`rows_affected`），与 sqlite query 的 data 形状不同。
- TLS 仅开关，不支持自定义 CA / 客户端证书（后续迭代）。
