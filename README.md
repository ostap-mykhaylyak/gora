# gora

**gora** is a MySQL proxy for **WordPress** and **WooCommerce**, written in Go
and configured with a single `config.yaml`.

A *gora* is the channel that carries water to a mill: the queries are the
water, MySQL is the mill.

gora proxies traffic, pools connections, multiplexes them, caches
WordPress's hottest reads, splits reads from writes across replicas, sets up
and governs the replication between them, and can rewrite, refuse, throttle
and profile what goes through it.

> ⚠️ **Not yet run against a production workload.** The test suite drives a
> real MySQL protocol implementation, not a mock, and covers the routing,
> caching and replication decisions described below — but the cluster
> commands have not been exercised against a real MySQL server pair. Try it
> somewhere you can afford to be wrong first.

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
- **WordPress-aware query cache** — the autoloaded options query (the
  hottest query of every pageload) and transient reads are served from RAM.
  Invalidation is write-driven: every write flowing through gora drops
  exactly the entries it can affect — per option name on the options table,
  per table elsewhere — with a TTL as a safety net. Reads inside a
  transaction always bypass the cache, and a write gora cannot parse flushes
  everything: in doubt it prefers a database roundtrip to a stale answer.
- **Stampede protection** — when many workers ask for the same cacheable
  query at once, one of them asks the database and the others wait for that
  answer. A cold cache under load is exactly when the database can least
  afford a hundred copies of the same query.
- **Rule-based caching for WooCommerce** — drop-ins in `/etc/gora/conf.d/`
  add rules as regex + TTL + invalidation tables, and `--init` installs a
  WooCommerce profile. `{prefix}` expands to the table prefix, which gora
  discovers from the database on its own unless you name it. Carts, customer
  sessions and orders — legacy or High-Performance Order Storage — are
  deliberately never cached.
- **Paginated listings** — a shop listing runs `SQL_CALC_FOUND_ROWS` and
  then asks for the total with `FOUND_ROWS()`, which reads a counter living
  on the connection. gora caches the rows and the total together and replays
  both, and refuses to serve rows whose total it has not captured yet:
  wrong pagination on a shop that looks healthy is worse than a cache miss.
- **Cache warm-up** — an option write invalidates the autoloaded snapshot;
  with `cache.warmup` gora refetches it in the background straight away, so
  the next visitor never pays for it.
- **Hot reload** — `systemctl reload gora` (SIGHUP) applies the conf.d
  drop-ins without dropping a single client connection, so adding a rule
  during an incident does not mean a restart. A drop-in that no longer
  parses is reported and the previous rules stay in force.
- **Query rewriting** — regex replacements applied before execution, for the
  SQL you cannot fix at the source: a plugin you did not write, a theme
  nobody maintains. Rewrites change what the database is asked, so none ship
  enabled. Prepared statements are never rewritten.
- **Query firewall** — a `block` rule refuses matching statements with a
  clean MySQL error before they reach the backend. With hot reload it is the
  emergency brake for a runaway plugin query: add the rule, reload, done.
  Arm it with `dry_run: true` first and it reports what it would have
  refused without refusing anything.
- **Per-statement throttling** — a `throttle` rule bounds how many copies of
  the same statement run at once. It is the brake for the query that is not
  wrong, only ruinous in quantity: the excess waits for `wait` and is then
  refused, so the site stays slow instead of going down. Limits apply per
  statement shape — literals are normalised away — so one runaway query is
  held back without touching everything else the rule matches. A cache hit
  costs the database nothing and never needs a slot.
- **Read/write split** — list `backend.replicas` and reads go to them while
  writes go to the primary. After a session writes, its own reads stay on
  the primary for `routing.sticky_after_write`: replication is asynchronous,
  and a shop that has just saved an order must not read back the state from
  before it. Everything inside a transaction stays on the primary, because a
  read there is reading uncommitted state that exists nowhere else.
- **Health checks** — every node is asked, on `routing.health_interval`,
  whether it is reachable, whether it is read-only, and how far behind it
  is. A replica beyond `routing.max_replica_lag` leaves the read rotation,
  and so does one whose lag gora cannot read at all: a replica that might be
  a day behind is not a replica, it is a backup.
- **Degraded mode** — with the primary unreachable the site does not stop.
  Reads keep coming from the replicas and the cache; writes are refused with
  the error code MySQL itself uses for a read-only server, so the client
  sees a database saying no rather than a connection timing out. A primary
  that has quietly become read-only — a failover nobody told gora about —
  is treated the same way.
- **Replication management** — gora does not copy data between servers: MySQL
  has done that for twenty years with binary logs and GTIDs. What it does is
  the part above. Point it at empty MySQL servers, run `gora --init-cluster`,
  and it checks each one can replicate, turns on GTIDs, gives them distinct
  server ids, creates the replication account, seeds the replicas with the
  clone plugin when the primary already has data, and starts them following
  the primary. No `my.cnf` is edited by hand, and a replica that has data of
  its own is refused rather than overwritten.
- **Failover** — when the primary goes, `gora --promote <address>` makes a
  replica the primary: it stops replicating, forgets where it was replicating
  from, becomes writable, and the others are repointed at it. With
  `replication.failover: automatic` gora does it by itself after
  `failover_delay`. The new primary is recorded in a state file, so a restart
  of gora does not go back to writing to a server that is now a replica — and
  a running gora picks up a promotion made on the command line without being
  restarted. The default is manual: promoting a primary has consequences that
  outlive the incident.
- **Circuit breaker** — when MySQL is down, waiting for per-request timeouts
  melts PHP-FPM. After `pool.breaker.failures` consecutive failures gora
  fails fast with a clean error and probes the backend until it recovers.
  `listen.max_connections` caps concurrent clients, and `pool.max_query_time`
  kills any statement running longer than the limit so one runaway query
  cannot hold a pooled connection hostage.
- **Profiling and advice** — with `profiling.enabled` gora turns its
  position into guidance. It sees every statement, so it aggregates them by
  shape: calls, total/avg/max time, rows, cache hit ratio, heaviest first,
  and logs anything slower than `slow_query` immediately. Then it explains
  the heaviest ones against the real schema and says what to do — the
  `ALTER TABLE` for a missing index, a `FULLTEXT` index for a search no
  B-tree can serve, the conf.d rule for a known antipattern. It suggests;
  it never runs DDL. Suggestions are kept in `advice_file`, so they survive
  a restart and `gora --advice` prints them.
- **Graceful shutdown** — a stop stops accepting, lets the statements already
  running finish (up to `listen.drain_timeout`) and only then closes client
  connections. Idle sessions are not waited for.
- **TLS** — `listen.tls` encrypts client connections, `backend.tls` encrypts
  the connection toward a remote MySQL, with an optional custom CA.
- **`gora status`** — a read-only unix socket exposes live state: every
  node with its role, health, lag and pool occupancy; connected clients,
  pinned sessions, statements running, cache hit ratios, and how often
  clients had to wait for a connection.

## Command line

Service verbs are bare words. Everything else is an option and always takes
two dashes; a single dash is rejected with the exact string to type instead.

```sh
gora start                    # run in the foreground (what systemd calls)
gora stop                     # SIGTERM the running instance and wait for it
gora restart                  # stop, then run in the foreground
gora reload                   # SIGHUP: re-read the configuration
gora status                   # print the state of the running instance
gora top                      # watch what it is doing, refreshed live
```

```sh
gora --init                   # install as a systemd service
gora --check-config           # validate the configuration and exit
gora --advice                 # print what the profiler has suggested
gora --init-cluster           # configure the servers into a replicating cluster
gora --promote 10.0.0.11:3306 # make that node the primary
gora status --json            # the raw snapshot, for whatever collects it
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
  address: "10.0.0.10:3306"  # the primary: where the writes go
  replicas: []               # read replicas; empty is a single server
  username: "wordpress"
  password: "change-me"
  connect_timeout: 5s

routing:
  sticky_after_write: 3s     # a session reads its own writes from the primary
  max_replica_lag: 5s        # a replica further behind stops serving reads
  health_interval: 2s

replication:
  enabled: false             # let gora configure and govern the cluster
  admin_username: "root"     # for cluster operations only, never for traffic
  admin_password: "change-me"
  user: "gora_repl"          # the account the replicas connect with
  password: "change-me"
  failover: manual           # manual | automatic
  failover_delay: 30s
  state_file: /var/lib/gora/cluster.json

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

cache:
  enabled: true
  table_prefix: auto         # or "wp_"
  autoload_options: true
  transients: true
  default_ttl: 5m
  max_entries: 10000
  max_bytes: 268435456
  max_result_bytes: 1048576
  warmup: true

profiling:
  enabled: false
  slow_query: 500ms
  report_interval: 10m
  top_queries: 20
  suggest_indexes: true
  suggest_rewrites: true
  advice_file: /var/log/gora/advice.json

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

### Rules (conf.d)

Every `*.yaml` file in `/etc/gora/conf.d/` declares what gora does with
traffic, in four sections. `{prefix}` expands to the table prefix, and RE2
is the expression syntax throughout.

```yaml
name: my-rules

# Cache these reads, drop them when these tables are written.
rules:
  - name: attribute-taxonomies
    match: "(?i)^SELECT \\* FROM {prefix}woocommerce_attribute_taxonomies"
    ttl: 30m
    invalidate_on: ["{prefix}woocommerce_attribute_taxonomies"]

# Change this statement before it runs.
rewrites:
  - name: drop-order-by-rand
    match: "(?i)\\s*ORDER\\s+BY\\s+RAND\\s*\\(\\s*\\)"
    replace: ""

# Refuse this statement. dry_run reports instead of refusing.
block:
  - name: no-truncate
    match: "(?i)^TRUNCATE"
    message: "TRUNCATE is not allowed through gora"
    dry_run: false

# Let at most two of these run at once; the rest wait a second, then fail.
throttle:
  - name: product-search
    match: "(?i)LIKE '%"
    max_concurrent: 2
    wait: 1s
```

`invalidate_on` is required on a cache rule: one without it serves stale
rows until its TTL expires, which is never what the author meant. Apply
changes with `systemctl reload gora` — connections are not dropped — and
check them first with `gora --check-config`, which compiles every section
and lists what it found.

Files are read in filename order, so `10-base.yaml` and `20-overrides.yaml`
behave the way the numbers suggest. Names must be unique within a section.

Only cache queries whose answer is the same for every visitor. Never cache
per-customer data — carts, sessions, orders.

## Operating gora

`gora status` answers *what is the state*. `gora top` answers *what is
happening*, refreshed every second, which is the question you have during an
incident:

```sh
gora top
```

```
gora 0.1.0   up 3h12m4s   14:22:07
------------------------------------------------------------------------
clients   48 connected, 2 pinned, 3 running
traffic   612.4 statements/s, 0.0 errors/s (7061233 total, 4 errors)
cache     71.3% hit ratio, 8842 entries, 41.2 MiB, 436.9 hits/s

NODE                     ROLE     STATE  LAG     CONNECTIONS  READS
10.0.0.10:3306           primary  up     -       12/100       yes
10.0.0.11:3306           replica  up     0s      34/100       yes
10.0.0.12:3306           replica  up     47s     0/100        no
```

`gora status --json` prints the same snapshot the daemon holds, for whatever
collects it: the readable report is what is derived, not the other way round.

What to look at, and what it means:

| What you see | What it means |
|---|---|
| a replica with `READS no` | it is down, further behind than `max_replica_lag`, or its lag could not be read at all |
| `LAG ?` | gora could not read the replication status — usually the proxy account is missing `REPLICATION CLIENT` |
| the primary in state `RO` | it has become read-only. Something failed over without telling gora; writes are being refused |
| pool waits above zero in `gora status` | `pool.max_open` is too small for the traffic, or statements are holding connections too long |
| a low cache hit ratio | the ratio only counts cacheable traffic, so a low one means the entries are being invalidated faster than they are used — usually a write-heavy table in an `invalidate_on` list |
| `REPLICATION STOPPED` | the replica hit an error applying something; `gora status` prints it |

The log is the other half. Every refused statement, throttled statement,
pinned session and reload is logged with the reason, and with
`profiling.enabled` the periodic report says which statements the time
actually went into.

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
| ~~M1~~ | data plane | MySQL protocol, connection pool, keepalive, per-query multiplexing, TLS |
| ~~M2~~ | cache | WordPress-aware query cache, conf.d rules, hot reload, warm-up |
| ~~M3~~ | traffic | query rewriting, firewall, per-digest throttling |
| ~~M4~~ | profiling | slow query log, aggregated report, index and rewrite advisor |
| ~~M5~~ | topology | multiple nodes, health checks, read/write split, degraded mode |
| ~~M6~~ | replication | GTID provisioning, seeding, monitoring, failover |
| ~~M7~~ | observability | `gora top`, extended status, documentation |

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
