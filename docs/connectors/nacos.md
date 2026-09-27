# nacos connector

Nacos connector 通过 Nacos HTTP OpenAPI 接入，**同时支持 Nacos 2.x 与 3.x**，实现为纯 `net/http` 客户端，无外部驱动依赖（符合 `CGO_ENABLED=0` 基线）。命令名 `nacos`（无别名）。覆盖：连接管理、配置的增删查改与监听（watch）、服务与实例浏览、命名空间列表。

认证为 username/password 登录换 bearer token（两版 login 响应结构一致 `{accessToken, tokenTtl, globalAdmin, username}`，仅路径不同），token 只存在于内存、按 tokenTtl 提前刷新，任何输出通道不得出现。

## 配置模型（nacos.json）

```json
{
  "version": 1,
  "instances": {
    "local": { "url": "http://127.0.0.1:8848", "version": "auto" }
  },
  "connections": {
    "local": {
      "instance": "local", "username": "nacos",
      "password": "enc:v1:...", "namespace": "public",
      "readonly": false, "timeout": "30s"
    }
  },
  "defaultConnection": "local"
}
```

- `instances`：名字 → `{url, version?}`。url 是**服务器根地址**（不含 `/nacos` context path），必须含 scheme（`http`/`https`），**不得内嵌 user:password@**（违反报 `CONFIG_INVALID`）。`version` 为服务端大版本钉选：`auto`（默认，首次使用时探测）/ `2` / `3`。
- `connections`：名字 → `{instance, username?, password?, namespace?, readonly?, timeout?}`。`password` 以 `enc:v1:` 加密落盘，永不在输出中回显；`namespace` 默认 `public`，数据命令可用 `--namespace`（别名 `--ns`）逐命令覆盖，两者同时设置时 `--namespace` 优先；`timeout` 覆盖全局 `--timeout`。
- **凭据可选**：2.x 默认不鉴权、3.x client 面只读端点免认证，因此无凭据连接合法——仅当 username+password 同时配置时才登录。
- `conn add <name>` 同名创建 instance 与 connection；`conn rm` 删除连接时，同名 instance 无其他引用则一并删除。

## 版本适配机制

- **auto 探测**：`GET {url}/nacos/v3/admin/core/state`（3.x 公开端点，免认证）返回 200 且为 JSON → 3.x；返回 401/403（3.x admin 面开启鉴权而 2.x 对未知路径 404）→ 3.x；其余 → 2.x。结果缓存在 client 上，每个命令进程最多探测一次。instance 钉选 `version: 2|3` 时跳过探测。
- **API 适配层**：内部按大版本拆 `apiV2` / `apiV3` 两个实现（api_v2.go / api_v3.go），命令层无版本分支。端点对照：

| 操作 | 2.x | 3.x |
|---|---|---|
| config get | `GET /nacos/v2/cs/config`（envelope，data 为内容串）；404 兜底 v1 `GET /nacos/v1/cs/configs`（裸文本） | `GET /nacos/v3/client/cs/config`（client 面免认证） |
| config publish/delete | `POST\|DELETE /nacos/v2/cs/config`；404 兜底 v1 `/nacos/v1/cs/configs`（参数 tenant） | `POST\|DELETE /nacos/v3/admin/cs/config` |
| config ls（列表） | `GET /nacos/v1/cs/configs?search=accurate`（见下方"列表搜索选型"） | `GET /nacos/v3/admin/cs/config/list?search=blur` |
| service ls / show | `GET /nacos/v1/ns/service/list`、`GET /nacos/v1/ns/service` | `GET /nacos/v3/admin/ns/service/list`、`GET /nacos/v3/admin/ns/service` |
| instance ls | `GET /nacos/v1/ns/instance/list` | `GET /nacos/v3/client/ns/instance/list` |
| namespace ls | `GET /nacos/v1/console/namespaces` | `GET /nacos/v3/admin/core/namespace/list` |
| namespace create/update | `POST\|PUT /nacos/v1/console/namespaces`（create 参数 `customNamespaceId`/`namespaceName`/`namespaceDesc`，update 参数 `namespace`/`namespaceShowName`/`namespaceDesc`） | `POST\|PUT /nacos/v3/admin/core/namespace`（统一 `namespaceId`/`namespaceName`/`namespaceDesc`） |
| login | `POST /nacos/v1/auth/login` | `POST /nacos/v3/auth/user/login` |

- **参数名差异**：3.x 用 `groupName`/`namespaceId`（传 2.x 的 `group` 会 400）；2.x 用 `dataId`/`group`/`namespaceId`（v1 为 `tenant`）。3.x 一律走 server 端口，不用 console 端口 API。
- **namespace 语义差异**：2.x 的 public 命名空间 id 是空串（适配层把 `public` 映射为 `""`，展示时再映射回 `public`）；3.x 的 public id 就是 `public`，且 admin 面**必须显式传 namespaceId**（空值结果错误）。
- **响应/错误体两套格式兼容**：Nacos envelope `{code,message,data}`（成功码为 0（v2/v3 envelope）或 200（部分 v1 console 端点），其余取 message 报错）与 Spring 标准错误 JSON `{timestamp,status,error,message,...}`（如 3.1.0 的 403）；`decodeData` 统一拆 envelope，`errDetail` 统一提取错误消息。响应体截断 512 字符、上限 64MB（同 jenkins）。
- **实测应答差异**（以真实 2.2.0 / 2.5.1 / 3.1.0 / 3.2.4 校准）：3.x config get 的 envelope data 是**对象**（`{content, contentType, md5, ...}`，contentType 即服务端记录的配置格式，用于 `config get` 高亮与 `--json` 的 type 字段）而非 2.x 的裸内容串（2.x get 不带格式信息，格式按 dataId 后缀客户端推断，推断不出时再用 accurate 精确查询兜底取服务端 type）；3.x config 不存在时返回 **HTTP 200 + envelope code 20004**（映射为与 2.x 404 相同的 QUERY_ERROR not-found 形态）；3.x instance list 的 data 是**裸 hosts 数组**；3.x service list 的 data 是分页对象 `{totalCount, pageItems:[{name, groupName, ...}]}`；2.x `/v1/console/namespaces` 包 code:200 envelope。**blur 搜索是通配符语义**（`*`/`?`，裸词精确匹配）。
- **列表搜索选型（2.x）**：老版本 2.x（实测 2.2.0）的 `search=blur` 列表项 **type 恒为 null**，`search=accurate` 正常返回 type（console 界面即用 accurate）。因此 2.x `config ls` 走 accurate，`--dataId` 的 blur 过滤改为客户端通配匹配（裸词包装为 `*word*` 的 substring 语义不变）；`config get` 的格式兜底同理走 accurate 精确查询（仅无后缀 dataId 触发，失败静默降级为不高亮）。3.x admin list 无此问题，仍用服务端 blur。
- **认证重试**：已配置凭据时所有请求带 `Authorization: Bearer <token>`；收到 401/403 时重新登录并重试一次（token 过期/服务端重启）。错误密码两版都返回 403 + 误导性 "User not found!" 文案，**错误分类只看状态码不看文案**。

## 命令

### conn 组

| 命令 | 说明 |
|---|---|
| `nacos conn add <name> --url <u> [--username u] [--password p] [--namespace ns] [--version auto\|2\|3] [--readonly] [--timeout 30s] [--set-default]` | 新增连接。非 TTY 缺 `--url` 报 `MISSING_ARGUMENT`；TTY 缺参走 huh 表单补全。`--password` 为明文凭据参数，使用时 stderr 警告。无默认连接时自动设为默认 |
| `nacos conn ls` | 列出连接（name / url / username / namespace / readonly / default 标记） |
| `nacos conn show <name>` | 连接详情；password 不回显（Value map 无 password 字段） |
| `nacos conn rm <name> [--yes]` | 删除连接；非 TTY 必须 `--yes`，TTY 弹确认 |
| `nacos conn default <name>` | 设为默认连接 |
| `nacos conn test <name>` | 版本探测 +（有凭据则）登录，返回 `{ok, api_version: v2\|v3, server_version?, auth, latency_ms}`；server_version 取对应版本的 state 端点的 `version` 字段（best-effort，缺失不影响结论） |

### config 组

配置按 dataId/group/namespace 三元组寻址：group 默认 `DEFAULT_GROUP`（`-g` 覆盖），namespace 默认连接的 namespace（`--namespace` 覆盖，别名 `--ns`）。

| 命令 | 参数 / flag | data 形状 |
|---|---|---|
| `nacos config get <dataId>` | `-g`、`--namespace`、`--no-highlight` | 文本模式裸输出内容（Bare），TTY 下按格式语法高亮（3.x 取服务端 contentType；2.x 按 dataId 后缀推断，推断不出再走 accurate 精确查询取服务端 type；`--no-highlight` 关闭；管道从不着色）；`--json` 为 `{dataId, group, namespace, type, content}` |
| `nacos config ls` | `-g`（精确过滤）、`--dataId`（blur 过滤：裸词按 substring，支持 `*`/`?` 通配符；2.x 为客户端过滤，见"列表搜索选型"）、`--namespace`、`--limit` | 表格 dataId/group/type（type 为服务端记录的配置格式；TTY 下 DEFAULT_GROUP 置灰、type 按格式分色）；单页抓取（pageSize = limit，上限 500） |
| `nacos config publish <dataId>` | `--file <path\|->` 与 `--content <string>` 二选一（必填）、`--type`（text/json/yaml/...，空则由服务端按 dataId 后缀推断）、`-g`、`--namespace` | `{dataId, group, namespace, published: true}` + Message；readonly 守卫 |
| `nacos config delete <dataId>` | `-g`、`--namespace` | `{dataId, group, namespace, deleted: true}` + Message；readonly 守卫 |
| `nacos config watch <dataId>` | `-g`、`--namespace`、`--interval`（默认 5s，仅 3.x 生效） | 流式文本：先输出当前内容，内容变化时输出 `--- changed <ts> ---` + 新内容；Ctrl+C 优雅退出（码 0） |

- **watch 实现**：2.x 用 HTTP 长轮询 `POST /nacos/v1/cs/configs/listener`（body `Listening-Configs=dataId\x02group\x02md5\x02tenant\x01`，头 `Long-Pulling-Timeout: 30000`；md5 变化时服务端提前返回变更坐标，否则 30s 空应答重新订阅），`--interval` 忽略。3.x 无该端点，降级为每 `--interval` 拉 client read API 比对 md5。配置不存在时从空内容开始监听（等创建）。watch 进程无 client 级超时（长轮询 30s 是设计内保持），长驻期间 token 按 tokenTtl 提前刷新。仅文本流，`--json` 报 `UNSUPPORTED_OPERATION`。

### service / instance / namespace 组

namespace 组有别名 `ns`（如 `muxcat nacos ns ls`）。

| 命令 | 说明 |
|---|---|
| `nacos service ls [--limit]` | 表格 service/group（2.x 的列表应答只有名字，group 列留空；TTY 下 DEFAULT_GROUP 置灰） |
| `nacos service show <serviceName> [-g] [--namespace]` | 服务端 detail 原样（文本模式 pretty JSON + 语法高亮） |
| `nacos instance ls <serviceName> [-g] [--namespace]` | 表格 ip/port/weight/healthy/enabled（TTY 下 healthy/enabled 绿/红着色） |
| `nacos namespace ls` | 表格 namespace/showName/quota/configCount（2.x 空 id 显示为 public） |
| `nacos namespace create <namespaceId> [--name 显示名] [--desc 描述]` | 创建命名空间；id 即数据命令 `--namespace` 的寻址键，`--name` 缺省等于 id；readonly 守卫 |
| `nacos namespace update <namespaceId> [--name 新名] [--desc 新描述]` | 更新命名空间；服务端每次更新都要求显示名，`--name` 缺省时自动沿用当前值；readonly 守卫 |

## 错误映射

| 场景 | 错误码 | 退出码 |
|---|---|---|
| HTTP 401 / 403（含登录失败；两版错密码都是 403 + "User not found!"，按状态码分类） | `AUTH_FAILED`（hint 提示检查 username/password，及 3.x admin 面默认强制鉴权） | 4 |
| HTTP 404（config 不存在等；3.x config 不存在为 HTTP 200 + envelope code 20004，归一到同一形态） | `QUERY_ERROR`（消息含 "not found"，hint 提示 dataId/group/namespace 三元组寻址） | 5 |
| 连接拒绝、无此主机 | `CONNECT_FAILED` | 3 |
| 超时（客户端或 context deadline） | `TIMEOUT` | 3 |
| envelope code != 0 | `QUERY_ERROR`（消息为 envelope message） | 5 |
| 其余非 2xx | `QUERY_ERROR`（body 截断 512 字符） | 5 |
| readonly 连接的 config publish/delete | `READONLY_VIOLATION` | 5 |
| watch 的 `--json` 输出 | `UNSUPPORTED_OPERATION` | 2 |

## 已知限制

- 3.x 无 HTTP 长轮询端点，`config watch` 在 3.x 降级为 `--interval` 轮询比对 md5（非推送，最短有效间隔受服务端负载影响）。
- 2.x 老版本（< 2.2）未验证；v2 config 端点缺失时自动兜底 v1，但更老版本的其他行为差异未覆盖。
- Nacos 2.4+ 与 3.x 默认无账号（首次启动需在 console 初始化管理员），在此之前带凭据的 `conn test` 会 `AUTH_FAILED`；2.x 关闭鉴权、3.x client 面只读端点可匿名使用。
- `config ls` / `service ls` 只取单页（pageSize 上限 500，配 `--limit`），不做全量翻页。
- 单次响应体上限 64MB；错误消息中的 body 截断 512 字符。
- TLS 由 url scheme 决定，不支持自定义 CA / 跳过证书校验（后续迭代按需加）。
