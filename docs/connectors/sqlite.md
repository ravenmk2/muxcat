# sqlite connector

SQLite 是 muxcat 的首个 connector，也是后续 connector 设计文档的范例格式。

## 配置模型（sqlite.json）

```json
{
  "version": 1,
  "instances": {
    "local": { "path": "./test.db" }
  },
  "connections": {
    "local": { "instance": "local", "readonly": false, "timeout": "5s" }
  },
  "defaultConnection": "local"
}
```

- `instances`：名字 → `{path}`。path 支持 `~` 展开。sqlite 无密码字段。
- `connections`：名字 → `{instance, readonly?, timeout?}`；`timeout` 为 Go duration 字符串（如 `5s`），覆盖全局 `--timeout`。
- `defaultConnection`：缺省连接名；`-c/--conn` 未指定时使用，两者皆无报 `CONN_NOT_FOUND`。
- `conn add <name>` 同名创建 instance 与 connection（instance 名 = 连接名）；`conn rm` 删除连接时，同名 instance 无其他引用则一并删除。

## 命令

| 命令 | 说明 |
|---|---|
| `sqlite conn add <name> --path <file> [--readonly] [--set-default]` | 新增连接。非 TTY 缺 `--path` 报 `MISSING_ARGUMENT`；TTY 缺参走 huh 表单补全（路径 + 只读确认）。无默认连接时自动设为默认 |
| `sqlite conn ls` | 列出连接（name / path / readonly / default 标记） |
| `sqlite conn show <name>` | 连接详情 |
| `sqlite conn rm <name> [--yes]` | 删除连接；非 TTY 必须 `--yes`，TTY 弹确认 |
| `sqlite conn default <name>` | 设为默认连接 |
| `sqlite conn test <name>` | 打开 + `SELECT 1`，返回延迟（`latency_ms` 与 meta.elapsed_ms） |
| `sqlite query [-c NAME] "SQL" [--limit N]` | 执行 SQL |
| `sqlite tables [-c NAME]` | 列出 `sqlite_master` 中的表与视图（排除 `sqlite_%` 内部对象） |
| `sqlite schema [-c NAME] [table]` | 无参输出全库 DDL，有参输出单表 DDL；对象不存在报 `QUERY_ERROR` |

### query 的 data 形状

```json
{
  "columns": [{ "name": "id", "type": "INTEGER" }],
  "rows": [[1, "a"]],
  "row_count": 1,
  "rows_affected": null
}
```

- 语句按首关键词分流：`SELECT/PRAGMA/WITH/EXPLAIN/VALUES/TABLE` 走查询路径返回结果集；其余走 Exec 路径返回 `rows_affected`（columns/rows 为空）。注意 `INSERT ... RETURNING` 会被分入 Exec 路径（已知限制）。
- 结果行数超过 `--limit`（默认 1000）时截断并置 `meta.truncated: true`（多读一行判断）；`--limit 0` 表示不截断。
- envelope 的 `meta.connector` 恒为 `sqlite`，`meta.connection` 为实际连接名。
- BLOB 列按字符串输出。

## readonly 实现

readonly 连接在 DSN 上附加 `mode=ro`，由 SQLite 自身拒绝写操作；connector 把错误信息含 `readonly` / `read-only` 的失败归类为 `READONLY_VIOLATION`（退出码 5），hint 指明连接为只读。

## 双驱动切换

- `driver_cgo.go`（`//go:build cgo`）：`github.com/mattn/go-sqlite3`，driverName `sqlite3`。
- `driver_nocgo.go`（`//go:build !cgo`）：`modernc.org/sqlite`，driverName `sqlite`。
- 业务代码统一 `sql.Open(driverName, dsn)`，对驱动无感知；CI 与 release 固定 `CGO_ENABLED=0`（纯 Go 路径），CGO 路径由本地开发覆盖。
- DSN 只用两驱动的 `_pragma=` 公共交集：
  `file:<path>?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)`，readonly 追加 `&mode=ro`。`:memory:` 原样传递（测试用）。

## 已知限制

- 查询/Exec 分流靠首关键词启发式，`INSERT ... RETURNING`、CTE 写语句等不走结果集路径。
- DSN 中路径未做 URL 转义，含 `?`、`#` 等特殊字符的文件名不受支持。
- 单连接串行执行；未暴露事务与多语句脚本接口。
- BLOB 列以字符串渲染，不可读字节会原样输出。
