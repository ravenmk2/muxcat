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
│   └── connector/       # connector 注册表及各 connector 实现
├── schema/              # JSON Schema（Draft 2020-12，go:embed）与校验实现
├── docs/                # 设计文档
└── scripts/             # 辅助脚本
```

## 文档索引

文档变化时同步更新

- docs/architecture.md：总体架构——三层模型、配置、安全、输出契约
- docs/connectors/：每个 connector 一份独立设计文档

## 路线图

✅ 已完成　📋 计划中（不分先后）

| 项目 | 状态 |
|---|---|
| 工程骨架 + SQLite | ✅ |
| MySQL | 📋 |
| Postgres | 📋 |
| Redis | 📋 |
| MongoDB | 📋 |
| ElasticSearch | 📋 |
| OpenObserve | 📋 |
| RabbitMQ | 📋 |
| AMQP | 📋 |
| EMQX | 📋 |
| Jenkins | 📋 |
| TUI | 📋 |
| 跨类型 conn ls 汇总 | 📋 |
| 密钥轮换 | 📋 |
