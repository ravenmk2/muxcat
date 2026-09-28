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
| Postgres | 📋 | — |
| SQLite | ✅ | 3.x |
| Redis | ✅ | 6+ |
| MongoDB | 📋 | — |
| Elasticsearch | 📋 | — |
| Nacos | ✅ | 2.x / 3.x |
| etcd | ✅ | 3.x |
| RabbitMQ | ✅ | 3.8 ~ 4.x |
| AMQP | 📋 | — |
| EMQX | 📋 | — |
| OpenObserve | ✅ | 0.x / 1.0 |
| Jenkins | ✅ | 2.x LTS |

Version ranges are design baselines. Per-feature compatibility details
live in each connector's doc under `docs/connectors/`.
