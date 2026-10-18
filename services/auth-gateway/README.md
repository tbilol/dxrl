# auth-gateway

Go service that owns **sign-in**: credentials (Google and Apple links, argon2id
password hashes), **phone SMS challenges**, **sessions** and the **tokens** the
api-gateway checks, plus the **JWKS** it checks them with. Contract:
[`proto/authgateway/v1/auth_gateway.proto`](../../proto/authgateway/v1/auth_gateway.proto)
(`agrogram.authgateway.v1.AuthGatewayService`). Conventions (errors, metadata,
validation, events) are in [back/README.md](../../README.md).

Accounts, roles and the plan are **not** here: they belong to
[identity](../identity), which auth-gateway calls when it creates an account
and every time it issues or refreshes tokens.

| RPC | Who may call | What it does |
| --- | --- | --- |
| `SignIn` | anyone | Google, Apple or phone + password; opens a session. First Google/Apple sign-in creates the account (`is_new_account`) |
| `RequestPhoneCode` | anyone | One-time SMS code for `SIGN_UP`, `RESET_PASSWORD` or `LINK_PHONE` (delivery is a STUB) |
| `SignUpWithPhone` | anyone | Phone + password account from a verified code, and signs it in. Idempotent on `request_id` |
| `ResetPassword` | anyone | New password from a verified code; ends every session of the account |
| `LinkSignInMethod` | the signed-in user | Adds Google, Apple or phone + password to the caller's account |
| `UnlinkSignInMethod` | the signed-in user | Removes one; the last one cannot go |
| `ListSignInMethods` | the signed-in user | The caller's methods, with masked display hints |
| `RefreshToken` | anyone holding the token | Rotates the pair; re-reads roles from identity |
| `SignOut` | the signed-in user | Ends the current session or every session |
| `VerifyToken` | internal, `x-agrogram-caller: api-gateway` | Signature, `iss`, `aud`, expiry and session; returns `common.v1.Principal` |
| `GetJwks` | anyone | The public signing keys, as a JWKS document |

Every error is a gRPC status with exactly one `agrogram.common.v1.Error`
(domain `auth-gateway`) in its details; codes follow the `ErrorReason` table.
Request validation is the protovalidate annotations of the proto
(INVALID_ARGUMENT / `VALIDATION_FAILED` with `field_violations`).

## Tokens

**Access token** — a signed JWT, default 15 minutes. Claims are exactly what
the edge turns into `x-agrogram-*` metadata (`common/v1/auth.proto`):

| Claim | Value |
| --- | --- |
| `iss`, `aud` | `AUTH_GATEWAY_JWT_ISSUER` / `_AUDIENCE`; both are checked on verification |
| `sub` | user id (UUID) |
| `sid` | session id (UUID) → `x-agrogram-session-id` |
| `roles` | Role names without `ROLE_`, e.g. `["ADMIN"]` (omitted when none: posting needs no role) → `x-agrogram-roles` |
| `iat`, `exp`, `jti` | issued at, expiry, token id |

There is no plan claim (one flat tier). Tokens issued before the
listings-only pivot carry a `premium` claim, which verification ignores.

Signed with **ES256** (P-256) by default, because the proto's `JsonWebKey`
documents `kty: "EC"`, `crv: "P-256"`, `alg: "ES256"` and both Envoy and Kong
verify it. An **RSA** key is detected from the PEM and used with RS256 (the
message already carries `n`/`e`). EdDSA is deliberately not supported. `kid` is
the **RFC 7638 JWK thumbprint** (base64url SHA-256 of the canonical JWK), so it
is stable across restarts and computable from the public key alone.

The key comes from `AUTH_GATEWAY_JWT_PRIVATE_KEY_FILE` (a PEM path, local
development) or `AUTH_GATEWAY_JWT_PRIVATE_KEY` (PEM or base64 PEM, containers
and secret managers). `AUTH_GATEWAY_JWT_PREVIOUS_PUBLIC_KEYS` keeps retired
public keys in `GetJwks` and in verification during a rotation. **Without a key
the service refuses to start** — it never generates one silently, because that
would invalidate every token in the field on each restart and give two replicas
different keys. `make dev-key` writes one into `secrets/` (git-ignored, and
excluded from the Docker build context).

**Refresh token** — opaque, 32 random bytes, base64url, default 30 days. Only
its SHA-256 digest is stored, so a database dump cannot be replayed. Every
refresh rotates it; presenting an already rotated token revokes the **whole
session** (TOKEN_REVOKED), which is the proto's stated behaviour and makes a
stolen token usable at most once. Sessions carry the `ClientInfo` of the
sign-in.

## Flows

**Sign-in.** Google/Apple: the ID token is verified (STUB, see below), the
subject is looked up; a subject with no credential is a first sign-in, so
identity's `CreateAccount` is called with a **UUIDv5 request id derived from
the sign-in identity** (`GOOGLE:<sub>`), which is exactly the idempotency
identity's proto asks for — a retried or parallel first sign-in can never
create two accounts. Otherwise `GetAccount` supplies live roles. An
account that is BLOCKED or DELETED gets PERMISSION_DENIED /
`ACCOUNT_DISABLED`; identity being unreachable or slow gets UNAVAILABLE /
`DEPENDENCY_UNAVAILABLE` after `AUTH_GATEWAY_IDENTITY_TIMEOUT` (never a hang,
never an endless retry). Phone: the argon2id hash is verified; every failure
answers INVALID_CREDENTIALS and counts against the per-number limit.

**Phone codes.** `RequestPhoneCode` enforces the purpose (SIGN_UP and
LINK_PHONE need a free number → PHONE_ALREADY_REGISTERED; RESET_PASSWORD needs
a registered one → NOT_FOUND), the per-number rate limit and the resend window
(default 60s → RESOURCE_EXHAUSTED with `retry_after_seconds`), then stores a
challenge (id, SHA-256 of id+code, purpose, ~10 min expiry, attempt counter).
`SignUpWithPhone`, `ResetPassword` and the phone variant of `LinkSignInMethod`
consume a matching, unexpired, unconsumed challenge with attempts left; a wrong
code counts an attempt, and everything wrong answers OTP_INVALID.
`ResetPassword` also revokes every session of the account.

**Link/unlink.** One credential per `(user, method)` and a globally unique
`(method, subject)`, both backed by unique constraints →
PROVIDER_ALREADY_LINKED / PHONE_ALREADY_REGISTERED. Removing the last method is
FAILED_PRECONDITION / `LAST_SIGN_IN_METHOD`. `display_hint` is masked:
`a***@gmail.com`, `+998 90 *** ** 67`.

**Caller identity.** `SignOut`, `ListSignInMethods`, `LinkSignInMethod` and
`UnlinkSignInMethod` read the caller from `x-agrogram-user-id` /
`x-agrogram-session-id`, which the edge sets after calling `VerifyToken`.
SignOut ends the current session (default) or all of them.

## Events

Transactional outbox (`outbox` table, written in the state change's
transaction) plus a relay goroutine that publishes with franz-go (idempotent
producer, acks=all, at-least-once, batches locked with `FOR UPDATE SKIP
LOCKED`, exponential backoff) — the same shape as identity. Kafka being down
never stops serving; rows wait in the outbox.

| Topic | Key | Envelope | When |
| --- | --- | --- | --- |
| `agrogram.notification.request.v1` | recipient user id | `NotificationRequestEvent{metadata, notification_requested}` | password changed via reset (`security.password_changed`), method linked (`security.sign_in_method_linked`), method unlinked (`security.sign_in_method_unlinked`) |

Category SECURITY (which users cannot turn off), `params` at most
`{"method": "GOOGLE"}`, subject `{RESOURCE_TYPE_USER, user id}`. **No code,
token, password or hash ever goes into an event, the outbox or a production
log.** SMS codes never travel over Kafka at all: the proto says they go through
notification's `SendNotification` RPC.

## Layout

```
services/auth-gateway/
├── cmd/auth-gateway/        main: serve (default) | migrate up|down|status | healthcheck
├── internal/
│   ├── apperr/              Error type -> gRPC status with one common.v1.Error
│   ├── auth/                x-agrogram-* metadata -> Principal; outgoing metadata for identity
│   ├── config/              AUTH_GATEWAY_* environment variables
│   ├── domain/              Credential, Session, RefreshToken, PhoneChallenge; display masking
│   ├── events/              NotificationRequestEvent builder, outbox relay (franz-go)
│   ├── grpcserver/          handler (11 RPCs), interceptors (recovery, principal, logging, validation)
│   ├── health/              Postgres readiness -> gRPC health + /healthz, /readyz
│   ├── identityclient/      gRPC client for identity (CreateAccount, GetAccount)
│   ├── log/                 slog (JSON or text)
│   ├── password/            argon2id hashing, PHC strings
│   ├── provider/            Google and Apple ID token verification STUBS
│   ├── ratelimit/           Redis fixed-window limiter (Allow + Exceeded)
│   ├── repository/postgres/ pgx v5 repository, outbox store, integration tests
│   ├── service/             use cases: sign-in, phone flows, linking, tokens
│   ├── sms/                 code generation and the delivery STUB
│   └── token/               key set, JWKS, JWT issue/verify, opaque refresh tokens
├── migrations/              goose SQL migrations (embedded)
├── secrets/                 `make dev-key` writes jwt-es256.pem here (git-ignored)
├── Dockerfile               multi-stage, context back/, distroless nonroot
├── Dockerfile.dockerignore  keeps the context to gen/go + services/auth-gateway, minus secrets/
├── compose.yaml             auth-gateway + auth-gateway-migrate, on deploy/ + identity
├── Makefile                 `make help`
└── .env.example             settings for running on the host
```

## Configuration

Environment variables only (`AUTH_GATEWAY_` prefix). Missing or invalid values
stop the start-up with one error that lists all of them.

| Variable | Default | Meaning |
| --- | --- | --- |
| `AUTH_GATEWAY_DATABASE_URL` | **required** | Postgres URL (role and database `auth_gateway`) |
| `AUTH_GATEWAY_DATABASE_MAX_CONNS` | `10` | pgx pool size |
| `AUTH_GATEWAY_IDENTITY_ADDR` | **required** | identity gRPC address (`127.0.0.1:15051` locally, `identity:50051` in compose) |
| `AUTH_GATEWAY_IDENTITY_TIMEOUT` | `3s` | Per-call timeout for identity and for provider verification |
| `AUTH_GATEWAY_JWT_PRIVATE_KEY_FILE` | empty | PEM file with the signing key (this or the next one is required) |
| `AUTH_GATEWAY_JWT_PRIVATE_KEY` | empty | The signing key itself, PEM or base64 PEM |
| `AUTH_GATEWAY_JWT_PREVIOUS_PUBLIC_KEYS` | empty | Retired public keys (PEM or base64 PEM, comma-separated) still served by `GetJwks` |
| `AUTH_GATEWAY_JWT_ISSUER` | `https://auth.agrogram.local` | `iss` claim, checked on verification |
| `AUTH_GATEWAY_JWT_AUDIENCE` | `agrogram-api` | `aud` claim, checked on verification |
| `AUTH_GATEWAY_ACCESS_TOKEN_TTL` | `15m` | Access token lifetime |
| `AUTH_GATEWAY_REFRESH_TOKEN_TTL` | `720h` | Refresh token and session lifetime (30 days) |
| `AUTH_GATEWAY_TOKEN_CLOCK_SKEW` | `30s` | Leeway when verifying `exp`/`iat` |
| `AUTH_GATEWAY_REDIS_URL` | empty | Redis URL for the rate limits; empty disables them (warning) |
| `AUTH_GATEWAY_KAFKA_BROKERS` | empty | Comma-separated brokers; empty disables the relay (events wait in the outbox) |
| `AUTH_GATEWAY_KAFKA_CLIENT_ID` | `auth-gateway` | Kafka client id |
| `AUTH_GATEWAY_GRPC_ADDR` | `:50051` | gRPC listen address |
| `AUTH_GATEWAY_HTTP_ADDR` | `:8081` | `/healthz`, `/readyz` listen address (also used by `auth-gateway healthcheck`) |
| `AUTH_GATEWAY_GRPC_REFLECTION` | `false` | gRPC server reflection (grpcurl); local only |
| `AUTH_GATEWAY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `AUTH_GATEWAY_LOG_FORMAT` | `json` | `json` or `text` |
| `AUTH_GATEWAY_PHONE_CODE_TTL` | `10m` | How long a code works |
| `AUTH_GATEWAY_PHONE_CODE_RESEND` | `60s` | Earliest resend for a number and purpose |
| `AUTH_GATEWAY_PHONE_CODE_LENGTH` | `6` | Digits in a generated code (4-8) |
| `AUTH_GATEWAY_PHONE_CODE_MAX_ATTEMPTS` | `5` | Wrong codes before a challenge is dead |
| `AUTH_GATEWAY_DEV_PHONE_CODE` | empty | **STUB**, development only: a fixed code so the flow works offline |
| `AUTH_GATEWAY_DEV_LOG_PHONE_CODES` | `false` | **STUB**, development only: log the code at debug level |
| `AUTH_GATEWAY_PHONE_CODE_RATE_LIMIT` | `5` | Code requests per number per window |
| `AUTH_GATEWAY_PHONE_CODE_RATE_WINDOW` | `1h` | That window |
| `AUTH_GATEWAY_SIGN_IN_RATE_LIMIT` | `10` | Failed sign-ins per identity per window |
| `AUTH_GATEWAY_SIGN_IN_RATE_WINDOW` | `15m` | That window |
| `AUTH_GATEWAY_ARGON2_MEMORY_KIB` | `65536` | argon2id memory (64 MiB) |
| `AUTH_GATEWAY_ARGON2_TIME` | `3` | argon2id iterations |
| `AUTH_GATEWAY_ARGON2_PARALLELISM` | `2` | argon2id lanes |
| `AUTH_GATEWAY_VERIFY_TOKEN_CALLER` | `api-gateway` | Only this `x-agrogram-caller` may call `VerifyToken`; empty disables the check |
| `AUTH_GATEWAY_OUTBOX_INTERVAL` | `1s` | Relay poll interval when idle |
| `AUTH_GATEWAY_OUTBOX_BATCH_SIZE` | `100` | Messages per relay batch |
| `AUTH_GATEWAY_OUTBOX_MAX_BACKOFF` | `30s` | Relay backoff cap while Kafka/Postgres fail |
| `AUTH_GATEWAY_OUTBOX_RETENTION` | `168h` | Published outbox rows older than this are deleted (`0` keeps them) |
| `AUTH_GATEWAY_HEALTH_INTERVAL` | `5s` | Postgres ping period (readiness) |
| `AUTH_GATEWAY_SHUTDOWN_TIMEOUT` | `20s` | Graceful shutdown budget |
| `AUTH_GATEWAY_TEST_DATABASE_URL` | empty | Integration tests only; unset = skipped |

Ports: in the container gRPC `:50051` and HTTP `:8081`; on the host (both
`.env.example` and `compose.yaml`) `127.0.0.1:15052` and `127.0.0.1:18052`
(identity uses 15051/18051).

## Commands

Everything runs from `back/services/auth-gateway` in your terminal. Go targets
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
`back/gen/` is git-ignored.

### 2. Signing key and settings (host)

```sh
make dev-key                  # writes secrets/jwt-es256.pem (git-ignored, 0600)
cp .env.example .env          # already points at ./secrets/jwt-es256.pem
make migrate-up               # goose, embedded SQL; also: migrate-status, migrate-down
```

The database and role `auth_gateway` are created by the infra stack
(`deploy/postgres`), and `deploy/.env.example` already carries
`AUTH_GATEWAY_DB_PASSWORD`.

### 3. Run locally, next to identity

auth-gateway needs identity. Either run identity on the host
(`cd ../identity && make run`, gRPC `127.0.0.1:15051`) or start only its
container (`cd ../identity && make compose-up`) — the default
`AUTH_GATEWAY_IDENTITY_ADDR` in `.env.example` points at that port either way.

```sh
make run                      # loads .env; gRPC 127.0.0.1:15052, HTTP 127.0.0.1:18052
curl -i http://127.0.0.1:18052/readyz
```

### 4. Run in Docker Compose (with the infra stack and identity)

```sh
make dev-key                  # the key is mounted read-only; do this first
make compose-up               # builds the image, runs the migrations, waits until healthy
make compose-logs
make compose-down             # removes only the auth-gateway containers
```

Same thing without make, from the directory that contains `back/` (three files,
in this order — relative paths resolve against the first one's directory):

```sh
AUTH_GATEWAY_UID=$(id -u) AUTH_GATEWAY_GID=$(id -g) \
docker compose -f back/deploy/docker-compose.yml \
  -f back/services/identity/compose.yaml \
  -f back/services/auth-gateway/compose.yaml \
  up -d --build --wait auth-gateway
```

The container runs as the host user that owns `secrets/jwt-es256.pem` (0600,
so the image's nonroot user could not read it); without the two variables it
runs as `1000:1000`.

Stop either `make run` or the container before starting the other: both use
host ports 15052/18052. `make infra-down` in `back/` also removes the
auth-gateway containers (`--remove-orphans`).

### 5. Tests

```sh
make test                     # unit tests, no infrastructure (fakes, pgxmock, miniredis, kfake)
make test-race                # the same with -race (needs a C compiler)
make lint                     # go vet (both build tags) + gofmt check
make test-integration         # -tags=integration against AUTH_GATEWAY_TEST_DATABASE_URL
```

The integration tests create a random schema (`auth_gateway_test_<hex>`), run
the migrations into it through `search_path`, exercise the real repository
(unique constraints, guarded updates, outbox) and the service end to end
(sign-up → sign-in → refresh → reuse detection → verify → sign-out), roll the
migrations back and forward, and drop the schema. The `auth_gateway` role owns
its database, so it may create schemas.

### 6. Smoke test with grpcurl

With `AUTH_GATEWAY_GRPC_REFLECTION=true` and
`AUTH_GATEWAY_DEV_PHONE_CODE=123456` (both defaults in `.env.example` and
`compose.yaml`):

```sh
ADDR=127.0.0.1:15052
uuid() { cat /proc/sys/kernel/random/uuid; }   # or uuidgen

grpcurl -plaintext $ADDR list agrogram.authgateway.v1.AuthGatewayService

# --- Google (STUB token: dev-google:<subject>[:<email>]) ---
grpcurl -plaintext -d '{"google":{"id_token":"dev-google:104711920733:alice@gmail.com"},
  "client":{"platform":"CLIENT_PLATFORM_WEB","device_name":"Chrome on Linux"}}' \
  $ADDR agrogram.authgateway.v1.AuthGatewayService/SignIn
# -> user_id, session_id, tokens, is_new_account: true (a second call: false)

# --- Apple (STUB token: dev-apple:<subject>[:<email>]) ---
grpcurl -plaintext -d '{"apple":{"identity_token":"dev-apple:000123.abcdef"}}' \
  $ADDR agrogram.authgateway.v1.AuthGatewayService/SignIn

# --- Phone: code -> sign-up -> sign-in ---
grpcurl -plaintext -d '{"phone_number":"+998901234567","purpose":"PHONE_CODE_PURPOSE_SIGN_UP"}' \
  $ADDR agrogram.authgateway.v1.AuthGatewayService/RequestPhoneCode
CHALLENGE=<challenge_id from the response>   # the code is 123456 (AUTH_GATEWAY_DEV_PHONE_CODE)

grpcurl -plaintext -d "{\"request_id\":\"$(uuid)\",\"phone_password\":{
  \"phone_number\":\"+998901234567\",
  \"verification\":{\"challenge_id\":\"$CHALLENGE\",\"code\":\"123456\"},
  \"password\":\"dehqon-parol-2026\"}}" \
  $ADDR agrogram.authgateway.v1.AuthGatewayService/SignUpWithPhone
ACCESS=<tokens.access_token>; REFRESH=<tokens.refresh_token>
USER_ID=<user_id>; SESSION_ID=<session_id>

grpcurl -plaintext -d '{"phone_password":{"phone_number":"+998901234567","password":"dehqon-parol-2026"}}' \
  $ADDR agrogram.authgateway.v1.AuthGatewayService/SignIn

# --- Tokens ---
grpcurl -plaintext -d '{}' $ADDR agrogram.authgateway.v1.AuthGatewayService/GetJwks
grpcurl -plaintext -H 'x-agrogram-caller: api-gateway' -d "{\"access_token\":\"$ACCESS\"}" \
  $ADDR agrogram.authgateway.v1.AuthGatewayService/VerifyToken
grpcurl -plaintext -d "{\"refresh_token\":\"$REFRESH\"}" \
  $ADDR agrogram.authgateway.v1.AuthGatewayService/RefreshToken
# reusing $REFRESH now answers TOKEN_REVOKED and kills the session

# --- Methods and sign-out (the edge normally sets these headers) ---
grpcurl -plaintext -H "x-agrogram-user-id: $USER_ID" -H "x-agrogram-session-id: $SESSION_ID" \
  -d '{}' $ADDR agrogram.authgateway.v1.AuthGatewayService/ListSignInMethods
grpcurl -plaintext -H "x-agrogram-user-id: $USER_ID" \
  -d '{"google":{"id_token":"dev-google:104711920733:alice@gmail.com"}}' \
  $ADDR agrogram.authgateway.v1.AuthGatewayService/LinkSignInMethod
grpcurl -plaintext -H "x-agrogram-user-id: $USER_ID" -d '{"method":"SIGN_IN_METHOD_GOOGLE"}' \
  $ADDR agrogram.authgateway.v1.AuthGatewayService/UnlinkSignInMethod
grpcurl -plaintext -H "x-agrogram-user-id: $USER_ID" -H "x-agrogram-session-id: $SESSION_ID" \
  -d '{"scope":"SIGN_OUT_SCOPE_CURRENT_SESSION"}' \
  $ADDR agrogram.authgateway.v1.AuthGatewayService/SignOut

# --- Password reset (ends every session) ---
grpcurl -plaintext -d '{"phone_number":"+998901234567","purpose":"PHONE_CODE_PURPOSE_RESET_PASSWORD"}' \
  $ADDR agrogram.authgateway.v1.AuthGatewayService/RequestPhoneCode
grpcurl -plaintext -d "{\"phone_password\":{\"phone_number\":\"+998901234567\",
  \"verification\":{\"challenge_id\":\"<challenge_id>\",\"code\":\"123456\"},
  \"password\":\"yangi-parol-2026\"}}" \
  $ADDR agrogram.authgateway.v1.AuthGatewayService/ResetPassword

grpcurl -plaintext -d '{"service":"agrogram.authgateway.v1.AuthGatewayService"}' $ADDR grpc.health.v1.Health/Check
```

The security notification requests appear on `agrogram.notification.request.v1`
in Kafka UI (http://127.0.0.1:18080); values are Protobuf binary.

Without `AUTH_GATEWAY_DEV_PHONE_CODE` the code is random, and the only way to
see it is `AUTH_GATEWAY_DEV_LOG_PHONE_CODES=true` plus
`AUTH_GATEWAY_LOG_LEVEL=debug` (or `SELECT` nothing — the database stores the
digest only).

## Stubs

`// STUB:` marks everything that stands in for an integration that does not
exist yet.

| Where | Stands in for |
| --- | --- |
| `internal/provider/provider.go` (package doc) | The whole Google/Apple verification: neither stub calls out |
| `internal/provider/provider.go` (`GoogleStub`) | Google JWKS, RS256 signature, `iss`/`aud`/`exp`/`nonce` checks. Accepts `dev-google:<subject>[:<email>]` |
| `internal/provider/provider.go` (`AppleStub`) | Apple JWKS, the same checks plus the `authorization_code` exchange that allows revoking the link. Accepts `dev-apple:<subject>[:<email>]` |
| `internal/sms/sms.go` (package doc) | The whole SMS path: nothing is ever sent |
| `internal/sms/sms.go` (`StubSender`) | notification's `SendNotification` RPC; logs the code only behind `AUTH_GATEWAY_DEV_LOG_PHONE_CODES` |
| `internal/service/phone.go` (`RequestPhoneCode`) | The fixed development code (`AUTH_GATEWAY_DEV_PHONE_CODE`) and the stub delivery call |
| `internal/service/service.go` (`Config.DevPhoneCode`) | The same, in the configuration |
| `cmd/auth-gateway/serve.go` | Wiring: `provider.GoogleStub`, `provider.AppleStub`, `sms.StubSender` instead of real clients |

## TODOs

| Where | What |
| --- | --- |
| `internal/sms/sms.go` (`StubSender.Send`) | `TODO(notification)`: call notification's `SendNotification` RPC (template `security.phone_code`, channel SMS). Codes must keep going over gRPC; `NotificationRequested` on Kafka is the wrong channel for a secret |
| `internal/service/tokens.go` (`revokeForReuse`) | `TODO(events)`: a "refresh token reused" security event belongs on a session/auth stream, and `events/v1` has no such schema (only `NotificationRequested`) |
| `internal/service/methods.go` (`UnlinkSignInMethod`) | `TODO(sessions)`: whether unlinking a method should end the sessions opened with it is a product decision the proto does not settle |
| `internal/identityclient/identityclient.go` (`Dial`) | `TODO(security)`: service-to-service TLS / mesh identity; `x-agrogram-caller` is trusted until then |

## Known gaps

- **No session listing or revocation API.** The schema keeps `ClientInfo` and
  `last_seen_at` per session (the proto's `ClientInfo` says they are "shown in
  session lists"), but `auth_gateway.proto` has no `ListSessions` /
  `RevokeSession` RPC, so nothing exposes them. Protos are frozen here.
- **No housekeeping worker.** Expired sessions, spent refresh tokens and dead
  phone challenges are never deleted (indexes for it exist); only published
  outbox rows are purged. A `DELETE`-by-age job belongs next to identity's
  worker once there is a second reason to have one.
- **`RequestPhoneCode` reveals whether a number is registered** (NOT_FOUND for
  a reset, PHONE_ALREADY_REGISTERED for a sign-up). The sign-up direction is
  unavoidable given the proto's error list, so the reset direction is honest
  rather than pretending a code was sent.
- **Roles in a token are a snapshot.** They are refreshed from
  identity on every refresh, not while a token lives; that is the contract in
  `common/v1/auth.proto`.
- **Google/Apple account deletion** (revoking the Apple refresh token, removing
  the link) has no RPC and no flow yet; see the Apple STUB comment.
- **`x-agrogram-caller` is trusted** on `VerifyToken`; the edge must strip it
  from client traffic (mTLS / service identity later).
