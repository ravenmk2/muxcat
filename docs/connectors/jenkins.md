# jenkins connector

Jenkins connector 通过 Jenkins 的 REST API（HTTP Basic Auth：username + API token）接入，实现为纯 `net/http` 客户端，无外部驱动依赖（符合 `CGO_ENABLED=0` 基线）。命令名 `jenkins`，别名 `jk`。覆盖：连接管理、job/folder 浏览、build 历史与控制台日志（含 `--follow` 流式跟随）、构建队列、节点状态、写操作（`job build` 触发构建可 `--wait` 等待结果、`build stop`、`job enable/disable`）、原生请求透传（request）。其余管理端点可用 `jk request` 透传。

API 核对的权威来源：Jenkins 实例自身的 `/api/` 文档入口 + 官方 Remote access API 文档。

## 配置模型（jenkins.json）

```json
{
  "version": 1,
  "instances": {
    "local": { "url": "http://127.0.0.1:8080" }
  },
  "connections": {
    "local": {
      "instance": "local", "username": "admin",
      "token": "enc:v1:...", "readonly": false, "timeout": "30s"
    }
  },
  "defaultConnection": "local"
}
```

- `instances`：名字 → `{url}`，纯端点属性。url 必须含 scheme（`http`/`https`），**不得内嵌 user:password@**（防明文凭据落盘，违反报 `CONFIG_INVALID`）。
- `connections`：名字 → `{instance, username?, token?, readonly?, timeout?}`。`token` 是 Jenkins **API token**（不是 Web 登录密码），以 `enc:v1:` 加密落盘，永不在输出中回显；`timeout` 为 Go duration 字符串，覆盖全局 `--timeout`。
- **多用户**：凭据是 connection 属性——同一 instance 可挂多个连接（不同权限用户）。
- `conn add <name>` 同名创建 instance 与 connection；`conn rm` 删除连接时，同名 instance 无其他引用则一并删除。

## 命令

### conn 组

| 命令 | 说明 |
|---|---|
| `jk conn add <name> --url <u> [--username u] [--token t] [--readonly] [--timeout 30s] [--set-default]` | 新增连接。非 TTY 缺 `--url` 报 `MISSING_ARGUMENT`；TTY 缺参走 huh 表单补全（url/username/API token/readonly）。`--token` 为明文凭据参数，使用时 stderr 警告。无默认连接时自动设为默认 |
| `jk conn ls` | 列出连接（name / url / username / readonly / default 标记） |
| `jk conn show <name>` | 连接详情；token 不回显（Value map 无 token 字段） |
| `jk conn rm <name> [--yes]` | 删除连接；非 TTY 必须 `--yes`，TTY 弹确认 |
| `jk conn default <name>` | 设为默认连接 |
| `jk conn test <name>` | `GET /whoAmI/api/json` 验证认证，返回 `{ok, user, latency_ms, version}`。`user` 为 whoAmI 的 fullName（回退连接配置的 username），方便确认 token 对应的账号；version 取自响应头 `X-Jenkins`（每次请求都带），不可得时留空附 note，不影响测试结论 |

### job 组

以下命令均支持 `-c/--conn` 与全局 `--timeout` / `--limit`。Job 一律按**全名**寻址（`folder/sub/job`），内部转换为 Jenkins 的 URL 形式（`/job/folder/job/sub/job/job`），各段做 path escape。

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `jk job ls` | `--folder F`（从该 folder 开始列）、`--class C`（按 class 列筛选，如 `workflow-job`；客户端筛选，folder 递归不受影响） | `GET {prefix}/api/json?tree=jobs[name,fullName,url,color,buildable,lastBuild[number,result,timestamp]]` | `--json` 为入口响应原样；文本列 `full_name, class, status, last_build` |
| `jk job show <job>` | — | `GET /job/.../api/json?tree=displayName,...,property[parameterDefinitions[...]]` | `--json` 为结构化 Value（参数默认值已脱敏）；文本为 `key: value` 列表，嵌套结构紧凑 JSON |
| `jk job build <job>` | `--param k=v`（可重复）、`--wait`、`--wait-timeout`（默认 10m） | 先 `GET /job/.../api/json?tree=property[parameterDefinitions[name]]` 探测是否参数化：参数化（含 multibranch 分支 job）→ `POST buildWithParameters`（无 `--param` 时 query 为空，按默认值触发——参数化 job 的裸 `POST /build` 会被 Stapler 以 400 "Nothing is submitted" 拒绝）；非参数化 → `POST /build`（此时仍带 `--param` 走 buildWithParameters，服务端 400 原样上浮）。探测失败（如 404）直接上浮 | 无 `--wait`：`{queued, queue_id, queue_url}`；`--wait`：构建汇总 `{job, number, result, duration, url, timestamp}`（见「--wait 语义」） |
| `jk job enable <job>` / `jk job disable <job>` | — | `POST /job/.../enable\|disable`（Jenkins 应答 302 跳 job 页，http client 自动跟随） | `{job, enabled: true\|false}` + Message |

- `job ls` 对 folder（含 multibranch，`_class` 匹配）按 fullName 递归拉取，最大递归深度 10（防失控）。
- `class` 列把 Java 类名简化为 `folder` / `workflow-job` / `freestyle` / `multibranch` / `matrix` 等；`status` 列映射 color 球：`blue→success`、`red→failed`、`yellow→unstable`、`aborted/disabled/not_built`，`_anime` 后缀统一为 `building`。
- **参数默认值脱敏**：`type` 含 `Password` 的参数默认值显示为 `***`（空值保持空，保留"是否已设置"的可判断性）；`--json` 同样脱敏（不回传服务端原值）。
- `conn show` / `job show` / `build show` 的 `key: value` 文本输出在 TTY 彩色开启时按 YAML 语法高亮（`Result.Syntax = "yaml"`）；`--json`、非 TTY、`NO_COLOR`/`--no-color` 下自动退化为无高亮原文。

### build 组

build 引用支持别名：`last`→`lastBuild`、`lastSuccessful`、`lastFailed`、`lastCompleted`，或正整数 build 号；其余值报 `CONFIG_INVALID`。

| 命令 | 参数 / flag | 端点 | data 形状 |
|---|---|---|---|
| `jk build ls <job>` | — | `GET /job/.../api/json?tree=builds[number,result,timestamp,duration,url,building]` | `--json` 原样；文本列 `number, result, timestamp(本地时间), duration(人性化), url`，building 中的 build result 显示 `BUILDING` |
| `jk build show <job> <n\|alias>` | — | `GET /job/.../{ref}/api/json?tree=...,actions[parameters[name,value,_class],causes[shortDescription]],artifacts[...],changeSet[...]` | 结构化 Value：基本信息 + parameters + causes + artifacts + changes |
| `jk build log <job> <n\|alias>` | `--full`、`--follow`、`--wait-timeout`（默认 10m） | `GET /job/.../{ref}/logText/progressiveText?start=N` | 文本直接输出日志原文；`--json` 为 `{job, build, size, truncated, log}` |
| `jk build stop <job> <n\|alias>` | — | `POST /job/.../{ref}/stop`（302 跳 build 页，自动跟随） | `{job, number, stopped: true}` + Message |

- **参数值脱敏**：参数名匹配 `(?i)password|secret|token|key` 或参数 `_class` 为 PasswordParameter 时，值显示 `***`（空值保持空）；`--json` 同样脱敏。
- `build log` 默认只保留末尾 **200 行**（全局 `--limit` 显式设置时覆盖该数值；`--limit 0` 或 `--full` 输出全部），被截断时首行标注 `... (showing last N lines, --full for all)`。
- **--follow 输出模型**：先按上述规则输出当前 tail（`--full` 则先输出全量），然后从 `size` 偏移循环 `progressiveText?start=size` 轮询增量（间隔 1s，读 `X-Text-Size`/`X-More-Data`），直到日志完整（build 结束的 log 直接退出）。文本模式增量块到达即写 stdout（真正跟随）；`--json` 无法流式，收集全部增量后输出单个 envelope（`truncated=false`，log 为完整日志）。`--wait-timeout` 到期报 `TIMEOUT`（文本模式已流出的部分保留在 stdout）。

### --wait 语义

`jk job build <job> --wait` 的轮询流程：

1. `POST build[WithParameters]` 返回 201 + `Location: /queue/item/{id}/`，解析出 queue id（Location 形态异常报 `QUERY_ERROR`，header 原值不入错误消息）。
2. **waitQueue**：每 2s 轮询 `GET /queue/item/{id}/api/json?tree=executable[number,url],cancelled,why`，直到拿到 `executable.number`；`cancelled=true` 报 `QUERY_ERROR`（why 附进消息）。**queue item 404 不视为错误**：执行器空闲时 item 可能在首次轮询前（毫秒级）就已出队——这是常见路径而非边缘竞态。404 时回退到 `GET {jobPath}/api/json?tree=builds[number,queueId]{,20}`，在最近 build 记录里按 `queueId` 匹配我们的 queue id（build 记录中 queueId 为数字类型，统一转 int64 比较）找到对应 build 号；本轮找不到则继续轮询。
3. **waitBuild**：每 1s 轮询 `GET {path}/{n}/api/json?tree=number,...,building`，直到 `building=false && result!=null`。

退出码约定：`SUCCESS`/`UNSTABLE` → 0；`FAILURE`/`ABORTED`/其他 → `QUERY_ERROR`（退出码 5），经 `cli.RenderPartial`：文本模式先渲染构建汇总再非零退出，`--json` 失败 envelope 的 `data` 携带构建汇总。`--wait-timeout`（默认 10m，与单次 HTTP `--timeout` 解耦）到期报 `TIMEOUT`（hint 指向 `--wait-timeout`）。

### queue 组

| 命令 | 端点 | data 形状 |
|---|---|---|
| `jk queue ls` | `GET /queue/api/json?tree=items[id,task[fullName,url],why,blocked,buildable,stuck,inQueueSince]` | `--json` 原样；文本列 `id, job, why, waiting(按 inQueueSince 换算), state(stuck>blocked>buildable>pending)` |

### node 组

| 命令 | 端点 | data 形状 |
|---|---|---|
| `jk node ls` | `GET /computer/api/json?tree=computer[displayName,offline,temporarilyOffline,idle,numExecutors,executors[currentExecutable[...]],description]` | `--json` 原样；文本列 `name, status(online/offline/temp-offline), idle, executors(busy/total), description` |

### request

```
jk request <method> <path> [--file <path|->] [--content-type <mime>]
```

原生请求透传（curl 语义）：

- method 枚举 `GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS`（大小写不敏感）；path 必须以 `/` 开头，原样拼接实例 baseURL（如 `/job/my-job/api/json`）。
- `--file` 提供请求体（`-` 读 stdin）；有 body 时默认 `Content-Type: application/json`，`--content-type` 显式覆盖（如向 `/createItem` 提交 job 定义需要 `text/xml`）。
- **完成的 HTTP 交换不论状态码都原样报告**：data 为 `{status, headers, body}`（body 按响应 Content-Type 解析为 JSON 值，否则字符串）；只有传输层失败（连接失败/超时）才产生错误。
- 非 GET/HEAD 自动携带 CSRF crumb（见下节），写端点因此也可达（建 job：`jk request POST "/createItem?name=x" --content-type text/xml --file config.xml`；删 job：`jk request POST /job/x/doDelete`）。
- **readonly 连接仅允许 GET/HEAD**，其余方法报 `READONLY_VIOLATION`（退出码 5）。

## Crumb / CSRF

Jenkins 默认开启 CSRF 防护，非 GET/HEAD 请求必须带 crumb 头。client 对非 GET/HEAD 请求自动：

1. `GET /crumbIssuer/api/json` 取 `{crumbRequestField, crumb}` 并缓存（每个命令进程一次）；404 表示服务端未启用 crumb issuer，无需 crumb，不视为错误。
2. 请求头附加 `{crumbRequestField}: {crumb}`。
3. 响应为 403 且 body 含 `No valid crumb`（crumb 过期，如服务端重启）时，重新取 crumb 重试一次。

crumb 获取的其他失败不阻断真实请求——让真实请求自己的错误（如 401）原样上浮。

## 错误映射

| 场景 | 错误码 | 退出码 |
|---|---|---|
| HTTP 401 / 403 | `AUTH_FAILED`（hint 提示用 API token 而非登录密码） | 4 |
| HTTP 404 | `QUERY_ERROR`（消息含 "not found"，hint 提示检查 job 全名 folder/sub/job 与 build 号/别名） | 5 |
| 连接拒绝、无此主机 | `CONNECT_FAILED` | 3 |
| 超时（客户端或 context deadline） | `TIMEOUT` | 3 |
| `--wait` / `--follow` 轮询超 `--wait-timeout` | `TIMEOUT`（hint 指向 `--wait-timeout`） | 3 |
| 其余非 2xx | `QUERY_ERROR`（body 截断 512 字符——Jenkins 错误页是 HTML，截断防刷屏） | 5 |
| `--wait` 构建终态为 FAILURE/ABORTED/其他 | `QUERY_ERROR`（`--json` envelope 的 data 携带构建汇总） | 5 |
| `--wait` 时 queue item 被取消 | `QUERY_ERROR`（消息含 cancelled 与 why） | 5 |
| readonly 连接的写操作（job build/stop/enable/disable、request 非 GET/HEAD） | `READONLY_VIOLATION` | 5 |
| request 的非 2xx 完成交换 | 不报错（data 原样报告 status） | 0 |

## 已知限制

- `job ls` folder 递归深度上限 10。
- 单次响应体上限 64MB（含大日志；超大日志用 tail/--limit 控制输出行数，`--follow` 文本模式流式输出不受行数限制）。
- `--wait`/`--follow` 为客户端轮询（queue 2s / build 1s / log 1s 间隔），非 WebSocket/推送；轮询间隔不可配置。
- 写操作守卫只有 readonly 连接（`READONLY_VIOLATION`），不设 `--yes` 确认（显式命令即意图）。
- TLS 由 url scheme 决定，不支持自定义 CA / 跳过证书校验（后续迭代按需加）。
