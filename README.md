# gora

**gora** is a MySQL proxy for **WordPress** and **WooCommerce**, written in Go
and configured with a single `config.yaml`.

A *gora* is the channel that carries water to a mill: the queries are the
water, MySQL is the mill.

> ⚠️ **Early development.** gora proxies traffic, pools connections and
> multiplexes them. The query cache, the traffic rules, the profiler and the
> replication manager are still to come — see the roadmap.

## Features

- **Connection pooling** — backend connections belong to gora and are opened
  with its own credentials. When a client disconnects, its connection is
  reset (`COM_RESET_CONNECTION`) and parked for the next one instead of being
  closed: hundreds of short-lived PHP requests share a handful of real MySQL
  connections. `min_idle` opens some up front, `max_lifetime` retires them on
  a schedule so a MySQL restart or a failover heals on its own.
- **Per-query multiplexing** — with `pool.multiplexing` (the default) the
  backend connection returns to the pool **between statements**, not just
  between sessions. Safety is automatic: sessions holding state are pinned to
  their connection — open transactions (tracked by keyword *and* by the
  server status flags, so `autocommit=0` is caught too), temporary tables,
  `LOCK TABLES`/`GET_LOCK()`, prepared statements, user variables. Session
  settings like `SET NAMES` do not pin: gora tracks them and replays them
  when the session lands elsewhere, and since every WordPress session sends
  the same ones, in the steady state nothing is replayed at all.
- **Keepalive pings** — idle connections receive periodic `COM_PING`s, both
  while parked and while attached to an inactive client. A worker that holds
  its connection through a long computation (importing a CSV, calling an
  external API) no longer comes back to *"MySQL server has gone away"*: gora
  kept the session alive meanwhile. If the connection drops anyway, the next
  command transparently attaches a fresh one.
- **Client authentication** — clients authenticate against gora, not against
  MySQL. With a `users` list the database password never leaves gora and
  never appears in `wp-config.php`; a failed login costs no backend
  connection at all.
- **Circuit breaker** — when MySQL is down, waiting for per-request timeouts
  melts PHP-FPM. After `pool.breaker.failures` consecutive failures gora
  fails fast with a clean error and probes the backend until it recovers.
  `listen.max_connections` caps concurrent clients, and `pool.max_query_time`
  kills any statement running longer than the limit so one runaway query
  cannot hold a pooled connection hostage.
- **Graceful shutdown** — a stop stops accepting, lets the statements already
  running finish (up to `listen.drain_timeout`) and only then closes client
  connections. Idle sessions are not waited for.
- **TLS** — `listen.tls` encrypts client connections, `backend.tls` encrypts
  the connection toward a remote MySQL, with an optional custom CA.
- **`gora status`** — a read-only unix socket exposes live state: connected
  clients, pinned sessions, statements running, pool occupancy, breaker
  state, and how often clients had to wait for a connection.

## Command line

Service verbs are bare words. Everything else is an option and always takes
two dashes; a single dash is rejected with the exact string to type instead.

```sh
gora start                    # run in the foreground (what systemd calls)
gora stop                     # SIGTERM the running instance and wait for it
gora restart                  # stop, then run in the foreground
gora reload                   # SIGHUP: re-read the configuration
gora status                   # print the state of the running instance
```

```sh
gora --init                   # install as a systemd service
gora --check-config           # validate the configuration and exit
gora --config /etc/gora/config.yaml
gora --version
gora --help
```

`gora stop`, `restart` and `reload` find the running instance through
`/run/gora/gora.pid`. Under systemd the usual `systemctl {start,stop,reload}
gora` works as well — the unit runs `gora start` and reloads with SIGHUP.

## Quick start

Download the Linux binary from the Releases page and let `--init` install
everything (it must run as root):

```sh
sudo ./gora --init
```

It copies the running binary to `/sbin/gora`, creates the `gora` system user,
`/etc/gora` (with `conf.d`), `/var/log/gora`, the systemd unit and the
logrotate configuration. Re-run it after downloading a new binary: that is
the upgrade procedure. Everything gora manages is rewritten, with one
exception — an existing `config.yaml` is copied to `config.yaml.bak` first.

```sh
sudo $EDITOR /etc/gora/config.yaml
sudo gora --check-config
sudo systemctl enable --now gora
gora status
```

## Configuration

See [config.default.yaml](cmd/gora/config.default.yaml) for the commented
template `--init` installs.

```yaml
listen:
  address: "0.0.0.0:3306"    # DB_HOST in wp-config.php
  max_connections: 0         # 0 = unlimited
  drain_timeout: 10s

backend:
  address: "10.0.0.10:3306"  # the MySQL server gora forwards to
  username: "wordpress"
  password: "change-me"
  connect_timeout: 5s

pool:
  max_open: 100
  max_idle: 10
  min_idle: 0
  ping_interval: 30s
  idle_timeout: 5m
  max_lifetime: 1h
  acquire_timeout: 5s
  multiplexing: true
  max_query_time: 0
  breaker:
    failures: 3
    probe_interval: 2s

status:
  socket: /run/gora/status.sock   # read-only, feeds `gora status`

log:
  level: info                # debug | info | warn | error
  format: text               # text | json
  path: /var/log/gora        # directory ("stdout"/"stderr" for the console)
```

To keep the real database credentials out of `wp-config.php`, add proxy-only
accounts — clients then authenticate against gora with these, while gora
still connects to MySQL with the `backend` credentials:

```yaml
users:
  - username: "wordpress"
    password: "proxy-only-password"
```

Unknown keys are an error, not a warning: a typo must never leave a default
silently in place.

## Current limitations

- Result sets are buffered in memory while being relayed; a full-table dump
  is better run directly against MySQL.
- Multi-statement mode, `LOAD DATA LOCAL INFILE` and the replication
  commands are refused with a clean error.
- A client that never selects a database may observe the one left by a
  previous session on a reused connection. WordPress always selects one.

## Roadmap

| | Milestone | Contents |
|---|---|---|
| ~~M0~~ | skeleton | CLI, configuration, logging, service control, `--init`, status socket, CI |
| **M1** | data plane | MySQL protocol, connection pool, keepalive, per-query multiplexing, TLS |
| M2 | cache | WordPress-aware query cache, conf.d rules, hot reload, warm-up |
| M3 | traffic | query rewriting, firewall, per-digest throttling |
| M4 | profiling | slow query log, aggregated report, index and rewrite advisor |
| M5 | topology | multiple nodes, health checks, read/write split, degraded mode |
| M6 | replication | GTID provisioning, seeding, monitoring, failover |
| M7 | observability | `gora top`, extended status, documentation |

## Development

```sh
make vet
make test
```

Real builds happen in CI: every push to `main` runs the tests and builds
Linux binaries (amd64 and arm64), downloadable as artifacts from the Actions
run. Pushing a `v*` tag publishes them on the Releases page via GoReleaser.

## License

[MIT](LICENSE)
