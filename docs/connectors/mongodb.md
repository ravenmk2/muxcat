# mongodb connector

MongoDB connector（MongoDB 6 / 7 / 8，优先覆盖 8），基于官方驱动 `go.mongodb.org/mongo-driver/v2`，纯 Go 实现，CI 与 release 的 `CGO_ENABLED=0` 基线不受影响。

Phase 3 覆盖连接管理、只读查询（`query` / `aggregate` / `dbs` / `collections`）与写入（`insert` / `update` / `delete` / `drop`），readonly 连接标记开始强制生效。

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
      "readonly": false,
      "timeout": "5s"
    }
  },
  "defaultConnection": "local"
}
```

- `instances`：名字 → `{hosts, tls?, replicaSet?}`，纯端点属性。`hosts` 为 `host:port` 数组，phase 1 只支持单条目；条目可省略端口，读取时默认补 27017。`tls: true` 要求 TLS 加密连接（最低 TLS 1.2）。`replicaSet` 保留字段，目前仅在 `--uri` 携带 `replicaSet` 参数时写入。
- `connections`：名字 → `{instance, username?, password?, authSource?, database?, readonly?, timeout?}`；`password` 以 `enc:v1:` 加密 blob 落盘、永不在输出中回显；`authSource` 为认证库，设置 username 而未指定 authSource 时默认 `admin`；`database` 为空表示不选默认库；`readonly` 为只读标记，phase 3 起强制：写命令在拨号前拒绝（见下「readonly 强制」）；`timeout` 为 Go duration 字符串（如 `5s`），覆盖全局 `--timeout`。
- `defaultConnection`：缺省连接名；`-c/--conn` 未指定时使用，两者皆无报 `CONN_NOT_FOUND`。
- `conn add <name>` 同名创建 instance 与 connection（instance 名 = 连接名）；`conn rm` 删除连接时，同名 instance 无其他引用则一并删除。

## 命令

| 命令 | 说明 |
|---|---|
| `mongodb conn add <name> --host <host> [--port 27017] [--username U] [--password P] [--auth-source DB] [--database DB] [--tls] [--readonly] [--timeout 5s] [--set-default] [--uri mongodb://...]` | 新增连接。非 TTY 缺 `--host`（且无 `--uri`）报 `MISSING_ARGUMENT`；两者皆空且是 TTY 时走 huh 表单补全（其余字段只补全未显式给出的）。`--password` 明文传参会打 stderr 警告后以 `enc:v1:` 加密落盘；`--uri` 内含明文密码同样告警。无默认连接时自动设为默认 |
| `mongodb conn ls` | 列出连接（name / addr / user / authSource / database / tls / readonly / default 标记），不泄露密码 blob |
| `mongodb conn show <name>` | 连接详情（不回显密码；`replicaSet` 仅在设置时展示） |
| `mongodb conn rm <name> [--yes]` | 删除连接；非 TTY 必须 `--yes`，TTY 弹确认；同名 instance 无其他引用时级联删除，默认连接被删后自动重选 |
| `mongodb conn default <name>` | 设为默认连接 |
| `mongodb conn test <name>` | ping + `buildInfo` + `hello`，返回 `{ok, latency_ms, version, topology, maxWireVersion}`；topology 取值 `standalone` / `replicaset` / `sharded` |
| `mongodb query <collection> [filter] [--db 库] [--projection DOC] [--sort field:asc\|desc] [--skip N] [--file 路径]` | find 查询；filter 为 Extended JSON 文档，可省略（匹配全部）；结果按 `--limit` 截断（`meta.truncated` 标记） |
| `mongodb aggregate <collection> [pipeline] [--db 库] [--file 路径]` | 只读聚合；pipeline 为 Extended JSON stage 数组（必填）；`$out`/`$merge` 写 stage 一律拒绝（`READONLY_VIOLATION`） |
| `mongodb dbs` | 列出数据库（name / sizeOnDisk / empty），`--json` 输出原始文档 |
| `mongodb collections [--db 库]` | 列出当前库的集合（name / type） |
| `mongodb insert <collection> [doc] [--db 库] [--file 路径] [--jsonl]` | 插入文档：单文档 → InsertOne；Extended JSON 数组 → InsertMany；`--jsonl` 按行批量（mongoimport 惯例）。集合不存在时隐式创建 |
| `mongodb update <collection> <filter> <update> [--db 库] [--many] [--upsert]` | 更新文档：默认 UpdateOne，`--many` 换 UpdateMany（mongosh 语义），`--upsert` 无匹配时插入。update 为操作符文档（`{"$set":...}`）或替换文档，空文档报 `CONFIG_INVALID` |
| `mongodb delete <collection> [filter] [--db 库] [--many] [--file 路径] [--yes]` | 删除文档：默认 DeleteOne，`--many` 换 DeleteMany。空 filter + `--many`（清空集合）需要确认 |
| `mongodb drop <collection> [--db 库] [--yes]` | 删除集合（不可逆，一律确认）；集合不存在报 `QUERY_ERROR` |

### 写命令

- **insert 输入三种形态**：单文档 `{"a":1}`、Extended JSON 数组 `[{...},{...}]`（每个元素必须是文档）、`--jsonl` 行分隔批量（空行跳过，坏行报 `CONFIG_INVALID` 并带行号；零文档报 `MISSING_ARGUMENT`）。输出 `{insertedCount, insertedIds}`（文本模式 ObjectID 渲染为 hex，--json 为 Extended JSON 形态）。
- **update/delete 默认单文档**：对齐 mongosh 语义，`--many` 才批量。
- **确认矩阵**：仅高危操作要确认——`delete` 空 filter + `--many`（清空集合）与 `drop`（不可逆）。非 TTY 必须 `--yes`，否则 `MISSING_ARGUMENT`；TTY 弹 huh 确认，拒绝则 `GENERAL cancelled`。普通写入不需要确认（readonly 是防线）。
- 无 create-collection 命令：insert 隐式创建集合；索引管理留待后续阶段。

### readonly 强制

readonly 连接的拦截点在**拨号前**（`resolveTarget` 之后）：`insert` / `update` / `delete` / `drop` 一律拒绝，报 `READONLY_VIOLATION`（退出码 5），hint 提示换可写连接（-c）。读命令（query / aggregate / dbs / collections）不受影响；aggregate 的 `$out`/`$merge` 护栏对**所有**连接生效。护栏是防误操作措施，不是安全边界。

### 查询输入（filter / pipeline）

输入来源优先级（与 mysql query 一致）：

1. 位置参数 `query users '{"age":{"$gte":18}}'`
2. `--file <路径>` 从文件读取；`--file -` 显式从 stdin 读取
3. 无参数且无 `--file` 时：stdin 非 TTY（管道/重定向）则读 stdin，TTY 报 `MISSING_ARGUMENT`（aggregate 的 pipeline 必填；query 的 filter 可省略为匹配全部）
4. 位置参数与 `--file` 同时出现 → `CONFIG_INVALID`

输入一律按 **Extended JSON** 解析（`bson.UnmarshalExtJSON`，relaxed 模式）：`{"_id":{"$oid":"..."}}`、`{"ts":{"$date":"..."}}` 等 BSON 类型写法均可。filter/projection 解析为保序文档（`bson.D`），pipeline 解析为 `bson.A` 且每个 stage 必须是文档；解析失败报 `CONFIG_INVALID`。

目标库由 `resolveDB` 决定：`--db` > 连接的 `database` 字段 > 报 `MISSING_ARGUMENT`（hint 提示传 `--db` 或给连接设置默认库）。

### 文档渲染

- 文本模式：首列 `_id`（仅当所有文档都含 `_id`），随后是顶层字段的并集（按首次出现顺序，封顶 15 列；超出的字段数在 Message 中提示 `+N more fields, use --json`）。标量直接渲染（string / int32 / int64 / float64 / bool / nil→`null` / DateTime→RFC3339 / ObjectID→hex）；其余（子文档、数组、Decimal128、Binary 等）渲染为紧凑 relaxed Extended JSON 字符串；文档缺失的字段显示为空单元格。
- `--json`：每篇文档经 relaxed Extended JSON 往返转换为标准 JSON（ObjectID 为 `{"$oid":"..."}` 等 Extended JSON 形态），envelope `data` 为文档数组，`meta.truncated` 标记截断。
- `--limit`（默认 1000，0 = 不限）：query 服务端 `SetLimit(n+1)` 探测截断；aggregate 游标侧多读一篇判断。截断时渲染前 n 篇并置 `meta.truncated: true`。

### aggregate 护栏

aggregate 是只读命令：pipeline 中任何 stage 的顶层 key 为 `$out` 或 `$merge` 时在拨号前拒绝，报 `READONLY_VIOLATION`（退出码 5），hint 说明写 stage 被拒绝。护栏是防误操作措施，不是安全边界。

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
| aggregate pipeline 含 `$out` / `$merge` stage（拨号前拒绝） | `READONLY_VIOLATION` |
| readonly 连接执行 insert / update / delete / drop（拨号前拒绝） | `READONLY_VIOLATION` |
| filter / pipeline / projection / sort / 插入文档的 Extended JSON 非法 | `CONFIG_INVALID` |
| 未指定库（无 `--db` 且连接无默认库） | `MISSING_ARGUMENT` |
| 清空集合 / drop 未经确认（非 TTY 缺 `--yes`） | `MISSING_ARGUMENT` |
| CommandError 11000（duplicate key） | `QUERY_ERROR` |
| drop 不存在的集合（响应无 `ns` 字段或服务端返回 NamespaceNotFound/26） | `QUERY_ERROR` |

## 示例

```sh
muxcat mongodb conn add local --host 127.0.0.1 --set-default
muxcat mongodb conn add prod --host db.example.com --username app --password s3cret --auth-source admin --database shop --tls --set-default
muxcat mongodb conn add rs --uri "mongodb://app:s3cret@db.example.com:27018/shop?replicaSet=rs0&tls=true"
muxcat mongodb conn ls
muxcat mongodb conn show prod --json
muxcat mongodb conn test prod
muxcat mongodb conn rm local --yes

muxcat mongodb dbs
muxcat mongodb collections --db shop
muxcat mongodb query users '{"age":{"$gte":18}}' --sort age:desc --projection '{"name":1,"age":1}'
muxcat mongodb query users --file filter.json --json
echo '{"status":"ok"}' | muxcat mongodb query users --db shop
muxcat mongodb aggregate orders '[{"$group":{"_id":"$status","n":{"$sum":1}}}]'
muxcat mongodb aggregate orders '[{"$match":{}},{"$out":"x"}]'   # READONLY_VIOLATION（拨号前拦截）

muxcat mongodb insert users '{"name":"a","age":30}'
muxcat mongodb insert users '[{"name":"b"},{"name":"c"}]'
muxcat mongodb insert users --file docs.jsonl --jsonl
muxcat mongodb update users '{"name":"a"}' '{"$set":{"active":true}}'
muxcat mongodb update users '{"status":"pending"}' '{"$set":{"status":"done"}}' --many
muxcat mongodb delete users '{"status":"expired"}' --many
muxcat mongodb delete sessions --many --yes                     # 清空集合（需确认）
muxcat mongodb drop staging --yes                               # 删除集合（需确认）
```

## 已知限制

- 单 host：`hosts` 数组只取一个条目，多 host URI 与 `mongodb+srv://` 均拒绝。
- 无 create-collection 命令（insert 隐式创建集合）；索引管理在后续阶段落地。
