# muxcat

[![Test](https://github.com/ravenmk2/muxcat/actions/workflows/test.yml/badge.svg)](https://github.com/ravenmk2/muxcat/actions/workflows/test.yml)
[![Release](https://img.shields.io/github/v/release/ravenmk2/muxcat)](https://github.com/ravenmk2/muxcat/releases)
[![License](https://img.shields.io/github/license/ravenmk2/muxcat)](LICENSE)

muxcat 是基础设施后端的统一命令行客户端，覆盖数据库、缓存、配置中心、消息队列、可观测性平台等各类后端。

- **统一体验**：所有 connector 共享同一套连接管理、查询命令与输出契约 —— 学会一个 connector，就会用全部
- **人机两宜**：TTY 下是类型感知着色的表格（NULL、二进制、日期一眼可辨）；管道中是稳定的 JSON envelope 与语义化退出码
- **Agent 友好**：结构化输出与错误 hint 让 AI Agent 能直接解析结果、自我纠错 —— 天然适合作为 AI Agent 操作基础设施的工具

## Connectors

✅ 已支持　📋 计划中

| Connector | 状态 |
|---|---|
| MySQL | ✅ |
| Postgres | 📋 |
| SQLite | ✅ |
| Redis | ✅ |
| MongoDB | 📋 |
| ElasticSearch | 📋 |
| Nacos（2.x / 3.x） | ✅ |
| etcd | ✅ |
| RabbitMQ | 📋 |
| AMQP | 📋 |
| EMQX | 📋 |
| OpenObserve | ✅ |
| Jenkins | ✅ |
