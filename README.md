# muxcat

[![Test](https://github.com/ravenmk2/muxcat/actions/workflows/test.yml/badge.svg)](https://github.com/ravenmk2/muxcat/actions/workflows/test.yml)
[![Release](https://img.shields.io/github/v/release/ravenmk2/muxcat)](https://github.com/ravenmk2/muxcat/releases)
[![License](https://img.shields.io/github/license/ravenmk2/muxcat)](LICENSE)

**cat 之于文件，muxcat 之于基础设施。**

muxcat 是一个面向基础设施后端的统一命令行客户端：用同一套连接管理、查询语法和输出契约，接入数据库、缓存、消息队列、可观测性平台等各类后端。

- **统一体验**：所有 connector 共享 conn 连接管理、全局 flag（`--output table/plain/tsv/json`、`--limit`、`--timeout`）与 envelope 结构化输出
- **人机两宜**：TTY 下类型感知着色的表格（NULL、二进制 hex、日期格式一眼可辨），管道中输出稳定的 JSON envelope，退出码语义化

## Connectors

✅ 已支持　📋 计划中

| Connector | 状态 |
|---|---|
| SQLite | ✅ |
| Redis | ✅ |
| MySQL | ✅ |
| Postgres | 📋 |
| etcd | 📋 |
| MongoDB | 📋 |
| ElasticSearch | 📋 |
| OpenObserve | 📋 |
| RabbitMQ | 📋 |
| AMQP | 📋 |
| EMQX | 📋 |
| Jenkins | 📋 |
