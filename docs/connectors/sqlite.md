# sqlite connector

SQLite 是 muxcat 的首个 connector，也是后续 connector 设计文档的范例格式。

## 配置模型（sqlite.json）

```json
{
  "version": 1,
  "instances": {
    "local": { "path": "./test.db", "ensureDb": true }
  },
  "connections": {
    "local": { "instance": "local", "readonly": false, "timeout": "5s" }
  },
  "defaultConnection": "local"
}
```

- `instances`：名字 → `{path, ensureDb?}`。path 支持 `~` 展开。sqlite 无密码字段。
  `ensureDb` 为 true 时允许首次使用时创建不存在的数据库文件（见「隐式建库防护」）。
- `connections`：名字 → `{instance, readonly?, timeout?}`；`timeout` 为 Go duration 字符串（如 `5s`），覆盖全局 `--timeout`。
- `defaultConnection`：缺省连接名；`-c/--conn` 未指定时使用，两者皆无报 `CONN_NOT_FOUND`。
- `conn add <name>` 同名创建 instance 与 connection（instance 名 = 连接名）；`conn rm` 删除连接时，同名 instance 无其他引用则一并删除；删除默认连接后按名字排序取第一个为新默认。

## 命令

| 命令 | 说明 |
|---|---|
| `sqlite conn add <name> --path <file> [--readonly] [--timeout 5s] [--ensure-db] [--set-default]` | 新增连接。非 TTY 缺 `--path` 报 `MISSING_ARGUMENT`；TTY 缺参走 huh 表单补全（路径 + 只读确认）。无默认连接时自动设为默认。`--timeout` 校验为合法 duration 且 > 0，非法报 `CONFIG_INVALID` |
| `sqlite conn ls` | 列出连接（name / path / readonly / default 标记）；instance 引用断裂时 path 显示 `<broken instance: NAME>` |
| `sqlite conn show <name>` | 连接详情（含 `ensureDb`） |
| `sqlite conn rm <name> [--yes]` | 删除连接；非 TTY 必须 `--yes`，TTY 弹确认 |
| `sqlite conn default <name>` | 设为默认连接 |
| `sqlite conn test <name>` | 打开 + `SELECT 1`，返回延迟（`latency_ms` 与 meta.elapsed_ms） |
| `sqlite query [-c NAME] "SQL" [--limit N]` | 执行 SQL |
| `sqlite tables [-c NAME]` | 列出 `sqlite_master` 中的表与视图（排除 `sqlite_%` 内部对象） |
| `sqlite schema [-c NAME] [table]` | 无参输出全库 DDL，有参输出单表 DDL；对象不存在报 `QUERY_ERROR` |
| `sqlite describe [-c NAME] <table>` | 表/视图的列定义（`pragma_table_info`，参数绑定），输出 `cid, name, type, notnull, default, pk`；对象不存在报 `QUERY_ERROR` |
| `sqlite status [-c NAME]` | 数据库状态：version、journal_mode、page_size/page_count、freelist_count、size_bytes（= page_size × page_count）、file_size_bytes、wal（`<file>-wal` 是否存在）；`:memory:` 连接省略文件项。table 两列固定顺序，JSON 为 `{"metrics": {...}}` |

### query 的 data 形状

```json
{
  "columns": [{ "name": "id", "type": "INTEGER" }],
  "rows": [[1, "a"]],
  "row_count": 1,
  "rows_affected": 0
}
```

- 语句分流：跳过前导空白、括号与 `--` / `/* */` 注释后取首关键词（`firstKeyword`，与 mysql guard 同款词法）。首词为 `SELECT/PRAGMA/WITH/EXPLAIN/VALUES/TABLE` 走查询路径返回结果集；`INSERT/UPDATE/DELETE` 含 `RETURNING` 子句也走查询路径；其余走 Exec 路径返回 `rows_affected`（columns/rows 为空）。
- 结果行数超过 `--limit`（默认 1000）时截断并置 `meta.truncated: true`（多读一行判断）；`--limit 0` 表示不截断。
- envelope 的 `meta.connector` 恒为 `sqlite`，`meta.connection` 为实际连接名。
- 值呈现与 mysql connector 一致（见 docs/connectors/mysql.md "值呈现规则"），由 sqlite connector 自行声明的同款 `CellStyle` 实现（独立实例，可分别演化）：`NULL` 渲染为 `NULL`（与空字符串可区分）；BLOB 列保留原始字节，文本模式渲染为 `0x` 大写 hex，`--json` 输出 `0x` hex 字符串。

## readonly 实现

双层防护（与 mysql/postgres 同一定位：客户端 guard 是防手滑层，服务端约束是硬边界）：

1. 客户端 guard：readonly 连接在打开文件前用首关键词白名单（`SELECT/PRAGMA/WITH/EXPLAIN/VALUES/TABLE`）拦截写语句，直接报 `READONLY_VIOLATION`（退出码 5），hint 指向改用可写连接。
2. 服务端约束：readonly 连接在 DSN 上附加 `mode=ro`，由 SQLite 自身拒绝写操作；guard 放行的可写变体（如写 PRAGMA、CTE 写语句）在此层被拒，错误信息含 `readonly` / `read-only` 的失败同样归类为 `READONLY_VIOLATION`。

## 隐式建库防护（ensureDb）

SQLite 原生行为是打开不存在的文件时静默创建空库，路径打错字就会留下一个意外的新文件。muxcat 默认禁止：

- `conn add`：path 非 `:memory:` 且文件不存在时，未带 `--ensure-db` 报 `CONFIG_INVALID`（hint: "pass --ensure-db to create it on first use"）。
- 打开时：非 readonly 连接 + `ensureDb` 未设置 + 文件不存在 → `CONNECT_FAILED`，杜绝手改配置或文件被删后的静默重建。
- readonly 连接靠 `mode=ro` 天然报错，不受此开关影响；`:memory:` 始终豁免。

## 双驱动切换

- `driver_cgo.go`（`//go:build cgo`）：`github.com/mattn/go-sqlite3`，driverName `sqlite3`。
- `driver_nocgo.go`（`//go:build !cgo`）：`modernc.org/sqlite`，driverName `sqlite`。
- 业务代码统一 `sql.Open(driverName, dsn)`，对驱动无感知；CI 与 release 固定 `CGO_ENABLED=0`（纯 Go 路径），CGO 路径由本地开发覆盖。
- DSN 只用两驱动的 `_pragma=` 公共交集：
  `file:<path>?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)`，readonly 追加 `&mode=ro`。`:memory:` 原样传递（测试用）。
- 路径拼入 DSN 前转义 `%`、`?`、`#`，含特殊字符的文件名可用。

## 已知限制

- 分流靠启发式：字符串字面量内含单词 `returning` 的写语句会被误判走查询路径（语句仍执行，`rows_affected` 报 0）；`WITH ... INSERT/DELETE` 等 CTE 写语句走查询路径。
- 单连接串行执行；未暴露事务与多语句脚本接口。
