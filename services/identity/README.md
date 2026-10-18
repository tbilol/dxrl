# identity

Go service that owns **accounts** and their **role set**. Contract:
[`proto/identity/v1/identity.proto`](../../proto/identity/v1/identity.proto)
(`agrogram.identity.v1.IdentityService`). Conventions (errors, metadata,
validation, events) are in [back/README.md](../../README.md).

AGROGRAM is listings-only with one flat tier: there is no plan or Premium,
no e-ijara farmer verification, and posting needs no role (any signed-in
user may post on either board). **ADMIN** (staff) is the only role.

| RPC | Who may call | What it does |
| --- | --- | --- |
| `CreateAccount` | internal, `x-agrogram-caller: auth-gateway` only | New account: ACTIVE, no roles, version 1, and `AccountCreated` in the same transaction. Idempotent on `request_id` |
| `GetAccount` | the user (empty `user_id`); `ROLE_ADMIN` or any internal caller for others | Roles, status, version, timestamps |

Every error is a gRPC status with exactly one `agrogram.common.v1.Error`
(domain `identity`) in its details; codes follow the `ErrorReason` table.
Request validation is the protovalidate annotations of the proto
(INVALID_ARGUMENT / `VALIDATION_FAILED` with `field_violations`).

## Events

Transactional outbox (`outbox` table, written in the state change's
transaction) plus a relay goroutine that publishes with franz-go (idempotent
producer, acks=all, at-least-once, batches locked with `FOR UPDATE SKIP
LOCKED`, exponential backoff). Kafka being down never stops serving; rows
wait in the outbox. Published rows are purged after `IDENTITY_OUTBOX_RETENTION`.

| Topic | Key | Envelope | When |
| --- | --- | --- | --- |
| `agrogram.identity.account.v1` | user id | `AccountEvent{metadata, account, event}` ([`events/v1/account.proto`](../../proto/events/v1/account.proto)) | see the table below |

### Account stream

Every account change writes its `AccountEvent` to the outbox in the same
transaction, so the change and its event commit or roll back together:

| Event (`oneof` case) | Published by | When | `account.version` |
| --- | --- | --- | --- |
| `AccountCreated` | `CreateAccount` | a new account (not on an idempotent retry) | 1 |

- **Snapshot** (`account`): id, roles (sorted, distinct; empty for a new
  account), version, created/updated time.
- `AccountRoleChanged` is still a case of the stream, for granting ADMIN
  later; nothing publishes it today (role management has no RPC).

### Metadata and headers

UUIDv7 `event_id`; `event_type` = the concrete message's full name
(`agrogram.events.v1.AccountCreated`); producer `identity`; aggregate USER /
user id; `aggregate_version` = the snapshot version; `occurred_at` = the time
of the state change; correlation id = `x-request-id` (a fresh UUIDv7 for calls
without one); `traceparent` / `tracestate` when present; `causation_id` empty.
Actor: for `AccountCreated` the new account's user (auth-gateway only relays
the sign-in). Every envelope is validated with
protovalidate before it is enqueued. Record headers: `content-type`,
`event-type`, `event-id`, `x-request-id`, `traceparent`/`tracestate`.

## Layout

```
services/identity/
├── cmd/identity/            main: serve (default) | migrate up|down|status | healthcheck
├── internal/
│   ├── apperr/              Error type -> gRPC status with one common.v1.Error
│   ├── auth/                x-agrogram-* metadata -> Principal; authorization rules
│   ├── config/              IDENTITY_* environment variables
│   ├── domain/              Account
│   ├── events/              AccountEvent builder, outbox relay (franz-go)
│   ├── grpcserver/          handler, interceptors (recovery, principal, logging, validation)
│   ├── health/              Postgres readiness -> gRPC health + /healthz, /readyz
│   ├── log/                 slog (JSON or text)
│   ├── repository/postgres/ pgx v5 repository, outbox store, integration tests
│   └── service/             use cases (accounts)
├── migrations/              goose SQL migrations (embedded): 00001 init, 00002 drops
│                            farmer_verifications, the plan columns and the RESELLER/FARMER roles
├── Dockerfile               multi-stage, context back/, distroless nonroot
├── Dockerfile.dockerignore  keeps the context to gen/go + services/identity
├── compose.yaml             identity + identity-migrate on top of deploy/docker-compose.yml
├── Makefile                 `make help`
└── .env.example             settings for running on the host
```

## Configuration

Environment variables only (`IDENTITY_` prefix). Missing or invalid values
stop the start-up with one error that lists all of them.

| Variable | Default | Meaning |
| --- | --- | --- |
| `IDENTITY_DATABASE_URL` | **required** | Postgres URL (role and database `identity`) |
| `IDENTITY_DATABASE_MAX_CONNS` | `10` | pgx pool size |
| `IDENTITY_KAFKA_BROKERS` | empty | Comma-separated brokers; empty disables the relay (events wait in the outbox) |
| `IDENTITY_KAFKA_CLIENT_ID` | `identity` | Kafka client id |
| `IDENTITY_GRPC_ADDR` | `:50051` | gRPC listen address |
| `IDENTITY_HTTP_ADDR` | `:8081` | `/healthz`, `/readyz` listen address (also used by `identity healthcheck`) |
| `IDENTITY_GRPC_REFLECTION` | `false` | gRPC server reflection (grpcurl); local only |
| `IDENTITY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `IDENTITY_LOG_FORMAT` | `json` | `json` or `text` |
| `IDENTITY_OUTBOX_INTERVAL` | `1s` | Relay poll interval when idle |
| `IDENTITY_OUTBOX_BATCH_SIZE` | `100` | Messages per relay batch |
| `IDENTITY_OUTBOX_MAX_BACKOFF` | `30s` | Relay backoff cap while Kafka/Postgres fail |
| `IDENTITY_OUTBOX_RETENTION` | `168h` | Published outbox rows older than this are deleted (`0` keeps them) |
| `IDENTITY_HEALTH_INTERVAL` | `5s` | Postgres ping period (readiness) |
| `IDENTITY_SHUTDOWN_TIMEOUT` | `20s` | Graceful shutdown budget |
| `IDENTITY_TEST_DATABASE_URL` | empty | Integration tests only; unset = skipped |

Ports: in the container gRPC `:50051` and HTTP `:8081`; on the host (both
`.env.example` and `compose.yaml`) `127.0.0.1:15051` and `127.0.0.1:18051`.

## Commands

Everything runs from `back/services/identity` in your terminal. Go targets
need Go (the go.mod asks for 1.26+, toolchain go1.27.1; `GOTOOLCHAIN=auto`
downloads it). The local infrastructure is `make infra-up` in `back/`
(see [deploy/README.md](../../deploy/README.md)).

### 1. Code generation

```sh
make gen     # downloads buf 1.73.0 to ~/.cache/agrogram/bin (sha256-checked),
             # runs `buf generate` in back/ and writes back/gen/go/go.mod
```

`make gen` is a thin call into the shared tooling, `back/tools/codegen.sh`:
the pinned buf version, the download-and-verify logic and the `gen/go/go.mod`
template live there once, for every service. `make proto-gen-local` in `back/`
runs the same script, and `make proto-gen` (the Docker path) now writes
`gen/go/go.mod` too, so neither leaves the Go module missing — `buf.gen.yaml`
has `clean: true`, which is why it has to be rewritten on every run.
`back/gen/` is git-ignored. `export BUF_TOKEN=...` avoids anonymous BSR
rate limits.

### 2. Settings and migrations (host)

```sh
cp .env.example .env          # passwords match back/deploy/.env.example placeholders
make migrate-up               # goose, embedded SQL; also: migrate-status, migrate-down
```

### 3. Run locally

```sh
make run                      # loads .env; gRPC 127.0.0.1:15051, HTTP 127.0.0.1:18051
curl -i http://127.0.0.1:18051/readyz
```

### 4. Run in Docker Compose (with the infra stack)

```sh
make compose-up               # builds the image, runs identity-migrate, waits until healthy
make compose-logs
make compose-down             # removes only the identity containers
```

Same thing without make, from the directory that contains `back/`:

```sh
docker compose -f back/deploy/docker-compose.yml -f back/services/identity/compose.yaml \
  up -d --build --wait identity
```

Stop either `make run` or the container before starting the other: both use
host ports 15051/18051. `make infra-down` in `back/` also removes the identity
containers (`--remove-orphans`).

### 5. Tests

```sh
make test                     # unit tests, no infrastructure (fakes, pgxmock, kfake)
make test-race                # the same with -race (needs a C compiler)
make lint                     # go vet (both build tags) + gofmt check
make test-integration         # -tags=integration against IDENTITY_TEST_DATABASE_URL
```

The integration tests create a random schema (`identity_test_<hex>`), run
the migrations into it through `search_path`, exercise the real repository
(idempotency, outbox) and the service end to end (`AccountCreated` in the
outbox), migration 00002 down and up (legacy roles removed, FARMER rejected
afterwards), roll the migrations back and forward, and drop the schema. The `identity` role owns
its database, so it may create schemas.

### 6. Smoke test with grpcurl

With `IDENTITY_GRPC_REFLECTION=true` (the default in `.env.example` and
compose.yaml):

```sh
ADDR=127.0.0.1:15051
uuid() { cat /proc/sys/kernel/random/uuid; }   # or uuidgen

grpcurl -plaintext $ADDR list agrogram.identity.v1.IdentityService

# auth-gateway creates the account (repeat with the same request_id: same account)
grpcurl -plaintext -H 'x-agrogram-caller: auth-gateway' \
  -d "{\"request_id\":\"$(uuid)\"}" $ADDR agrogram.identity.v1.IdentityService/CreateAccount
USER_ID=<account.id from the response>

# the user reads the account (the edge normally sets these headers)
grpcurl -plaintext -H "x-agrogram-user-id: $USER_ID" \
  -H "x-request-id: $(uuid)" -d '{}' $ADDR agrogram.identity.v1.IdentityService/GetAccount

# errors carry agrogram.common.v1.Error in the details
grpcurl -plaintext -d '{}' $ADDR agrogram.identity.v1.IdentityService/GetAccount   # UNAUTHENTICATED

grpcurl -plaintext -d '{"service":"agrogram.identity.v1.IdentityService"}' $ADDR grpc.health.v1.Health/Check
```

The account events appear on `agrogram.identity.account.v1` in Kafka UI
(http://127.0.0.1:18080); values are Protobuf binary, the `event-type` header
names the event.

### Making a local admin

Roles are read from the `x-agrogram-roles` header (a token-time snapshot);
the database is where auth-gateway will get them from. For local testing:

```sql
-- psql "postgresql://identity@127.0.0.1:15432/identity"
INSERT INTO account_roles (account_id, role, granted_at)
VALUES ('<user uuid>', 'ADMIN', now()) ON CONFLICT DO NOTHING;
UPDATE accounts SET version = version + 1, updated_at = now() WHERE id = '<user uuid>';
```

then call with `-H 'x-agrogram-user-id: <user uuid>' -H 'x-agrogram-roles: ADMIN'`.

## Stubs

None.

## Known gaps

- The proto has the two RPCs above. Account update/delete and role
  management have no RPCs, so they are not implemented; ADMIN is granted by
  hand (above).
- Account status (BLOCKED, DELETED) has no RPC and no event yet.
- The comments of `migrations/00001_init.sql` still describe verification
  and plans, which 00002 drops; applied migrations are not edited.
- Internal callers are recognized by the `x-agrogram-caller` header alone; the
  edge must strip it from client traffic (mTLS / service identity later).
