# identity

Go service that owns **accounts**: the role set, e-ijara **farmer verification**
and the **plan**. Contract: [`proto/identity/v1/identity.proto`](../../proto/identity/v1/identity.proto)
(`agrogram.identity.v1.IdentityService`). Conventions (errors, metadata,
validation, events) are in [back/README.md](../../README.md).

| RPC | Who may call | What it does |
| --- | --- | --- |
| `CreateAccount` | internal, `x-agrogram-caller: auth-gateway` only | New account: ACTIVE, `{ROLE_RESELLER}`, plan DEFAULT, version 1. Idempotent on `request_id` |
| `GetAccount` | the user (empty `user_id`); `ROLE_ADMIN` or any internal caller for others | Roles, status, plan, `is_premium`, `premium_until`, `is_verified_farmer`, `farmer_verification_status`, version |
| `SubmitFarmerVerification` | a signed-in user with an ACTIVE account | e-ijara verification (stubbed); grants `ROLE_FARMER` when VERIFIED |
| `GetFarmerVerification` | same rules as `GetAccount` | The latest verification, or NOT_FOUND |
| `SetAccountPremium` | internal, `x-agrogram-caller: order-escrow` only | Premium on (plan PREMIUM, no end date) or off (plan DEFAULT); version + 1 and `AccountPremiumChanged`. Asking for the value the account already has is a no-op (no version bump, no event). NOT_FOUND for an unknown account |

Every error is a gRPC status with exactly one `agrogram.common.v1.Error`
(domain `identity`) in its details; codes follow the `ErrorReason` table.
Request validation is the protovalidate annotations of the proto
(INVALID_ARGUMENT / `VALIDATION_FAILED` with `field_violations`).

## Farmer verification flow

1. The caller must be signed in (UNAUTHENTICATED), have an account
   (NOT_FOUND) that is ACTIVE (PERMISSION_DENIED / `ACCOUNT_DISABLED`).
2. A retry with the same `(user, request_id)` returns the original
   verification as it stands now (no new check, no rate-limit hit).
3. Already holding `ROLE_FARMER` → ALREADY_EXISTS.
4. Rate limit per user, fixed window in Redis (default 5 per hour) →
   RESOURCE_EXHAUSTED / `RATE_LIMITED` with `metadata.retry_after_seconds`.
   Fails open (warning log) when Redis is down or not configured.
5. Contract number format check (trimmed, stored upper case) → INVALID_ARGUMENT.
6. Held by another account's VERIFIED verification → ALREADY_EXISTS /
   `LEASE_CONTRACT_ALREADY_CLAIMED`; nothing is stored.
7. Stored as PENDING, then e-ijara is asked inline with
   `IDENTITY_EIJARA_TIMEOUT`. The answer is applied in one transaction:
   status, `ROLE_FARMER` + account version bump (VERIFIED), and in the
   outbox `FarmerVerified` / `FarmerVerificationRejected` (+
   `AccountRoleChanged` when the role was granted) and a notification
   request. REJECTED is a normal response
   (`status: REJECTED`, `rejection_reason`), not an RPC error. If a partial
   unique index (one VERIFIED verification per contract) trips at this point,
   the verification is stored REJECTED / `LEASE_CONTRACT_ALREADY_CLAIMED`.
8. No answer in time → the response is PENDING; the background worker
   retries (`FOR UPDATE SKIP LOCKED`, exponential backoff, attempt count and
   last error recorded). Clients poll `GetFarmerVerification`.
9. The same worker expires VERIFIED verifications whose `valid_until` is
   before today (Asia/Tashkent), revokes `ROLE_FARMER` unless another
   VERIFIED verification remains, and requests a notification; the same
   transaction writes `FarmerVerificationExpired` (+ `AccountRoleChanged`
   when the role was revoked).

## Events

Transactional outbox (`outbox` table, written in the state change's
transaction) plus a relay goroutine that publishes with franz-go (idempotent
producer, acks=all, at-least-once, batches locked with `FOR UPDATE SKIP
LOCKED`, exponential backoff). Kafka being down never stops serving; rows
wait in the outbox. Published rows are purged after `IDENTITY_OUTBOX_RETENTION`.

| Topic | Key | Envelope | When |
| --- | --- | --- | --- |
| `agrogram.identity.account.v1` | user id | `AccountEvent{metadata, account, event}` ([`events/v1/account.proto`](../../proto/events/v1/account.proto)) | see the table below |
| `agrogram.notification.request.v1` | recipient user id | `NotificationRequestEvent{metadata, notification_requested}` | verification VERIFIED (`account.farmer_verified`), REJECTED (`account.farmer_verification_rejected`), EXPIRED (`account.farmer_verification_expired`) |

### Account stream

Every account change writes its `AccountEvent` to the outbox in the same
transaction, so the change and its event commit or roll back together:

| Event (`oneof` case) | Published by | When | `account.version` |
| --- | --- | --- | --- |
| `AccountCreated` | `CreateAccount` | a new account (not on an idempotent retry) | 1 |
| `FarmerVerified` | `SubmitFarmerVerification` or the worker's retry (`applyDecision`) | e-ijara answered VERIFIED | unchanged |
| `FarmerVerificationRejected` | same | e-ijara answered REJECTED, or the partial unique index turned it into `LEASE_CONTRACT_ALREADY_CLAIMED` | unchanged |
| `AccountRoleChanged` | same, right after `FarmerVerified` | `ROLE_FARMER` was actually granted (not when the account already held it) | + 1 |
| `FarmerVerificationExpired` | the worker (`ExpireVerifications`) | a VERIFIED lease ended (the day after `valid_until`, Asia/Tashkent) | unchanged |
| `AccountRoleChanged` | same, right after `FarmerVerificationExpired` | `ROLE_FARMER` was actually revoked (not while another VERIFIED lease remains) | + 1 |
| `AccountPremiumChanged` | `SetAccountPremium` | Premium actually turned on or off | + 1 |

- **Snapshot** (`account`): id, roles (sorted), `is_verified_farmer`,
  `is_premium` (evaluated at `occurred_at`), `premium_until` (only while
  Premium is active and has an end), version, created/updated time. It is the
  account right after its own event, so in a pair such as `FarmerVerified` +
  `AccountRoleChanged` the first carries the old version and roles, the second
  the new ones. The farmer verification status is not in the snapshot (the
  account version does not cover it); it travels in the `FarmerVerif*` events.
- **Details**: `AccountRoleChanged` carries old and new role sets
  (`previous_roles` → `roles`), `granted_roles` / `revoked_roles` and the
  causing `farmer_verification_id`; `FarmerVerified` the verification id and
  lease dates; `FarmerVerificationRejected` the id and `rejection_reason`;
  `FarmerVerificationExpired` the id and the lease's last day;
  `AccountPremiumChanged` the new `is_premium`. Contract numbers are not
  published on this stream.
- **No spurious events**: `AccountRoleChanged` only when the role set differs
  (and the envelope's protovalidate rule refuses an empty change);
  `AccountPremiumChanged` only when `is_premium` flips.

### Metadata and headers

UUIDv7 `event_id`; `event_type` = the concrete message's full name (e.g.
`agrogram.events.v1.AccountRoleChanged`, `agrogram.events.v1.NotificationRequested`);
producer `identity`; aggregate USER / user id; `aggregate_version` = the
snapshot version (0 on the notification stream); `occurred_at` = the time of
the state change; correlation id = `x-request-id` (a fresh UUIDv7 for worker
jobs and for calls without one); `traceparent` / `tracestate` when present;
`causation_id` empty (every identity event is caused by a command or by
identity's own worker). Actor: the signed-in user; for `AccountCreated` the new
account's user (auth-gateway only relays the sign-in); for an internal call
without user headers the calling service (e.g. `{service: "order-escrow"}`);
for worker jobs `{service: "identity"}`. Every envelope is validated with
protovalidate before it is enqueued. Record headers: `content-type`,
`event-type`, `event-id`, `x-request-id`, `traceparent`/`tracestate`.

Notification requests: category ACCOUNT, params `lease_contract_number`
(+ `rejection_reason`), subject `{RESOURCE_TYPE_FARMER_VERIFICATION, id}`,
channels empty.

## Layout

```
services/identity/
├── cmd/identity/            main: serve (default) | migrate up|down|status | healthcheck
├── internal/
│   ├── apperr/              Error type -> gRPC status with one common.v1.Error
│   ├── auth/                x-agrogram-* metadata -> Principal; authorization rules
│   ├── config/              IDENTITY_* environment variables
│   ├── domain/              Account, Verification, Lease, Decision; Asia/Tashkent dates
│   ├── eijara/              e-ijara checker STUB and contract number format
│   ├── events/              AccountEvent and NotificationRequestEvent builders, outbox relay (franz-go)
│   ├── grpcserver/          handler, interceptors (recovery, principal, logging, validation)
│   ├── health/              Postgres readiness -> gRPC health + /healthz, /readyz
│   ├── log/                 slog (JSON or text)
│   ├── ratelimit/           Redis fixed-window limiter
│   ├── repository/postgres/ pgx v5 repository, outbox store, integration tests
│   └── service/             use cases, verification flow, background worker
├── migrations/              goose SQL migrations (embedded)
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
| `IDENTITY_REDIS_URL` | empty | Redis URL for the rate limit; empty disables it (warning) |
| `IDENTITY_KAFKA_BROKERS` | empty | Comma-separated brokers; empty disables the relay (events wait in the outbox) |
| `IDENTITY_KAFKA_CLIENT_ID` | `identity` | Kafka client id |
| `IDENTITY_GRPC_ADDR` | `:50051` | gRPC listen address |
| `IDENTITY_HTTP_ADDR` | `:8081` | `/healthz`, `/readyz` listen address (also used by `identity healthcheck`) |
| `IDENTITY_GRPC_REFLECTION` | `false` | gRPC server reflection (grpcurl); local only |
| `IDENTITY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `IDENTITY_LOG_FORMAT` | `json` | `json` or `text` |
| `IDENTITY_EIJARA_TIMEOUT` | `3s` | Inline e-ijara call timeout |
| `IDENTITY_EIJARA_STUB_DELAY` | `0s` | Simulated e-ijara latency (stub) |
| `IDENTITY_VERIFICATION_RATE_LIMIT` | `5` | Submissions per user per window |
| `IDENTITY_VERIFICATION_RATE_WINDOW` | `1h` | Rate-limit window |
| `IDENTITY_WORKER_INTERVAL` | `30s` | Worker period (retries and expiry) |
| `IDENTITY_WORKER_BATCH_SIZE` | `50` | Rows per worker pass |
| `IDENTITY_VERIFICATION_RETRY_BACKOFF` | `30s` | First retry delay, doubled per attempt |
| `IDENTITY_VERIFICATION_RETRY_BACKOFF_MAX` | `1h` | Retry delay cap |
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
make test                     # unit tests, no infrastructure (fakes, pgxmock, miniredis, kfake)
make test-race                # the same with -race (needs a C compiler)
make lint                     # go vet (both build tags) + gofmt check
make test-integration         # -tags=integration against IDENTITY_TEST_DATABASE_URL
```

The integration tests create a random schema (`identity_test_<hex>`), run
the migrations into it through `search_path`, exercise the real repository
(idempotency, the partial unique index, role grant/revoke, plan changes,
outbox) and the service end to end (including the order of the account and
notification events in the outbox, and `SetAccountPremium`), roll the
migrations back and forward, and drop the schema. The `identity` role owns
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
grpcurl -plaintext -H "x-agrogram-user-id: $USER_ID" -H 'x-agrogram-roles: RESELLER' \
  -H "x-request-id: $(uuid)" -d '{}' $ADDR agrogram.identity.v1.IdentityService/GetAccount

# farmer verification: VERIFIED (stub), then ALREADY_EXISTS on a second submit
grpcurl -plaintext -H "x-agrogram-user-id: $USER_ID" \
  -d "{\"request_id\":\"$(uuid)\",\"lease_contract_number\":\"AB-2026/0001\"}" \
  $ADDR agrogram.identity.v1.IdentityService/SubmitFarmerVerification
grpcurl -plaintext -H "x-agrogram-user-id: $USER_ID" -d '{}' \
  $ADDR agrogram.identity.v1.IdentityService/GetFarmerVerification

# REJECTED path (stub hook), on a fresh account: contract numbers starting with INVALID
#   ... -d "{\"request_id\":\"$(uuid)\",\"lease_contract_number\":\"INVALID-1\"}" ...

# Premium on (order-escrow only); repeat it: same account, same version, no event
grpcurl -plaintext -H 'x-agrogram-caller: order-escrow' \
  -d "{\"user_id\":\"$USER_ID\",\"is_premium\":true}" \
  $ADDR agrogram.identity.v1.IdentityService/SetAccountPremium

# errors carry agrogram.common.v1.Error in the details
grpcurl -plaintext -d '{}' $ADDR agrogram.identity.v1.IdentityService/GetAccount   # UNAUTHENTICATED

grpcurl -plaintext -d '{"service":"agrogram.identity.v1.IdentityService"}' $ADDR grpc.health.v1.Health/Check
```

The account events appear on `agrogram.identity.account.v1` and the
notification requests on `agrogram.notification.request.v1` in Kafka UI
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

then call with `-H 'x-agrogram-user-id: <user uuid>' -H 'x-agrogram-roles: RESELLER,ADMIN'`.

## Stubs

`// STUB:` marks every stand-in for the e-ijara integration:

| Where | Stands in for |
| --- | --- |
| `internal/eijara/eijara.go` (package doc) | The whole e-ijara client: `Stub` answers locally, never calls out |
| `internal/eijara/eijara.go` (`contractNumberPattern`) | The real contract number format (unknown): `^[A-Za-z0-9][A-Za-z0-9/-]{2,63}$` after trimming, compared upper case |
| `internal/eijara/eijara.go` (`Stub.Check`, delay) | e-ijara latency: `IDENTITY_EIJARA_STUB_DELAY` |
| `internal/eijara/eijara.go` (`Stub.Check`, hook) | Test hook: numbers starting with `INVALID` → REJECTED / `LEASE_CONTRACT_INVALID` |
| `internal/eijara/eijara.go` (`Stub.Check`, answer) | Every other number: VERIFIED, lease from today for one year (Asia/Tashkent); location and area unset |
| `cmd/identity/serve.go` | Wiring: `eijara.Stub` instead of a real client |

## TODOs

- `TODO(billing)` (`internal/service/accounts.go`, `internal/auth/auth.go`):
  purchase and renewal of Premium are not designed. `SetAccountPremium` is the
  on/off switch they will use; order-escrow is its only allowed caller until
  billing has a service of its own.
- `TODO(e-ijara)` (`internal/grpcserver/handler.go`): lease location and area.

## Known gaps

- The proto has the five RPCs above. Account update/delete and role
  management have no RPCs, so they are not implemented. Roles, plan and
  premium are exposed through `GetAccount`; `ROLE_FARMER` changes only through
  verification and expiry; Premium only through `SetAccountPremium`.
- `premium_until` is not managed: `SetAccountPremium` clears it, and nothing in
  identity sets it. A `premium_until` set by hand in SQL ends Premium silently:
  no `AccountPremiumChanged` is published when it passes.
- Account status (BLOCKED, DELETED) has no RPC and no event yet;
  `SetAccountPremium` does not check the status.
- e-ijara failures never surface as `DEPENDENCY_UNAVAILABLE` (the RPC comment
  lists it): by design the verification is returned PENDING and retried.
- `LEASE_CONTRACT_INVALID` arrives as a REJECTED verification, not as an RPC
  error, although the RPC comment lists it under "Errors".
- Internal callers are recognized by the `x-agrogram-caller` header alone; the
  edge must strip it from client traffic (mTLS / service identity later).
