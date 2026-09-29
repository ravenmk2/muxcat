# muxcat

[![Test](https://github.com/ravenmk2/muxcat/actions/workflows/test.yml/badge.svg)](https://github.com/ravenmk2/muxcat/actions/workflows/test.yml)
[![Release](https://img.shields.io/github/v/release/ravenmk2/muxcat)](https://github.com/ravenmk2/muxcat/releases)
[![License](https://img.shields.io/github/license/ravenmk2/muxcat)](LICENSE)

*cat reads files. muxcat reads infrastructure.*

One command line for all your infrastructure. Databases, caches,
config centers, message queues, observability platforms — whatever
lives on the other end, you connect the same way, query the same way,
and read the same output.

- **One learning curve** — every connector shares the same connection
  management, query commands and output contract. Learn one, use them all.
- **For humans and machines** — type-aware, colorized tables in the
  terminal (NULLs, binaries and dates at a glance); a stable JSON
  envelope with semantic exit codes in a pipe.
- **Agent-ready** — structured output and self-correcting error hints
  make muxcat a natural tool for AI agents operating infrastructure.

## Connectors

✅ Available · 📋 Planned

| Connector | Status | Supported versions |
|---|---|---|
| MySQL | ✅ | 5.7+ / 8.x |
| Postgres | ✅ | 12+ |
| SQLite | ✅ | 3.x |
| Redis | ✅ | 6+ |
| MongoDB | 📋 | — |
| Elasticsearch | 📋 | 7 / 8 / 9 |
| Nacos | ✅ | 2.x / 3.x |
| etcd | ✅ | 3.x |
| RabbitMQ | ✅ | 3.8 ~ 4.x |
| AMQP | ✅ | 0.9.1 / 1.0 |
| EMQX | ✅ | 5.x (≤ 5.8.6) |
| MQTT | ✅ | 3.1.1 / 5.0 |
| OpenObserve | ✅ | 0.x / 1.0 |
| Jenkins | ✅ | 2.x LTS |

Design baselines, not hard limits — see `docs/connectors/` for
per-connector compatibility notes.
