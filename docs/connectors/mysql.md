# mysql connector

MySQL connector（MySQL 5.7+ / 8.x），基于纯 Go 驱动 `github.com/go-sql-driver/mysql`，经 `database/sql` 标准接口访问，CI 与 release 的 `CGO_ENABLED=0` 基线不受影响。

## 配置模型（mysql.json）

```json
{
  "version": 1,
  "instances": {
    "local": { "host": "127.0.0.1", "port": 3306, "tls": false }
  },
  "connections": {
    "local": {
      "instance": "local",
      "username": "root",
      "password": "enc:v1:...",
      "database": "app",
      "readonly": false,
      "timeout": "5s"
    }
  },
  "defaultConnection": "local"
}
```

- `instances`：名字 → `{host, port, tls?}`，纯端点属性；`tls: true` 要求 TLS 加密连接。
- `connections`：名字 → `{instance, username?, password?, database?, readonly?, timeout?}`；`password` 以 `enc:v1:` 加密 blob 落盘、永不在输出中回显；`database` 为空表示不选默认库；`timeout` 为 Go duration 字符串（如 `5s`），覆盖全局 `--timeout`。
- `defaultConnection`：缺省连接名；`-c/--conn` 未指定时使用，两者皆无报 `CONN_NOT_FOUND`。
- `conn add <name>` 同名创建 instance 与 connection（instance 名 = 连接名）；`conn rm` 删除连接时，同名 instance 无其他引用则一并删除。

## 命令

| 命令 | 说明 |
|---|---|
| `mysql conn add <name> --host <host> [--port 3306] [--username U] [--password P] [--database DB] [--tls] [--readonly] [--timeout 5s] [--set-default]` | 新增连接。非 TTY 缺 `--host` 报 `MISSING_ARGUMENT`；`--host` 为空且是 TTY 时走 huh 表单补全（其余字段只补全未显式给出的）。`--password` 明文传参会打 stderr 警告后以 `enc:v1:` 加密落盘。无默认连接时自动设为默认 |
| `mysql conn ls` | 列出连接（name / addr / user / database / tls / readonly / default 标记），不泄露密码 blob |
| `mysql conn show <name>` | 连接详情（不回显密码） |
| `mysql conn rm <name> [--yes]` | 删除连接；非 TTY 必须 `--yes`，TTY 弹确认 |
| `mysql conn default <name>` | 设为默认连接 |
| `mysql conn test <name>` | PING + `SELECT VERSION()`，返回 `{ok, latency_ms, version}` |
| `mysql query ["SQL"] [--db 库] [--file 路径] [--limit N]` | 执行 SQL |
| `mysql tables [--db 库]` | 列出当前库的表与视图（`information_schema.TABLES`） |
| `mysql schema [--db 库] [table]` | 无参输出所有 BASE TABLE 的 DDL，有参输出单表 DDL（`SHOW CREATE TABLE`）；表不存在报 `QUERY_ERROR` |

### query 的 SQL 输入来源

优先级（互斥校验）：

1. 位置参数 `query "SELECT 1"`
2. `--file <路径>` 从文件读取；`--file -` 显式从 stdin 读取
3. 无参数且无 `--file` 时：stdin 非 TTY（管道/重定向）则读 stdin，TTY 报 `MISSING_ARGUMENT`（hint 给出三种用法）
4. 位置参数与 `--file` 同时出现 → `MISSING_ARGUMENT`

文件/stdin 读到的文本与内联 SQL 走同一条分流路径。**单语句限制**：`multiStatements` 未启用，多语句脚本由服务端报错（通常是 1064 语法错误）并归类 `QUERY_ERROR`。

### query 的 data 形状

```json
{
  "columns": [{ "name": "id", "type": "INT" }],
  "rows": [[1, "a"]],
  "row_count": 1,
  "rows_affected": 0
}
```

- 语句按首关键词分流（大小写不敏感，跳过前导空白、`(`、`--`/`#`/`/* */` 注释）：`SELECT/SHOW/DESC/DESCRIBE/EXPLAIN/WITH/VALUES/TABLE` 走查询路径返回结果集；其余走 Exec 路径返回 `rows_affected`（columns/rows 为空）。注意 `WITH ... UPDATE/DELETE` 等可写 CTE 会按首关键词 `WITH` 被分入查询路径（已知限制）。
- 结果行数超过 `--limit`（默认 1000）时截断并置 `meta.truncated: true`（多读一行判断）；`--limit 0` 表示不截断。
- envelope 的 `meta.connector` 恒为 `mysql`，`meta.connection` 为实际连接名。
- `type` 取 `ColumnTypes().DatabaseTypeName()`；`NULL` 输出为 null；二进制列输出 `0x` 大写 hex 字符串（见下"值呈现规则"）。
- `--db` 在 DSN 层临时换库（不写回配置），对 `query`/`tables`/`schema` 均生效。

## 值呈现规则（文本模式，对齐官方 mysql cli）

以下呈现由 mysql connector 通过 `Result.CellStyle` 自行声明（opt-in）；未声明 CellStyle 的 connector 使用 legacy 默认（nil → 空串、无着色）。

| 数据类别 | 文本模式（无色彩） | 彩色模式（table / plain，颜色开启时） | `--json` |
|---|---|---|---|
| NULL | `NULL` | `NULL`，暗灰斜体（color 8 + italic） | `null` |
| 空字符串 | 空（与 NULL 天然可区分） | 空 | `""` |
| 整数/浮点 | 原样数字 | 青色（color 6） | number |
| DECIMAL | 文本（驱动 []byte → string，精度无损） | 青色 | string |
| 字符串（CHAR/VARCHAR/TEXT/ENUM/SET/JSON 列） | 原文 | 绿色（color 2） | string |
| 日期时间（DATE/DATETIME/TIMESTAMP） | `2024-02-29`、`2024-02-29 12:34:56.789`（去时区尾缀，含微秒时保留） | 黄色（color 3） | RFC3339（结构化契约，与 cli 文本格式刻意不同） |
| TIME/YEAR | 原文/数字（驱动给 string/int64） | 黄色/青色 | 保持 |
| 二进制（BINARY/VARBINARY/*BLOB/GEOMETRY/BIT） | `0xDEADBEEF`（大写 hex，对齐 `--binary-as-hex` 语义） | 品红（color 5） | `0x` hex string |

- 时区说明：text 协议下服务端按会话时区发送墙钟时间，驱动 `loc` 只影响 `time.Time` 的时区标签不改变数字，因此去掉尾缀后显示数字与官方 cli 一致。
- 二进制恒为 hex（不提供开关）：官方 cli 默认直出原始字节对管道不友好，hex 是其 `--binary-as-hex` 推荐语义；同时修复了非 UTF-8 二进制在 JSON 中变 `\ufffd` 的损坏问题。
- 彩色着色作用于 table 与 plain 两种文本模式（颜色开启时：TTY auto，或显式 `--output` 强制）；tsv/json 永不着色，面向管道与机器处理。
- table 模式在表格总宽超过终端宽度时从最宽列开始压缩单元格（ANSI 感知截断 + `…` 尾缀，列下限不小于列名宽度与 8）；纯展示层行为，不改数据，`meta.truncated` 仍只表示 `--limit` 行截断。plain/tsv/json 一律不截断。

## readonly 双保险

1. **客户端守卫（拨号前）**：readonly 连接的 SQL 先过首关键字白名单 `SELECT/SHOW/DESC/DESCRIBE/EXPLAIN/WITH/USE`（跳过前导空白、`(`、`--`/`#`/`/* */` 注释），其余一律在拨号前拦截，报 `READONLY_VIOLATION`（退出码 5）。**白名单不含 `SET`**——防止 `SET SESSION transaction_read_only=0` 自我解除。守卫是防误操作措施，不是安全边界。
2. **服务端强制（拨号后）**：连接建立后执行 `SET SESSION transaction_read_only = 1`（MySQL 5.7 / MariaDB 报 1193 时自动回退旧变量名 `tx_read_only`）；服务端兜底错误 1290/1792/1836 归类为 `READONLY_VIOLATION`。

## 错误码映射

| 条件 | 错误码 |
|---|---|
| `*mysql.MySQLError` 1045（Access denied） | `AUTH_FAILED` |
| `*mysql.MySQLError` 1290 / 1792 / 1836 | `READONLY_VIOLATION` |
| 其他 `*mysql.MySQLError` | `QUERY_ERROR` |
| `context.DeadlineExceeded` / `net.Error.Timeout()` | `TIMEOUT` |
| dial tcp / connection refused / no such host | `CONNECT_FAILED` |

## DSN 构建

`mysql.NewConfig()` + `FormatDSN()`：`Net=tcp`、`Addr=<host>:<port>`、`DBName=<database 或 --db 覆盖>`、`ParseTime=true`、`MultiStatements=false`；`tls: true` → `TLSConfig="true"`，否则 `"false"`；密码从 `enc:v1:` blob 解密后填入。驱动日志在 `init()` 中被静音（`mysql.SetLogger` + NopLogger），连接失败统一走结构化错误。

## 示例

```sh
muxcat mysql conn add prod --host db.internal --username app --password s3cret --database shop --set-default
muxcat mysql conn test prod
muxcat mysql query "SELECT id, email FROM users LIMIT 5"
muxcat mysql query --file report.sql --json
echo "SHOW TABLES" | muxcat mysql query --db analytics
muxcat mysql tables
muxcat mysql schema users
muxcat mysql conn add ro --host db.internal --username app --database shop --readonly
muxcat mysql query -c ro "SELECT 1"              # 放行
muxcat mysql query -c ro "DROP TABLE users"      # READONLY_VIOLATION（拨号前拦截）
```

## 已知限制

- 查询/Exec 分流靠首关键词启发式，`WITH ... UPDATE/DELETE` 等可写 CTE 会被分入查询路径。
- 未启用多语句脚本（`MultiStatements=false`）；无事务接口。
- 单连接串行执行；每次命令新建并关闭连接池。
