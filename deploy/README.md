# Local infrastructure

The backing services the AGROGRAM backend needs for local development
(Phase 1, infra step). No AGROGRAM service runs here yet.

| Service | Image | What it is |
| --- | --- | --- |
| `postgres` | `postgres:16.15-alpine3.24` | PostgreSQL 16. One database and one login role per service, never shared |
| `redis` | `redis:7.4.11-alpine3.21` | Redis 7 with a password and AOF persistence |
| `kafka` | `apache/kafka:4.3.1` | Kafka, a single KRaft node (broker + controller), no ZooKeeper |
| `kafka-init` | `apache/kafka:4.3.1` | One-shot job: creates the topics, then exits 0 |
| `kafka-ui` | `ghcr.io/kafbat/kafka-ui:v1.5.0` | Kafka web UI (kafbat, the maintained fork of `provectuslabs/kafka-ui`) |
| `adminer` | `adminer:5.5.1` | Lightweight PostgreSQL web UI |

Files:

```
deploy/
├── docker-compose.yml                            # the stack
├── .env.example                                  # settings template, copy to .env
├── postgres/init/01-create-service-databases.sh  # databases + roles, first start only
└── kafka/create-topics.sh                        # topic list, run by kafka-init
```

## Prerequisites

- Docker Engine with the Compose plugin. Tested with Engine 29.8.1 and
  Compose v5.5.1. The healthchecks use `start_interval`, which needs Engine
  25 or newer.
- Permission to run `docker` without sudo (see
  [Troubleshooting](#troubleshooting)).
- `make` is optional. Every target is a plain `docker compose` command.
- About 1 GB of free RAM: Kafka and Kafka UI take about 400 MB each.

## First-time setup

```sh
cd back
cp deploy/.env.example deploy/.env
$EDITOR deploy/.env        # replace every change-me-... password
```

- `deploy/.env` is git-ignored; never commit it. `.env.example` is committed
  and holds placeholders only.
- Use only letters, digits, `-` and `_` in passwords, e.g.
  `openssl rand -hex 16`. They are embedded in connection URLs, and Compose
  would interpolate a `$`.
- The passwords have no defaults. Without `deploy/.env`, Compose stops with
  `required variable ... is missing a value: copy deploy/.env.example to
  deploy/.env and set it`.

## Start and stop

With `make`, from `back/`:

| Command | What it does |
| --- | --- |
| `make infra-up` | Starts everything and waits until every service is healthy and `kafka-init` has exited 0 (`docker compose up --wait`), then shows the status |
| `make infra-ps` | Status and health of every container, including the finished `kafka-init` |
| `make infra-logs` | Follows the logs; `make infra-logs SERVICE=kafka` for one service |
| `make infra-down` | Stops and removes the containers and the network. **Data volumes are kept** |

The targets refuse to run until `deploy/.env` exists. To use another file,
pass `INFRA_ENV_FILE=path/to/file`.

Without `make`, from the directory that contains `back/`:

```sh
docker compose -f back/deploy/docker-compose.yml up -d --wait
docker compose -f back/deploy/docker-compose.yml down
```

Compose reads `back/deploy/.env` by itself, because the project directory is
the compose file's directory. This was checked with Compose v5.5.1: a `.env`
in the current directory is ignored. `--env-file` paths, by contrast, are
resolved from the current directory.

The other `docker compose` commands in this file are run **from
`back/deploy/`**, where Compose finds both `docker-compose.yml` and `.env`
without any flags.

## Ports and addresses

Every published port binds to `BIND_ADDRESS` (`127.0.0.1` by default), so
nothing is reachable from the LAN. The host ports are deliberately
non-default; `5432`, for example, is often taken by a natively installed
PostgreSQL. Change them in `deploy/.env`.

| Service | From the host | From containers on the `agrogram` network | Variable |
| --- | --- | --- | --- |
| PostgreSQL | `127.0.0.1:15432` | `postgres:5432` | `POSTGRES_PORT` |
| Redis | `127.0.0.1:16379` | `redis:6379` | `REDIS_PORT` |
| Kafka | `localhost:19092` | `kafka:9092` | `KAFKA_PORT` |
| Kafka UI | <http://127.0.0.1:18080> | `kafka-ui:8080` | `KAFKA_UI_PORT` |
| Adminer | <http://127.0.0.1:18081> | `adminer:8080` | `ADMINER_PORT` |

The network has a fixed name, `agrogram`. A service stack added later joins it
with:

```yaml
networks:
  agrogram:
    external: true
```

## PostgreSQL

Each service owns one database. Its login role has the same name and is the
only non-superuser that can connect to it: `CONNECT` is revoked from `PUBLIC`,
so `catalog` cannot open `identity`. The role owns its database and can
create schemas and tables in it, but it cannot create databases or roles.
All of this is done by `postgres/init/01-create-service-databases.sh`.

| Service | Database = role | Password variable | URL from the host | URL from a container |
| --- | --- | --- | --- | --- |
| auth-gateway | `auth_gateway` | `AUTH_GATEWAY_DB_PASSWORD` | `postgresql://auth_gateway:PASSWORD@127.0.0.1:15432/auth_gateway` | `postgresql://auth_gateway:PASSWORD@postgres:5432/auth_gateway` |
| identity | `identity` | `IDENTITY_DB_PASSWORD` | `postgresql://identity:PASSWORD@127.0.0.1:15432/identity` | `postgresql://identity:PASSWORD@postgres:5432/identity` |
| profile-social | `profile_social` | `PROFILE_SOCIAL_DB_PASSWORD` | `postgresql://profile_social:PASSWORD@127.0.0.1:15432/profile_social` | `postgresql://profile_social:PASSWORD@postgres:5432/profile_social` |
| catalog | `catalog` | `CATALOG_DB_PASSWORD` | `postgresql://catalog:PASSWORD@127.0.0.1:15432/catalog` | `postgresql://catalog:PASSWORD@postgres:5432/catalog` |
| order-escrow | `order_escrow` | `ORDER_ESCROW_DB_PASSWORD` | `postgresql://order_escrow:PASSWORD@127.0.0.1:15432/order_escrow` | `postgresql://order_escrow:PASSWORD@postgres:5432/order_escrow` |
| *(admin only)* | superuser `postgres`, database `postgres` | `POSTGRES_SUPERUSER_PASSWORD` | `postgresql://postgres:PASSWORD@127.0.0.1:15432/postgres` | `postgresql://postgres:PASSWORD@postgres:5432/postgres` |

The superuser's name is `POSTGRES_SUPERUSER` (default `postgres`). Services
never use it.

psql:

```sh
# With a local psql client; it prompts for the password.
psql "postgresql://catalog@127.0.0.1:15432/catalog"

# Without one, use the client in the container (from back/deploy/). The image
# trusts connections over its local socket, so there is no password prompt,
# but the CONNECT rules still apply.
docker compose exec postgres psql -U catalog -d catalog
docker compose exec postgres psql -U postgres
```

### Changing passwords

The init script runs only when the `postgres-data` volume is empty. Editing a
password in `deploy/.env` later does **not** change it in the database. You
have two options:

- **Keep the data.** Run `make infra-up`, which recreates the container with
  the new values, then re-run the idempotent init script. It resets the five
  service passwords to what `deploy/.env` says:

  ```sh
  docker compose exec postgres /docker-entrypoint-initdb.d/01-create-service-databases.sh
  ```

  The script does not touch the superuser. Change its password with
  `ALTER ROLE postgres PASSWORD '...'` in `docker compose exec postgres psql
  -U postgres`.
- **Start from scratch.** Wipe the volumes (see
  [Resetting data](#resetting-data)); the next start initializes everything
  from `deploy/.env`.

### Adding a service database

1. Add the name to `databases` in `postgres/init/01-create-service-databases.sh`.
2. Pass `<NAME>_DB_PASSWORD` to the `postgres` service in `docker-compose.yml`
   and add it to `.env.example` (and to your `deploy/.env`).
3. Run `make infra-up`, then re-run the init script as shown above.

## Redis

One password (`requirepass`, the default user) and AOF persistence
(`appendonly yes`, fsync every second), in addition to Redis's default RDB
snapshots.

```sh
# With a local redis-cli.
redis-cli -h 127.0.0.1 -p 16379 --askpass
# In the container, already authenticated through REDISCLI_AUTH (from back/deploy/).
docker compose exec redis redis-cli
```

URLs: `redis://:PASSWORD@127.0.0.1:16379/0` from the host,
`redis://:PASSWORD@redis:6379/0` from a container.

## Kafka

This is a single KRaft node running Kafka 4.3.1. It has two client listeners:

| Listener | Clients | Bootstrap server |
| --- | --- | --- |
| `EXTERNAL` | Programs on the host (a service started with `go run`, an IDE, a CLI) | `localhost:19092` |
| `INTERNAL` | Containers on the `agrogram` network | `kafka:9092` |

A client must use the address that matches where it runs. From the host,
`kafka:9092` does not resolve. Inside a container, `localhost:19092` points
at the container itself.

**Topics are never created automatically** (`auto.create.topics.enable=false`).
Producing to or subscribing to an unknown topic fails with
`UNKNOWN_TOPIC_OR_PARTITION` or `Topic ... not present in metadata`. The
one-shot `kafka-init` service runs `kafka/create-topics.sh` on every `up`. It
creates the missing topics and leaves existing ones untouched, including
their partition counts and configs.

| Topic | Partitions | Retention | Key |
| --- | --- | --- | --- |
| `agrogram.identity.account.v1` | 3 | 7 days (broker default) | user_id. Planned stream: no `AccountEvent` schema yet |
| `agrogram.profilesocial.profile.v1` | 3 | 7 days | user_id |
| `agrogram.catalog.listing.v1` | 3 | 7 days | listing_id |
| `agrogram.orderescrow.transaction.v1` | 3 | 7 days | transaction_id |
| `agrogram.mediapipeline.media.v1` | 3 | 7 days | media_id |
| `agrogram.notification.request.v1` | 3 | 7 days | recipient_id |
| `agrogram.notification.deadletter.v1` | 1 | 14 days | original key |
| `agrogram.analytics.deadletter.v1` | 1 | 14 days | original key |
| `agrogram.profilesocial.deadletter.v1` | 1 | 14 days | original key |
| `agrogram.catalog.deadletter.v1` | 1 | 14 days | original key |
| `agrogram.orderescrow.deadletter.v1` | 1 | 14 days | original key |
| `agrogram.mediapipeline.deadletter.v1` | 1 | 14 days | original key |

All topics have replication factor 1. The stream partition count comes from
`KAFKA_STREAM_PARTITIONS` and applies only to topics that do not exist yet.
The list must stay in sync with `proto/events/v1/metadata.proto`. To add a
topic, add it to both files and run `make infra-up`.

CLI, from `back/deploy/`:

```sh
# Topics, partitions and configs
docker compose exec kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:9092 --describe

# Produce "key:value" lines, then read them back
docker compose exec kafka /opt/kafka/bin/kafka-console-producer.sh --bootstrap-server kafka:9092 \
  --topic agrogram.catalog.listing.v1 --reader-property parse.key=true --reader-property key.separator=:
docker compose exec kafka /opt/kafka/bin/kafka-console-consumer.sh --bootstrap-server kafka:9092 \
  --topic agrogram.catalog.listing.v1 --from-beginning --formatter-property print.key=true

# The same tools from the host network, through the EXTERNAL listener
docker run --rm --network host apache/kafka:4.3.1 \
  /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:19092 --list
```

## Web UIs

Neither UI has a login, which is one more reason to keep `BIND_ADDRESS` at
`127.0.0.1`.

- **Kafka UI**: <http://127.0.0.1:18080>. The cluster is named
  `agrogram-local` and shows the brokers, topics, messages and consumer
  groups.
- **Adminer**: <http://127.0.0.1:18081>. The login form's *System* defaults
  to MySQL, so pick **PostgreSQL**. *Server* is prefilled with `postgres`
  (the container name, not `127.0.0.1:15432`). Then enter a role, its
  password and its database. A link that preselects PostgreSQL:
  <http://127.0.0.1:18081/?pgsql=postgres&username=catalog&db=catalog>.

## Checking health

```sh
make infra-ps          # or, from back/deploy/: docker compose ps -a
```

A healthy stack has five containers `Up ... (healthy)` and `kafka-init`
`Exited (0)`:

```
SERVICE      STATUS
adminer      Up 2 minutes (healthy)
kafka        Up 2 minutes (healthy)
kafka-init   Exited (0) 2 minutes ago
kafka-ui     Up 2 minutes (healthy)
postgres     Up 2 minutes (healthy)
redis        Up 2 minutes (healthy)
```

| Service | Healthcheck |
| --- | --- |
| `postgres` | `pg_isready` over TCP. It passes only after the first-start init script has finished, because until then the server listens on its socket only |
| `redis` | `redis-cli ping` with the password (`REDISCLI_AUTH`) |
| `kafka` | `kafka-broker-api-versions.sh` against the internal listener |
| `kafka-ui` | `/actuator/health` reports `UP` |
| `adminer` | `GET /` answers |
| `kafka-init` | No healthcheck: it must end as `Exited (0)`. Its log lists each topic as `created` or `exists`: `make infra-logs SERVICE=kafka-init` |

`make infra-up` fails, and prints the status, if a service does not become
healthy or `kafka-init` fails.

## Resetting data

Data lives in three named volumes: `agrogram_postgres-data`,
`agrogram_redis-data` and `agrogram_kafka-data`. `make infra-down` and
`docker compose down` keep them. To delete everything:

```sh
docker compose down -v     # from back/deploy/; deletes ALL local data
```

Wipe the volumes when:

- you changed `POSTGRES_SUPERUSER`, `POSTGRES_SUPERUSER_PASSWORD` or a
  `*_DB_PASSWORD` and do not want to update the roles by hand. The init
  scripts only run on an empty volume (see
  [Changing passwords](#changing-passwords));
- you changed `KAFKA_CLUSTER_ID`. The broker refuses to start on a volume
  formatted with another cluster id;
- you want `KAFKA_STREAM_PARTITIONS` to apply to existing topics. The
  alternative is `kafka-topics.sh --alter --partitions N`, which can only
  increase the count;
- you just want a clean slate.

To reset only one volume, run `docker compose down`, then for example
`docker volume rm agrogram_kafka-data`, then `make infra-up`.

## Troubleshooting

- **`port is already allocated` / `address already in use`.** Another
  process holds the host port. Find it with `ss -ltnp 'sport = :15432'`, then
  change the port in `deploy/.env` (e.g. `POSTGRES_PORT=25432`) and run
  `make infra-up`.
- **`permission denied while trying to connect to the Docker daemon socket`.**
  Your user is not in the `docker` group, or your session started before you
  were added. Run `sudo usermod -aG docker "$USER"` and log out and back in.
  To use the group in the current terminal only, run `newgrp docker`, or
  wrap a single command in `sg docker -c 'make infra-up'`.
- **`required variable ... is missing a value`.** `deploy/.env` is missing or
  a variable in it is empty: `cp deploy/.env.example deploy/.env`. Variables
  exported in your shell take precedence over `deploy/.env`; check them with
  `env | grep -E '_PORT|PASSWORD|KAFKA_'`.
- **A service cannot log in to PostgreSQL after a password change.** See
  [Changing passwords](#changing-passwords).
- **Kafka restarts in a loop after `KAFKA_CLUSTER_ID` changed.** The volume
  was formatted with the old id. Put the old id back, or wipe
  `agrogram_kafka-data`.
- **Clients on another machine cannot reach Kafka.** Setting `BIND_ADDRESS`
  to `0.0.0.0` publishes the port, but the broker still tells clients to
  connect to `localhost`. Set `KAFKA_EXTERNAL_HOST` to this machine's LAN IP.
- **`kafka-init` exited with a non-zero code.** Read
  `make infra-logs SERVICE=kafka-init`. A common cause is a non-numeric
  `KAFKA_STREAM_PARTITIONS`.
- **Redis logs `WARNING Memory overcommit must be enabled!`.** This is a host
  kernel setting and harmless for local development. To silence it, run
  `sudo sysctl vm.overcommit_memory=1`.
