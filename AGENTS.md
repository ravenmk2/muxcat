# AI Agents 工作规范

muxcat 是一个面向基础设施后端的统一命令行客户端（Go CLI），以内置 Connector 方式接入各类基础设施（数据库、缓存、消息队列、可观测性平台等），提供一致的连接管理、查询与结构化输出（envelope）契约。

## 目录结构

```txt
muxcat/
├── cmd/muxcat/          # 程序入口，仅组装启动（var version = "dev"）
├── internal/            # 应用私有代码
│   ├── cli/             # cobra 命令树：root、config 组、connector 注册
│   ├── config/          # 配置读写、原子写、点路径 get/set、MUXCAT_HOME
│   ├── secret/          # keychain/env 主密钥、AES-256-GCM、enc:v1: blob
│   ├── output/          # Renderer、envelope、TTY/降级检测、退出码
│   ├── upgrade/         # 自更新：GitHub release 查询、带重试下载、checksum 校验、自替换
│   └── connector/       # connector 注册表及各 connector 实现
├── schema/              # JSON Schema（Draft 2020-12，go:embed）与校验实现
├── docs/                # 设计文档
└── scripts/             # 辅助脚本
```

## 文档索引

文档变化时同步更新

- docs/architecture.md：总体架构——三层模型、配置、安全、输出契约
- docs/upgrade.md：自更新设计——upgrade 命令、下载重试、平台自替换
- docs/connectors/：每个 connector 一份独立设计文档

## 安全规范

**所有功能禁止明文输出密码、密钥等凭据。** 任何输出通道（table/plain/tsv/json、stderr、错误消息、hint、日志）都不得回显明文凭据。具体要求：

- 凭据落盘仅允许 `enc:v1:` blob；解密后的明文只存在于内存，不得写入任何输出
- `conn ls` / `conn show` 等命令不回显密码字段
- `--password` 等明文 flag 仅作输入入口：打 stderr 警告后加密存储，不回显
- 连接串/DSN 内含解密后凭据，不得出现在错误消息或日志中
- 可能携带凭据的服务端配置/元信息（如 redis `CONFIG GET` 的 requirepass/masterauth）必须脱敏为 `***`；空值保持空，保留"是否已设置"的可判断性
- 新增 connector 或命令时，自查所有输出路径是否满足本条；测试应包含凭据不泄露的断言

## 路线图

✅ 已完成　📋 计划中（不分先后）

| 项目 | 状态 |
|---|---|
| 工程骨架 + SQLite | ✅ |
| 自更新 upgrade | ✅ |
| MySQL | ✅ |
| Postgres | 📋 |
| Redis | ✅ |
| etcd | 📋 |
| MongoDB | 📋 |
| ElasticSearch | 📋 |
| OpenObserve | ✅ |
| RabbitMQ | 📋 |
| AMQP | 📋 |
| EMQX | 📋 |
| Jenkins | 📋 |
| TUI | 📋 |
| 跨类型 conn ls 汇总 | 📋 |
| 密钥轮换 | 📋 |
