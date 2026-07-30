# gora

**gora** is a MySQL proxy for **WordPress** and **WooCommerce**, written in Go
and configured with a single `config.yaml`.

A *gora* is the channel that carries water to a mill: the queries are the
water, MySQL is the mill.

> ⚠️ **Early development.** This is the service skeleton: gora installs,
> starts, reloads, reports its state and stops. It does **not** proxy traffic
> yet — the client listener arrives with the next milestone.

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

backend:
  address: "10.0.0.10:3306"  # the MySQL server gora forwards to
  username: "wordpress"
  password: "change-me"

status:
  socket: /run/gora/status.sock   # read-only, feeds `gora status`

log:
  level: info                # debug | info | warn | error
  format: text               # text | json
  path: /var/log/gora        # directory ("stdout"/"stderr" for the console)
```

Unknown keys are an error, not a warning: a typo must never leave a default
silently in place.

## Roadmap

| | Milestone | Contents |
|---|---|---|
| **M0** | skeleton | CLI, configuration, logging, service control, `--init`, status socket, CI |
| M1 | data plane | MySQL protocol, connection pool, keepalive, per-query multiplexing, TLS |
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
