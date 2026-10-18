# profile-social

Go service that owns **public profiles** and the **follow graph**, and
serves a user's own listings (read from catalog). Contract:
[`proto/profilesocial/v1/profile_social.proto`](../../proto/profilesocial/v1/profile_social.proto)
(`agrogram.profilesocial.v1.ProfileSocialService`). Conventions (errors,
metadata, validation, events) are in [back/README.md](../../README.md).

Profiles exist and stay in sync through identity's account stream
(`agrogram.identity.account.v1`), which profile-social consumes. AGROGRAM is
listings-only: there are no transactions, so no ratings or deal counts, and
no Premium or verified-farmer badges.

| RPC | Who may call | What it does | Errors |
| --- | --- | --- | --- |
| `CreateProfile` | a signed-in user (their own profile) | Fills in the public fields of the caller's row and makes it ACTIVE (version + 1). Works on a row the account stream created, on a deleted profile (created anew), or with no row yet. Idempotent on `request_id` | UNAUTHENTICATED; ALREADY_EXISTS (ACTIVE, other request id); INVALID_ARGUMENT |
| `GetProfile` | anyone (anonymous too) | An ACTIVE profile; `is_followed_by_me` for signed-in callers | NOT_FOUND (none, PENDING or DELETED, also for the owner) |
| `UpdateProfile` | the owner only (`profile.id` = caller) | Changes the `update_mask` fields (display_name, avatar, bio, region, district); version + 1; values equal to the current ones change nothing | PERMISSION_DENIED; NOT_FOUND; ABORTED / CONFLICT (stale `expected_version`, `metadata.current_version`) |
| `DeleteProfile` | the owner (empty `user_id`); `ROLE_ADMIN` for others | Soft delete: DELETED, version + 1; its follow edges are removed and the other sides' counters lowered in the same transaction; the fields stay. Deleting a deleted profile is a no-op | PERMISSION_DENIED; NOT_FOUND (never created) |
| `FollowUser` | a signed-in user with an ACTIVE profile | Adds the edge; follower/following counters + 1 in the same transaction, only when the edge is new (following again is a no-op). Rate-limited per user | SELF_ACTION_NOT_ALLOWED (INVALID_ARGUMENT); NOT_FOUND (target not ACTIVE); FAILED_PRECONDITION (caller has no ACTIVE profile); RATE_LIMITED |
| `UnfollowUser` | a signed-in user | Removes the edge and lowers both counters; unfollowing a user not followed is a no-op. Rate-limited per user | SELF_ACTION_NOT_ALLOWED; NOT_FOUND; RATE_LIMITED |
| `ListFollowers` / `ListFollowing` | anyone | ACTIVE profiles, newest follow first; opaque keyset page tokens; `total_size` = the cached counter | NOT_FOUND (user not ACTIVE); INVALID_ARGUMENT (token of another list or user) |
| `ListUserListings` | anyone for ACTIVE listings of a user with an ACTIVE profile; the owner for any status | Calls catalog `ListListings` with an owner filter and maps each listing to a `ListingCard`; catalog's page token passes through | PERMISSION_DENIED (non-owner asks for a status other than ACTIVE); NOT_FOUND; an unreachable catalog is an empty page, not an error |

Every error is a gRPC status with exactly one `agrogram.common.v1.Error`
(domain `profile-social`) in its details; codes follow the `ErrorReason`
table. Request validation is the protovalidate annotations of the proto
(INVALID_ARGUMENT / `VALIDATION_FAILED` with `field_violations`), plus what
the contract cannot express (a display name of blanks).

Callers reach profile-social through the edge (api-gateway), which checks the
access token with auth-gateway (`VerifyToken`) and forwards the user as
`x-agrogram-*` metadata. profile-social never calls identity or
auth-gateway: accounts arrive on the account stream, so neither address is
in its configuration. The only service it calls is catalog, for `ListUserListings`.

## Profile life cycle

One row per user id in `profiles`, with the user's public fields, the
follow counters and a projection of the account (roles, `account_version`):

| Status | How a row gets there | Visible through the API |
| --- | --- | --- |
| `PENDING` | the first account event for the user (normally `AccountCreated`) | no: NOT_FOUND everywhere, excluded from lists, cannot be followed |
| `ACTIVE` | `CreateProfile` (from PENDING, from DELETED, or inserting the row with `account_version` 0 when no account event has arrived yet) | yes |
| `DELETED` | `DeleteProfile` | no; `CreateProfile` makes it ACTIVE again (follows gone) |

- The account projection and the user's fields never overwrite each other:
  events touch only the projection columns, `CreateProfile` / `UpdateProfile`
  only the user's. Events may therefore arrive before or after
  `CreateProfile`.
- `version` is the profile's optimistic-concurrency version: 0 until the
  user creates the profile, + 1 on every create, update and delete. Account
  updates and follow counters do not bump it (a follower
  must not make the owner's edit fail with CONFLICT).
- Roles: ADMIN is the only role left. RESELLER and FARMER, granted before
  the listings-only pivot, are dropped when an account event is decoded
  (and migration 00002 removed them from stored rows).

## Events

### Consumed: identity's account stream

| | |
| --- | --- |
| Topic | `agrogram.identity.account.v1` (`AccountEvent`, [`events/v1/account.proto`](../../proto/events/v1/account.proto)), key = user id |
| Consumer group | `profilesocial` (`PROFILE_SOCIAL_KAFKA_CONSUMER_GROUP`); a new group starts at the beginning of the topic |
| Dead letters | `agrogram.profilesocial.deadletter.v1` (`DeadLetter`, [`events/v1/metadata.proto`](../../proto/events/v1/metadata.proto)), key = original key, `consumer_group` = the group |

Processing follows the consumer rules of `metadata.proto`:

- **One path for every event type.** Each record's `AccountSnapshot` is
  applied to the projection when its version is newer than the stored
  `account_version` (`INSERT … ON CONFLICT (id) DO UPDATE … WHERE
  profiles.account_version < EXCLUDED.account_version`, which also creates
  the PENDING row). `AccountCreated` and `AccountRoleChanged` (roles)
  change it; an event that repeats the current version is a no-op. An
  unknown `oneof` case (a newer identity) applies its snapshot like any other.
- **De-duplication.** The event id goes into `processed_events` in the same
  transaction as the projection change; a known id changes nothing. An older
  snapshot after a newer one is ignored (and recorded, so its redelivery is a
  duplicate). Inbox rows are purged after `PROFILE_SOCIAL_INBOX_RETENTION`.
- **Poison records** (not `application/x-protobuf`, undecodable, failing
  protovalidate, snapshot contradicting its metadata) are dead-lettered at
  once (`attempts` 1, `ERROR_REASON_VALIDATION_FAILED`).
- **Outages** (Postgres unreachable, credentials refused, schema not
  migrated, timeouts, serialization failures; `postgres.IsTransient`) are
  retried with exponential backoff for as long as they last: the record is
  neither skipped nor dead-lettered and the partition does not advance.
- **Any other failure** is retried `PROFILE_SOCIAL_CONSUMER_MAX_ATTEMPTS`
  times with backoff, then dead-lettered (`ERROR_REASON_INTERNAL`,
  `metadata.failure = retries_exhausted`) and the consumer moves on.
- **Offsets** are committed after each poll batch, only for records that
  were processed or dead-lettered (at-least-once). A dead letter that cannot
  be published is retried until it is; its record stays uncommitted. A
  rebalance or a crash between processing and the commit only redelivers
  records, which the inbox absorbs.

### Published: the profile stream

Transactional outbox (`outbox` table) plus a relay goroutine (the same relay
as identity: franz-go idempotent producer, acks=all, batches locked with
`FOR UPDATE SKIP LOCKED`, exponential backoff). Published rows are purged
after `PROFILE_SOCIAL_OUTBOX_RETENTION`.

profile-social publishes **no event today**: `RatingUpdated`, the only case
of `agrogram.profilesocial.profile.v1`, went with ratings. The relay keeps
running to drain rows queued before that and for the profile events to come
(`TODO(profile-stream)`).

## Layout

```
services/profile-social/
├── cmd/profile-social/      main: serve (default) | migrate up|down|status | healthcheck
├── internal/
│   ├── apperr/              Error type -> gRPC status with one common.v1.Error
│   ├── auth/                x-agrogram-* metadata -> Principal; outgoing metadata for catalog
│   ├── catalogclient/       gRPC client for catalog ListListings -> ListingCard (lazy connection)
│   ├── config/              PROFILE_SOCIAL_* environment variables
│   ├── domain/              Profile, Account projection, pages
│   ├── events/              DeadLetter builder, outbox relay, consumer, account record handler
│   ├── grpcserver/          handler, interceptors (recovery, principal, logging, validation)
│   ├── health/              Postgres readiness -> gRPC health + /healthz, /readyz
│   ├── log/                 slog (JSON or text)
│   ├── ratelimit/           Redis fixed-window limiter (follow/unfollow)
│   ├── repository/postgres/ pgx v5 repository, outbox/inbox store, IsTransient, integration tests
│   └── service/             use cases: profiles, follows, account projection, listings
├── migrations/              goose SQL migrations (embedded)
├── Dockerfile               multi-stage, context back/, distroless nonroot
├── Dockerfile.dockerignore  keeps the context to gen/go + services/profile-social
├── compose.yaml             profile-social + profile-social-migrate on top of deploy/docker-compose.yml
├── Makefile                 `make help`
└── .env.example             settings for running on the host
```

Schema:

- `migrations/00001_init.sql`: `profiles` (PK user id; status, public
  fields, `create_request_id`, `version`, counters, account projection with
  `account_version`), `follows` (PK `(follower_id, followee_id)`, no
  self-follow; indexes `(followee_id, created_at DESC, follower_id DESC)`
  and `(follower_id, created_at DESC, followee_id DESC)`),
  `completed_transactions` and `ratings` (dropped by 00002),
  `processed_events` (inbox, PK event id), `outbox` (as identity's).
- `migrations/00002_drop_ratings_premium.sql`: drops `ratings`,
  `completed_transactions` and the rating, deals, Premium and
  verified-farmer columns; removes RESELLER and FARMER from `roles` and
  limits its CHECK to ADMIN. Its Down restores the 00001 schema, empty.

Concurrency: follow, unfollow and delete lock the profile rows involved in id
order (`FOR NO KEY UPDATE`) before touching edges and counters, so they
serialize without deadlocks; edge inserts use `ON CONFLICT DO NOTHING` and
counters change only when a row was inserted or deleted.

## Configuration

Environment variables only (`PROFILE_SOCIAL_` prefix). Missing or invalid
values stop the start-up with one error that lists all of them.

| Variable | Default | Meaning |
| --- | --- | --- |
| `PROFILE_SOCIAL_DATABASE_URL` | **required** | Postgres URL (role and database `profile_social`) |
| `PROFILE_SOCIAL_DATABASE_MAX_CONNS` | `10` | pgx pool size |
| `PROFILE_SOCIAL_REDIS_URL` | empty | Redis URL for the follow rate limit (keys `profile-social:ratelimit:*`); empty disables it (warning) |
| `PROFILE_SOCIAL_KAFKA_BROKERS` | empty | Comma-separated brokers; empty disables the account-stream consumer (no role updates) and the relay (warning) |
| `PROFILE_SOCIAL_KAFKA_CLIENT_ID` | `profile-social` | Kafka client id |
| `PROFILE_SOCIAL_KAFKA_CONSUMER_GROUP` | `profilesocial` | Consumer group of the account stream, also `DeadLetter.consumer_group` |
| `PROFILE_SOCIAL_CATALOG_ADDR` | empty | catalog gRPC address for ListUserListings (lazy connection): `127.0.0.1:15054` in `.env.example` (catalog's local binary or container), `catalog:50051` in `compose.yaml`. Empty, or catalog unreachable: an empty page (warning) |
| `PROFILE_SOCIAL_CATALOG_TIMEOUT` | `2s` | Per-call catalog timeout |
| `PROFILE_SOCIAL_GRPC_ADDR` | `:50051` | gRPC listen address |
| `PROFILE_SOCIAL_HTTP_ADDR` | `:8081` | `/healthz`, `/readyz` listen address (also used by `profile-social healthcheck`) |
| `PROFILE_SOCIAL_GRPC_REFLECTION` | `false` | gRPC server reflection (grpcurl); local only |
| `PROFILE_SOCIAL_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `PROFILE_SOCIAL_LOG_FORMAT` | `json` | `json` or `text` |
| `PROFILE_SOCIAL_FOLLOW_RATE_LIMIT` | `100` | Follow + unfollow calls per user per window (fails open without Redis) |
| `PROFILE_SOCIAL_FOLLOW_RATE_WINDOW` | `1h` | Rate-limit window |
| `PROFILE_SOCIAL_CONSUMER_MAX_ATTEMPTS` | `5` | Retry budget of a failing record (outages excluded) before it is dead-lettered |
| `PROFILE_SOCIAL_CONSUMER_RETRY_BACKOFF` | `500ms` | First retry delay, doubled per attempt |
| `PROFILE_SOCIAL_CONSUMER_RETRY_BACKOFF_MAX` | `30s` | Retry delay cap |
| `PROFILE_SOCIAL_CONSUMER_HANDLE_TIMEOUT` | `10s` | One processing attempt |
| `PROFILE_SOCIAL_CONSUMER_BATCH_SIZE` | `100` | Records per poll (offsets are committed per batch) |
| `PROFILE_SOCIAL_INBOX_RETENTION` | `720h` | Processed event ids older than this are deleted (`0` keeps them) |
| `PROFILE_SOCIAL_OUTBOX_INTERVAL` | `1s` | Relay poll interval when idle |
| `PROFILE_SOCIAL_OUTBOX_BATCH_SIZE` | `100` | Messages per relay batch |
| `PROFILE_SOCIAL_OUTBOX_MAX_BACKOFF` | `30s` | Relay backoff cap while Kafka/Postgres fail |
| `PROFILE_SOCIAL_OUTBOX_RETENTION` | `168h` | Published outbox rows older than this are deleted (`0` keeps them) |
| `PROFILE_SOCIAL_HEALTH_INTERVAL` | `5s` | Postgres ping period (readiness) |
| `PROFILE_SOCIAL_SHUTDOWN_TIMEOUT` | `20s` | Graceful shutdown budget |
| `PROFILE_SOCIAL_TEST_DATABASE_URL` | empty | Integration tests only; unset = skipped |

Ports: in the container gRPC `:50051` and HTTP `:8081`; on the host (both
`.env.example` and `compose.yaml`) `127.0.0.1:15053` and `127.0.0.1:18053`
(identity uses 15051/18051, auth-gateway 15052/18052).

## Commands

Everything runs from `back/services/profile-social` in your terminal. Go
targets need Go (the go.mod asks for 1.26+, toolchain go1.27.1;
`GOTOOLCHAIN=auto` downloads it). The local infrastructure is `make infra-up`
in `back/` (see [deploy/README.md](../../deploy/README.md)); it creates the
`profile_social` database and role and the Kafka topics.

### 1. Code generation

```sh
make gen     # the shared back/tools/codegen.sh: pinned buf 1.73.0, writes back/gen/go/go.mod
```

### 2. Settings and migrations (host)

```sh
cp .env.example .env          # then set the passwords from back/deploy/.env
make migrate-up               # goose, embedded SQL; also: migrate-status, migrate-down
```

### 3. Run locally (with the stream consumer)

```sh
make run                      # loads .env; gRPC 127.0.0.1:15053, HTTP 127.0.0.1:18053
curl -i http://127.0.0.1:18053/readyz
```

With `PROFILE_SOCIAL_KAFKA_BROKERS` set, `serve` starts the consumer of
`agrogram.identity.account.v1` (group `profilesocial`) and the outbox relay
next to the gRPC server. On first start the group reads the topic from the
beginning, so every account identity has published gets its PENDING row.
`make infra-up` in `back/` creates the topic (the broker does not
auto-create topics).

With `PROFILE_SOCIAL_CATALOG_ADDR=127.0.0.1:15054` (`.env.example`),
`ListUserListings` calls catalog's local binary or container; while catalog
is not running it answers empty pages (with a warning).

### 4. Run in Docker Compose (with the infra stack)

```sh
make compose-up               # builds the image, runs profile-social-migrate, waits until healthy
make compose-logs
make compose-down             # removes only the profile-social containers
```

### 5. Run alongside the other services

profile-social only needs Postgres and Kafka to start. identity feeds it
through the account stream; it calls catalog for `ListUserListings`. Local
binaries, one terminal each (each with its `.env`; `.env.example` already
points profile-social at catalog on `127.0.0.1:15054`):

```sh
(cd ../identity && make migrate-up && make run)          # 127.0.0.1:15051
(cd ../auth-gateway && make migrate-up && make run)      # 127.0.0.1:15052 (needs make dev-key once)
(cd ../catalog && make migrate-up && make run)           # 127.0.0.1:15054
make migrate-up && make run                              # 127.0.0.1:15053
```

Or all four in Compose, from the directory that contains `back/`
(profile-social's compose file points at `catalog:50051`):

```sh
AUTH_GATEWAY_UID=$(id -u) AUTH_GATEWAY_GID=$(id -g) \
docker compose -f back/deploy/docker-compose.yml \
  -f back/services/identity/compose.yaml \
  -f back/services/auth-gateway/compose.yaml \
  -f back/services/profile-social/compose.yaml \
  -f back/services/catalog/compose.yaml \
  up -d --build --wait identity auth-gateway profile-social catalog
```

Leaving out catalog's file keeps everything else working: `ListUserListings`
answers empty pages.

### 6. Tests

```sh
make test                     # unit tests, no infrastructure (fakes, pgxmock, miniredis, kfake, bufconn)
make test-race                # the same with -race (needs a C compiler)
make lint                     # go vet (both build tags) + gofmt check
make test-integration         # -tags=integration against PROFILE_SOCIAL_TEST_DATABASE_URL
```

The unit tests cover profile create/update/delete, follows (idempotency,
counters, self-follow, pagination), the account projection (duplicates,
older-after-newer, same-version repeats, unknown oneof case, event before and
after `CreateProfile`), and the consumer on a kfake cluster (order, commits,
poison and budget dead letters, outages that are never dead-lettered, a
shutdown that leaves the record in progress uncommitted). The integration
tests create a random schema (`profile_social_test_<hex>`), migrate it
through `search_path`, run the real repository and service (version guard,
concurrent follows, keyset pages, delete, the outbox, outages that are
transient), migration 00002 down and up (legacy roles removed, FARMER
rejected afterwards), roll the migrations back and forward, and drop the
schema.

### 7. Smoke test with grpcurl

With `PROFILE_SOCIAL_GRPC_REFLECTION=true` and identity + auth-gateway
running (section 5); the listing step also needs catalog:

```sh
PS=127.0.0.1:15053; AG=127.0.0.1:15052; ID=127.0.0.1:15051
uuid() { cat /proc/sys/kernel/random/uuid; }

# sign in (STUB Google token): identity creates the account and publishes AccountCreated
grpcurl -plaintext -d '{"google":{"id_token":"dev-google:1001:dilnoza@gmail.com"}}' \
  $AG agrogram.authgateway.v1.AuthGatewayService/SignIn
USER_ID=<user_id from the response>

grpcurl -plaintext -H "x-agrogram-user-id: $USER_ID" -d "{\"user_id\":\"$USER_ID\"}" \
  $PS agrogram.profilesocial.v1.ProfileSocialService/GetProfile          # NOT_FOUND: still PENDING
grpcurl -plaintext -H "x-agrogram-user-id: $USER_ID" \
  -d "{\"request_id\":\"$(uuid)\",\"profile\":{\"display_name\":\"Dilnoza\",
  \"region\":\"REGION_FARGONA\",\"district\":\"Quva\"}}" \
  $PS agrogram.profilesocial.v1.ProfileSocialService/CreateProfile

# a second user follows the first
grpcurl -plaintext -d '{"google":{"id_token":"dev-google:1002:bobur@gmail.com"}}' \
  $AG agrogram.authgateway.v1.AuthGatewayService/SignIn
OTHER=<user_id>
grpcurl -plaintext -H "x-agrogram-user-id: $OTHER" -d "{\"request_id\":\"$(uuid)\",\"profile\":{\"display_name\":\"Bobur\"}}" \
  $PS agrogram.profilesocial.v1.ProfileSocialService/CreateProfile
grpcurl -plaintext -H "x-agrogram-user-id: $OTHER" -d "{\"user_id\":\"$USER_ID\"}" \
  $PS agrogram.profilesocial.v1.ProfileSocialService/FollowUser          # follower_count 1, is_followed_by_me
grpcurl -plaintext -d "{\"user_id\":\"$USER_ID\",\"page\":{\"page_size\":10}}" \
  $PS agrogram.profilesocial.v1.ProfileSocialService/ListFollowers

# the user's listings, from catalog (an empty page, with a warning in the log, while catalog is not running)
grpcurl -plaintext -d "{\"user_id\":\"$USER_ID\"}" $PS agrogram.profilesocial.v1.ProfileSocialService/ListUserListings

grpcurl -plaintext -d '{"service":"agrogram.profilesocial.v1.ProfileSocialService"}' $PS grpc.health.v1.Health/Check
```

The consumer's progress is visible in Kafka UI (http://127.0.0.1:18080,
consumer group `profilesocial`); dead letters land on
`agrogram.profilesocial.deadletter.v1`.

## Stubs

`// STUB:` marks every stand-in. profile-social has none left: the empty page
`ListUserListings` answers while catalog is not configured or unreachable is
a deliberate fallback, not a stand-in.

## TODOs

- `TODO(media)`: avatars carry only `media_id`; type, status and URLs come
  from media-pipeline (`internal/grpcserver/handler.go`,
  `internal/events/events.go`).
- `TODO(profile-stream)`: publish profile creations and updates once
  `profile.proto` has such an event (`internal/service/profiles.go`).
- `TODO(security)`: service-to-service TLS for the catalog client
  (`internal/catalogclient/catalogclient.go`).

## Known gaps

- profile-social publishes no profile event, so catalog cannot build owner
  cards (names, avatars) from the profile stream yet (`TODO(profile-stream)`).
- The consumer starts a new group at the beginning of the account topic.
  Accounts whose events were deleted by topic retention before profile-social
  first ran get no PENDING row until their next account event (a backfill
  would need identity to re-publish snapshots).
- `ListUserListings` calls catalog live, so a profile shows no listings while
  catalog is down. The proto's service comment has profile-social project
  catalog's listing stream (`agrogram.catalog.listing.v1`) instead;
  `internal/service/listings.go` describes how, should the live call become
  a latency or availability problem.
- The comments of `migrations/00001_init.sql` still describe ratings, deals
  and badges, which 00002 drops; applied migrations are not edited.
- Account events published before the pivot carry RESELLER / FARMER. After
  the contract drops those values, protovalidate (`defined_only`) rejects
  such a record and it is dead-lettered; only replays of old events are
  affected.
- A `common.v1.Role` value added to the proto later fails protovalidate
  (`defined_only`) until profile-social is regenerated, so such account
  events would be dead-lettered.
- Avatars are not checked against media-pipeline (existence, purpose
  AVATAR, ownership).
- Internal callers are recognized by `x-agrogram-caller` alone; the edge must
  strip client-supplied `x-agrogram-*` headers (mTLS / service identity
  later).
