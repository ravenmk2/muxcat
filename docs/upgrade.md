# muxcat upgrade 自更新设计

`muxcat upgrade` 基于现有 GitHub Releases 发布链路（goreleaser）实现自更新：查询最新 release、下载对应平台资产、校验 SHA256、原子替换自身二进制。全部用标准库实现，不引入更新框架。

## 命令

```txt
muxcat upgrade                  # 检查并升级到最新 release
muxcat upgrade --check          # 只检查，不升级
muxcat upgrade --version 0.3.0  # 升级到指定版本（v 前缀可省）
muxcat upgrade --attempts 5     # 单个资产的下载尝试次数，默认 10
```

复用全局 flag：`--yes`（跳过确认）、`--json`、`--no-color`。`--timeout` 语义在 upgrade 下特化：显式指定时生效；未指定时默认 **5 分钟**（查询向的 30s 全局默认值和 `props.defaults.timeout` 不适用于几十 MB 的资产下载），约束整个命令（含全部重试）。

## 流程

1. **解析（Resolve）**
   - 默认 `GET /repos/ravenmk2/muxcat/releases/latest`（该端点天然跳过 prerelease）；`--version` 指定时改用 `GET .../releases/tags/v<ver>`。
   - 设置 `GITHUB_TOKEN` 环境变量时带 `Authorization: Bearer` 头，用于提升匿名 API 限流（60 次/小时/IP）。
   - 版本判定：当前为 `dev`（本地构建）→ 可升级（「转正」）；与 latest 规范化后相等 → already up to date，exit 0（幂等）；当前版本高于 latest 且未指定 `--version` → 不降级，按 up to date 处理；显式 `--version` 指定旧版本 → 尊重用户意图，允许降级。
2. **确认**：TTY 且未 `--yes` 时用 huh Confirm 确认（**默认 Yes**，回车即确认）；非 TTY 且未 `--yes` 直接报 `UNSUPPORTED_OPERATION` + hint（`re-run with --yes`），绝不等待输入（与全局交互契约一致）。
3. **下载（Apply）**：资产名 `muxcat_{version}_{GOOS}_{GOARCH}.tar.gz`（Windows 为 `.zip`；资产名不带 v 前缀，下载 URL 的 tag 带 v 前缀），连同 `checksums.txt` 下载到系统临时目录。
   - 重试：网络错误 / 连接停滞 / 5xx / 429 可重试，其余 4xx 立即失败；指数退避 500ms 起、×2、5s 封顶、全 jitter；总尝试次数为 `--attempts`；整体受 `--timeout` context 约束（超时映射 `TIMEOUT`，错误信息报告**实际**尝试次数）。
   - 停滞检测：单次尝试 30s 无任何数据流入（含等待响应头阶段）即中止该次尝试并判为可重试（`IdleTimeout`）；缓慢但持续有数据的下载不受影响，不会被中途掐断。
   - 进度：TTY 且非 json 模式时在 **stderr** 显示 charm（bubbles/progress）进度条，保持 stdout 可管道；`Content-Length` 未知时降级为已下载字节数。
4. **校验**：解析 `checksums.txt`（`<sha256>  <文件名>` 行格式），对资产做 SHA256 比对；缺条目或不符报 `UPGRADE_FAILED`。
5. **解压**：按扩展名选择 tar.gz / zip，取出 `muxcat` / `muxcat.exe`，置 0755。
6. **自替换**：`os.Executable()` + `EvalSymlinks` 定位真实路径；先在目标目录探测可写性，不可写报 `UPGRADE_FAILED` + hint（换 `~/.local/bin` 或提权）。
   - **Unix**：目标目录写临时文件 → `chmod 0755` → `rename` 原子覆盖（无备份）。
   - **Windows**：运行中的 exe 不能覆盖但能 rename——当前 exe 改名 `<target>.old` → 写入新文件；失败时尽力还原。`.old` **保留为备份**并在结果中展示路径，下次 upgrade 启动时清理旧备份。
7. **输出**：成功走 envelope/RenderResult，data 为 `{from, to, asset, installed_to, backup}`（Unix 无备份时 `backup` 为空）；`--check` 输出 `{current, latest, update_available}`。

## TTY 彩色输出

TTY 且输出模式解析为 table（auto 默认）时，upgrade 走高亮渲染（配色复用 help 模板调色板：紫 `#7D56F4`、绿 `#04B575`、蓝 `#2D9CDB`，颜色决策走 `ResolveColor` 链，`--no-color`/`NO_COLOR`/非 TTY 自动降级）：

- `--check` 有新版：`muxcat <latest> is available (current: <current>)` + 命令提示。
- 确认后先输出 `upgrade:` / `package:` / `location:` 三行（stdout，无缩进），随后进度条（stderr），完成后 `✓ upgrade complete`；Windows 有备份时追加 `backup:` 行。
- plain/tsv/json 模式与非 TTY 严格保持标准契约输出，不含任何 ANSI。

## 错误码与退出码

| 场景 | 错误码 | 退出码 |
|---|---|---|
| 网络 / API 失败、下载重试耗尽 | `CONNECT_FAILED` | 3 |
| 下载/API 超时 | `TIMEOUT` | 3 |
| 非 TTY 未带 `--yes` | `UNSUPPORTED_OPERATION` | 2 |
| `--attempts` 非法 | `CONFIG_INVALID` | 2 |
| release/资产缺失、校验失败、目录不可写、替换失败 | `UPGRADE_FAILED` | 5 |

`UPGRADE_FAILED` 为本次新增错误码（映射执行类退出码 5）。

## 安全边界

- 信任 GitHub 发布渠道（TLS + release 资产）+ `checksums.txt` 防下载损坏/投毒的一致性问题；cosign/minisign 签名校验为后续增量（需改 goreleaser 配置并内置公钥）。
- 临时文件一律落在系统临时目录，失败不污染现有二进制；替换前目标二进制不被修改。

## 明确不做（后续增量）

- 启动时后台自动检查更新
- `upgrade --rollback` 回滚（Windows 的 `.old` 天然是一份备份，但暂无命令面）
- 包管理器（brew/scoop/apt）安装检测
- 签名验签
