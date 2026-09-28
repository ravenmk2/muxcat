# postgres connector

PostgreSQL connector（PG 12+，已在 16 / 17 / 18 实测），基于纯 Go 驱动 `github.com/jackc/pgx/v5` 的 stdlib 模式（`database/sql` 标准接口，driver 名 `pgx`），CI 与 release 的 `CGO_ENABLED=0` 基线不受影响。

## 配置模型（postgres.json）

```json
{
  "version": 1,
  "instances": {
    "local": { "host": "127.0.0.1", "port": 5432, "tls": false }
  },
  "connections": {
    "local": {
      "instance": "local",
      "username": "postgres",
      "password": "enc:v1:...",
      "database": "app",
      "readonly": false,
      "timeout": "5s"
    }
  },
  "defaultConnection": "local"
}
```

- `instances`：名字 → `{host, port, tls?}`，纯端点属性；`tls: true` 要求 TLS 加密连接（`sslmode=require`，加密但不校验 CA，libpq 语义，与 mysql 的 `tls=true` 对齐）。
- `connections`：名字 → `{instance, username?, password?, database?, readonly?, timeout?}`；`password` 以 `enc:v1:` 加密 blob 落盘、永不在输出中回显；`database` 为空时回退到 `postgres` 库；`timeout` 为 Go duration 字符串（如 `5s`），覆盖全局 `--timeout`。
- `defaultConnection`：缺省连接名；`-c/--conn` 未指定时使用，两者皆无报 `CONN_NOT_FOUND`。
- `conn add <name>` 同名创建 instance 与 connection（instance 名 = 连接名）；`conn rm` 删除连接时，同名 instance 无其他引用则一并删除。

## 命令

| 命令 | 说明 |
|---|---|
| `postgres conn add <name> --host <host> [--port 5432] [--username U] [--password P] [--database DB] [--tls] [--readonly] [--timeout 5s] [--set-default]` | 新增连接。非 TTY 缺 `--host` 报 `MISSING_ARGUMENT`；`--host` 为空且是 TTY 时走 huh 表单补全（其余字段只补全未显式给出的）。`--password` 明文传参会打 stderr 警告后以 `enc:v1:` 加密落盘。无默认连接时自动设为默认 |
| `postgres conn ls` | 列出连接（name / addr / user / database / tls / readonly / default 标记），不泄露密码 blob |
| `postgres conn show <name>` | 连接详情（不回显密码） |
| `postgres conn rm <name> [--yes]` | 删除连接；非 TTY 必须 `--yes`，TTY 弹确认 |
| `postgres conn default <name>` | 设为默认连接 |
| `postgres conn test <name>` | PING + `SELECT version()`，返回 `{ok, latency_ms, version}` |
| `postgres query ["SQL"] [--db 库] [--file 路径] [--limit N]` | 执行 SQL |
| `postgres execute ["SQL"] [--db 库] [--file 路径]` | 显式执行：任意语句无条件走 Exec，只报 `rows_affected`（结果集丢弃）；readonly 连接一律拒绝 |
| `postgres databases` | 列出数据库（`pg_database`，排除 template），列 name/owner/encoding/size |
| `postgres schemas` | 列出用户 schema（`pg_namespace`，排除 `pg_*` 与 `information_schema`） |
| `postgres tables [--schema 名]` | 列出全部用户 schema 的表与视图（带 schema 列），`--schema` 过滤并去掉 schema 列；数据来自 `information_schema.tables` |
| `postgres schema [--schema 名] [table]` | 无参输出全部用户 BASE TABLE 的列定义汇总；有参输出单表 \d+ 风格结构化描述（列 + 索引 + 约束）；表不存在报 `QUERY_ERROR` |
| `postgres status [--all]` | 精选状态指标；`--all` 全量 `pg_stat_database` |
| `postgres settings [pattern]` | 服务器设置（`pg_settings`）；pattern 为大小写不敏感子串过滤（非 LIKE 通配） |
| `postgres activity` | `pg_stat_activity` 实时会话 |
| `postgres kill <pid> [--cancel] [--yes]` | 终止后端（默认 `pg_terminate_backend`；`--cancel` 改 `pg_cancel_backend`）；readonly 连接拒绝；非 TTY 必须 `--yes` |
| `postgres roles` | 列出角色（`pg_roles`） |
| `postgres grants [role]` | 表级授权（`information_schema.role_table_grants`）；无参为 current_user |
| `postgres extensions` | 已安装扩展（`pg_extension` JOIN `pg_namespace`） |
| `postgres locks` | 当前锁与阻塞者（`pg_locks` LEFT JOIN `pg_stat_activity`） |
| `postgres replication` | 复制状态：主库出 `pg_stat_replication`，备库出 `pg_stat_wal_receiver` 精选字段 |

### query 的 SQL 输入来源

优先级（互斥校验）：

1. 位置参数 `query "SELECT 1"`
2. `--file <路径>` 从文件读取；`--file -` 显式从 stdin 读取
3. 无参数且无 `--file` 时：stdin 非 TTY（管道/重定向）则读 stdin，TTY 报 `MISSING_ARGUMENT`（hint 给出三种用法）
4. 位置参数与 `--file` 同时出现 → `MISSING_ARGUMENT`

文件/stdin 读到的文本与内联 SQL 走同一条分流路径。**单语句限制**：每次调用只执行一条语句，多语句脚本由服务端报错并归类 `QUERY_ERROR`。

### query 的 data 形状

```json
{
  "columns": [{ "name": "id", "type": "INT4" }],
  "rows": [[1, "a"]],
  "row_count": 1,
  "rows_affected": 0
}
```

- 语句按首关键词分流（大小写不敏感，跳过前导空白、`(`、`--`/`/* */` 注释）：`SELECT/WITH/TABLE/VALUES/EXPLAIN/SHOW` 走查询路径返回结果集；其余走 Exec 路径返回 `rows_affected`（columns/rows 为空）。注意 `WITH ... UPDATE/DELETE` 等可写 CTE 会按首关键词 `WITH` 被分入查询路径（已知限制）。
- 结果行数超过 `--limit`（默认 1000）时截断并置 `meta.truncated: true`（多读一行判断）；`--limit 0` 表示不截断。
- envelope 的 `meta.connector` 恒为 `postgres`，`meta.connection` 为实际连接名。
- `type` 取 `ColumnTypes().DatabaseTypeName()`（pgx stdlib 返回大写型名：`INT4`、`NUMERIC`、`TIMESTAMPTZ`、`BYTEA`、数组为 `_TEXT` 形式）；`NULL` 输出为 null；bytea 列输出 `0x` 大写 hex 字符串（见下"值呈现规则"）。
- `--db` 为临时换库：PG 不支持会话内 `USE`，实现为用另一个 dbname 重新建立连接（不写回配置），仅 `query`/`execute` 支持。

## 值呈现规则（文本模式）

以下呈现由 postgres connector 通过 `Result.CellStyle` 自行声明（opt-in），类别与配色与 mysql connector 契约一致（NULL 暗灰斜体、数字青、字符串绿、时间黄、二进制品红）。

**pgx stdlib 经 `database/sql` 扫描到 `*any` 的实测 Go 类型**（PG 16/17/18 一致）：

| PG 类型 | 实测 Go 类型 | 文本模式 | `--json` |
|---|---|---|---|
| NULL | `nil` | `NULL`（暗灰斜体） | `null` |
| INT2/INT4/INT8/OID | `int64` | 原样数字（青） | number |
| FLOAT4/FLOAT8 | `float64` | 原样数字（青） | number |
| NUMERIC | `[]byte` → string | 精度无损文本（青） | string |
| BOOL | `bool` | `true`/`false` | bool |
| TEXT/VARCHAR/BPCHAR/NAME | `[]byte` → string | 原文（绿） | string |
| UUID / JSON / JSONB | `[]byte` → string | 原文（JSONB 为服务端规范化文本） | string |
| 数组（`_TEXT` 等） | `[]byte` → string | PG 数组字面量 `{a,b,c}` | string |
| TIMESTAMP | `time.Time`（UTC 标记） | `2024-02-29 12:34:56.789`（黄） | RFC3339（`...Z`） |
| TIMESTAMPTZ | `time.Time`（客户端本地时区） | `2024-02-29 15:04:56.789 +08:00`（保留时区偏移，黄） | RFC3339（带偏移） |
| DATE | `time.Time` | `2024-02-29`（黄） | RFC3339 |
| BYTEA | `[]byte`（保留原始字节） | `0xDEADBEEF`（大写 hex，品红） | `0x` hex string |

- 时区说明：TIMESTAMPTZ 由 pgx 转为客户端进程本地时区的 `time.Time`，文本布局含时区偏移（`-07:00` 形式），JSON 为 RFC3339；TIMESTAMP（无时区）不追加偏移。
- 二进制恒为 hex（不提供开关）：bytea 经 `output.IsBinaryType("BYTEA")` 识别，文本渲染为 `0x` 大写 hex，JSON 同样为 hex 字符串（不出现 base64），与 mysql 的 `--binary-as-hex` 语义一致。
- 彩色着色作用于 table 与 plain 两种文本模式；tsv/json 永不着色。
- table 模式在表格总宽超过终端宽度时从最宽列开始压缩单元格；plain/tsv/json 一律不截断。

## execute 与 query 的分工

- `query` 取数优先：按首关键词自动分流，SELECT 类返回结果集，其余报 `rows_affected`——自动分流是便利。
- `execute` 显式执行：任意语句（含 SELECT）无条件走 `ExecContext`，只报 `rows_affected`，结果集丢弃。输入通道与 query 完全一致（位置参数 / `--file` / `--file -` / stdin 管道，互斥校验相同），支持 `--db` 临时换库。
- readonly 行为不同：query 过首关键字白名单守卫（只拦写）；execute 一律拒绝（含 SELECT），错误同为 `READONLY_VIOLATION` 且拨号前发生。

## 运维命令

面向实例级元信息的十三个命令，通用约定：

- 均**不支持 `--db`**（与当前库无关，保持 flag 面干净）。
- 除 `kill` 外全部是只读命令，readonly 连接放行（不经过 query 的 SQL 文本守卫）。
- catalog 查询只用 PG 12-18 稳定存在的列（刻意避开 PG17 改名的 `pg_stat_bgwriter`），已在 16/17/18 三个大版本实测通过。

### databases

`pg_database`（排除 `datistemplate`），列 `name | owner | encoding | size`（`pg_database_size`，字节数）；JSON `{"databases": [{name, owner, encoding, size}]}`。

### schemas

`pg_namespace`，排除 `pg_*` 与 `information_schema`，列 `name | owner`；JSON `{"schemas": [{name, owner}]}`。

### status [--all]

- 默认精选（固定顺序）：`version, uptime_s, max_connections, connections, active, idle, xact_commit, xact_rollback, cache_hit_rate, deadlocks`。其中 `uptime_s` 取自 `pg_postmaster_start_time()`；连接计数按 `pg_stat_activity.state` 聚合；事务与缓存计数为 `pg_stat_database` 全库求和；`cache_hit_rate = blks_hit/(blks_hit+blks_read)`（百分比 1 位小数，无块访问时按 100%）。
- `--all`：`SELECT * FROM pg_stat_database ORDER BY datname` 全行全列（列集随版本动态读取，PG18 新增列自然兼容）；JSON `{"databases": [ {...} ]}`。
- JSON：精选 `{"metrics": {name: value}}`（数值为 number）。

### settings [pattern]

`SELECT name, setting, unit, source FROM pg_settings`；pattern 为**客户端**大小写不敏感子串过滤（不是 LIKE 通配）。列 `name | value | unit | source`（unit 可为 NULL）；JSON `{"settings": {name: value}}`。

### activity

`pg_stat_activity`，列 `pid | user | db | client | state | duration_s | wait | query`；后台进程的 user/db/state 为 NULL；`duration_s` 为 `now()-query_start` 秒数；`wait` 为 `wait_event_type.wait_event`；query 文本模式截断到 80 字符、JSON 保留全量。JSON `{"sessions": [...]}`。

### kill \<pid> [--cancel] [--yes]

- pid 校验：纯数字且 > 0，否则 `MISSING_ARGUMENT`。
- readonly 连接拨号前拒绝：`READONLY_VIOLATION`。
- 确认：TTY 先查 `pg_stat_activity` 展示目标摘要（pid 不存在报 `QUERY_ERROR` "no such backend pid"）再弹 huh 确认；非 TTY 必须 `--yes`。
- 默认 `pg_terminate_backend(pid)`（断开连接）；`--cancel` 改 `pg_cancel_backend(pid)`（中断当前查询、保留连接）。函数返回 false（pid 已消失）报 `QUERY_ERROR`。

### roles

`pg_roles`，列 `role | superuser | createdb | createrole | login | replication | conn_limit`；JSON `{"roles": [...]}`。

### grants [role]

- 无参默认 `current_user`；角色名经 `$1` 参数绑定（不拼接）。
- `information_schema.role_table_grants`，列 `schema | table | privilege | grantable`；JSON `{"role": ..., "grants": [...]}`。

### extensions

`pg_extension JOIN pg_namespace`，列 `name | version | schema`（当前数据库已安装的扩展）；JSON `{"extensions": [...]}`。

### locks

`pg_locks LEFT JOIN pg_stat_activity LEFT JOIN pg_class`，列 `pid | locktype | relation | mode | granted | blocking_pids | user | query`。`pg_locks` 是集群级视图：relation 经 `pg_class` 解析为表名，其他数据库的关系（当前库无法解析）回退显示 OID 文本；`pid` 可空（prepared transaction 持有的锁为 NULL）。`blocking_pids` 为 `pg_blocking_pids(pid)` 的数组文本（无阻塞者显示 `{}`）；query 文本模式截断、JSON 保留。未授予的锁（`granted=false`）排在前面。

### replication

- 先 `SELECT pg_is_in_recovery()` 判断角色。
- 主库（false）：`pg_stat_replication` 行，列 `pid | user | client_addr | state | sent_lsn | write_lsn | flush_lsn | replay_lsn | sync_state`；空结果 → `Message: "no replicas"` + JSON `{"replicas": []}`（ok:true）。
- 备库（true）：`pg_stat_wal_receiver` 精选字段 `status | sender_host | sender_port | received_lsn | latest_end_lsn | last_msg_send_time | last_msg_receipt_time`（PG13+ 的 `written_lsn` 以 `received_lsn` 输出名呈现）；无 wal receiver 行 → `"no wal receiver"` + JSON `{"receiver": null}`。
- 安全：`pg_stat_wal_receiver.conninfo` 可能携带凭据，**永不查询该列**。

## readonly 双保险

1. **客户端守卫（拨号前）**：readonly 连接的 SQL 先过首关键字白名单 `SELECT/WITH/TABLE/VALUES/EXPLAIN/SHOW`（跳过前导空白、`(`、`--`/`/* */` 注释），其余一律在拨号前拦截，报 `READONLY_VIOLATION`（退出码 5）。**白名单不含 `SET`**——防止 `SET default_transaction_read_only = off` 自我解除。守卫是防误操作措施，不是安全边界。
2. **服务端强制（连接配置层）**：readonly 连接的驱动配置携带 runtime param `default_transaction_read_only=on`（pgx `RuntimeParams`），服务端对连接池的**每条物理连接**在启动时应用，天然免疫 `database/sql` 连接池与 `SET SESSION` 的错配问题（mysql 的 SET SESSION 方案在池化下只对单条连接生效）。服务端兜底错误 SQLSTATE 25006 归类为 `READONLY_VIOLATION`。

## 错误码映射

| 条件 | 错误码 |
|---|---|
| `*pgconn.PgError` 28P01 / 28000 | `AUTH_FAILED` |
| `*pgconn.PgError` 25006 | `READONLY_VIOLATION` |
| 其他 `*pgconn.PgError` | `QUERY_ERROR` |
| `context.DeadlineExceeded` / `net.Error.Timeout()` | `TIMEOUT` |
| dial tcp / connection refused / no such host | `CONNECT_FAILED` |

连接错误文本（`pgconn.ConnectError`）只携带 host/user/database，**不含密码**（密码只经结构化字段传递，从不拼入连接串），可安全出现在错误消息中。

## 连接配置构建

程序化配置而非手工拼 DSN：`pgx.ParseConfig("sslmode=require|disable")`（仅常量 token 走连接串解析，获得 libpq 语义的 TLS 行为），随后 Host/Port/User/Database/Password/RuntimeParams 全部结构化赋值——密码含任意特殊字符也无需转义，明文只存在于内存。`stdlib.OpenDB(*cfg)` 打开 `database/sql` 句柄，`PingContext` 验证后即用。`--db` 覆盖与 `database` 缺省回退 `postgres` 均在配置层完成。

## 示例

```sh
muxcat postgres conn add prod --host db.internal --username app --password s3cret --database shop --set-default
muxcat postgres conn test prod
muxcat postgres query "SELECT id, email FROM users LIMIT 5"
muxcat postgres query --file report.sql --json
echo "SELECT * FROM pg_stat_activity" | muxcat postgres query --db analytics
muxcat postgres tables --schema public
muxcat postgres schema users
muxcat postgres conn add ro --host db.internal --username app --database shop --readonly
muxcat postgres query -c ro "SELECT 1"              # 放行
muxcat postgres query -c ro "DROP TABLE users"      # READONLY_VIOLATION（拨号前拦截）
```

## 已知限制

- 查询/Exec 分流靠首关键词启发式，`WITH ... UPDATE/DELETE` 等可写 CTE 会被分入查询路径。
- 单语句限制；无事务接口。`COPY ... FROM STDIN` 走 Exec 会因无数据流而报错（归类 `QUERY_ERROR`）。
- 无参 `schema` 的列类型取自 `information_schema`（varchar/numeric 已拼回长度与精度修饰符）；单表 describe 用 `pg_catalog.format_type` 输出完整类型。
- 数组值以 PG 数组字面量文本呈现（`{a,b,c}`），不做元素级结构化。
- `databases` 的 size 列需要各库的 CONNECT 权限（或 `pg_read_all_stats` 成员），否则由服务端报错归类 `QUERY_ERROR`。
- 单连接串行执行；每次命令新建并关闭连接池。
