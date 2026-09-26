# profile-social

Go service that owns **public profiles**, the **follow graph** and **user
ratings**, and serves a user's own listings (read from catalog). Contract:
[`proto/profilesocial/v1/profile_social.proto`](../../proto/profilesocial/v1/profile_social.proto)
(`agrogram.profilesocial.v1.ProfileSocialService`). Conventions (errors,
metadata, validation, events) are in [back/README.md](../../README.md).

Profiles exist and stay in sync through identity's account stream
(`agrogram.identity.account.v1`), which profile-social consumes; completed
escrow transactions arrive on order-escrow's transaction stream
(`agrogram.orderescrow.transaction.v1`), which it consumes too, and unlock
ratings; ratings are published on the profile stream
(`agrogram.profilesocial.profile.v1`) through a transactional outbox.

| RPC | Who may call | What it does | Errors |
| --- | --- | --- | --- |
| `CreateProfile` | a signed-in user (their own profile) | Fills in the public fields of the caller's row and makes it ACTIVE (version + 1). Works on a row the account stream created, on a deleted profile (created anew), or with no row yet. Idempotent on `request_id` | UNAUTHENTICATED; ALREADY_EXISTS (ACTIVE, other request id); INVALID_ARGUMENT |
| `GetProfile` | anyone (anonymous too) | An ACTIVE profile; `is_followed_by_me` for signed-in callers; `is_premium` evaluated against `premium_until` now | NOT_FOUND (none, PENDING or DELETED, also for the owner) |
| `UpdateProfile` | the owner only (`profile.id` = caller) | Changes the `update_mask` fields (display_name, avatar, bio, region, district); version + 1; values equal to the current ones change nothing | PERMISSION_DENIED; NOT_FOUND; ABORTED / CONFLICT (stale `expected_version`, `metadata.current_version`) |
| `DeleteProfile` | the owner (empty `user_id`); `ROLE_ADMIN` for others | Soft delete: DELETED, version + 1; its follow edges are removed and the other sides' counters lowered in the same transaction; fields, ratings and transaction history stay. Deleting a deleted profile is a no-op | PERMISSION_DENIED; NOT_FOUND (never created) |
| `FollowUser` | a signed-in user with an ACTIVE profile | Adds the edge; follower/following counters + 1 in the same transaction, only when the edge is new (following again is a no-op). Rate-limited per user | SELF_ACTION_NOT_ALLOWED (INVALID_ARGUMENT); NOT_FOUND (target not ACTIVE); FAILED_PRECONDITION (caller has no ACTIVE profile); RATE_LIMITED |
| `UnfollowUser` | a signed-in user | Removes the edge and lowers both counters; unfollowing a user not followed is a no-op. Rate-limited per user | SELF_ACTION_NOT_ALLOWED; NOT_FOUND; RATE_LIMITED |
| `ListFollowers` / `ListFollowing` | anyone | ACTIVE profiles, newest follow first; opaque keyset page tokens; `total_size` = the cached counter | NOT_FOUND (user not ACTIVE); INVALID_ARGUMENT (token of another list or user) |
| `RateUser` | a signed-in party of a completed transaction | Rates the counterparty (score 1-5, comment ≤ 500), once per transaction and rater; the rating, the rated user's aggregate (sum, count, version + 1) and `RatingUpdated` in the outbox commit together. Idempotent on `request_id` | RATING_NOT_ALLOWED (FAILED_PRECONDITION: no such completed transaction, including one whose `PayoutReleased` has not been consumed yet, or the caller is not a party); ALREADY_RATED |
| `ListRatings` | anyone | Ratings received, newest first, plus `summary` (average, count) | NOT_FOUND (user not ACTIVE) |
| `ListUserListings` | anyone for ACTIVE listings of a user with an ACTIVE profile; the owner for any status | Calls catalog `ListListings` with an owner filter and maps each listing to a `ListingCard`; catalog's page token passes through | PERMISSION_DENIED (non-owner asks for a status other than ACTIVE); NOT_FOUND; an unreachable catalog is an empty page, not an error |

Every error is a gRPC status with exactly one `agrogram.common.v1.Error`
(domain `profile-social`) in its details; codes follow the `ErrorReason`
table. Request validation is the protovalidate annotations of the proto
(INVALID_ARGUMENT / `VALIDATION_FAILED` with `field_violations`), plus what
the contract cannot express (a display name of blanks).

Callers reach profile-social through the edge (api-gateway), which checks the
access token with auth-gateway (`VerifyToken`) and forwards the user as
`x-agrogram-*` metadata. profile-social never calls identity, auth-gateway
or order-escrow: accounts arrive on the account stream and completed
transactions on the transaction stream, so none of their addresses is in its
configuration. The only service it calls is catalog, for `ListUserListings`.

## Profile life cycle

One row per user id in `profiles`, with the user's public fields, counters
and a projection of the account (roles, `is_verified_farmer`, `is_premium`,
`premium_until`, `account_version`):

| Status | How a row gets there | Visible through the API |
| --- | --- | --- |
| `PENDING` | the first account event for the user (normally `AccountCreated`), or a rating received before the user created a profile | no: NOT_FOUND everywhere, excluded from lists, cannot be followed |
| `ACTIVE` | `CreateProfile` (from PENDING, from DELETED, or inserting the row with `account_version` 0 when no account event has arrived yet) | yes |
| `DELETED` | `DeleteProfile` | no; `CreateProfile` makes it ACTIVE again (ratings kept, follows gone) |

- The account projection and the user's fields never overwrite each other:
  events touch only the projection columns, `CreateProfile` / `UpdateProfile`
  only the user's. Events may therefore arrive before or after
  `CreateProfile`; until the first one arrives the badges are off.
- `version` is the profile's optimistic-concurrency version: 0 until the
  user creates the profile, + 1 on every create, update, delete and received
  rating. Account updates and follow counters do not bump it (a follower
  must not make the owner's edit fail with CONFLICT).
- `is_premium` is shown only while `premium_until` (if any) is in the
  future: identity publishes no event when a Premium period ends.

## Events

### Consumed: identity's account stream

| | |
| --- | --- |
| Topic | `agrogram.identity.account.v1` (`AccountEvent`, [`events/v1/account.proto`](../../proto/events/v1/account.proto)), key = user id |
| Consumer group | `profilesocial` (`PROFILE_SOCIAL_KAFKA_CONSUMER_GROUP`), shared with the transaction stream (one client reads both topics); a new group starts at the beginning of the topic |
| Dead letters | `agrogram.profilesocial.deadletter.v1` (`DeadLetter`, [`events/v1/metadata.proto`](../../proto/events/v1/metadata.proto)), key = original key, `consumer_group` = the group |

Processing follows the consumer rules of `metadata.proto`:

- **One path for every event type.** Each record's `AccountSnapshot` is
  applied to the projection when its version is newer than the stored
  `account_version` (`INSERT … ON CONFLICT (id) DO UPDATE … WHERE
  profiles.account_version < EXCLUDED.account_version`, which also creates
  the PENDING row). `AccountCreated`, `AccountRoleChanged` (verified-farmer
  badge + roles) and `AccountPremiumChanged` (Premium badge) change it;
  `FarmerVerified`, `FarmerVerificationRejected` and
  `FarmerVerificationExpired` repeat the current version and are no-ops (the
  badge changes with the `AccountRoleChanged` published right after them). An
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

### Consumed: order-escrow's transaction stream

| | |
| --- | --- |
| Topic | `agrogram.orderescrow.transaction.v1` (`TransactionEvent`, [`events/v1/transaction.proto`](../../proto/events/v1/transaction.proto)), key = transaction id |
| Consumer group | the account stream's (`profilesocial`): one client, one group, both topics |
| Dead letters | `agrogram.profilesocial.deadletter.v1`, as for the account stream (`source_topic` = this topic) |

profile-social needs one fact from this stream: that a transaction between a
farmer and a reseller is complete (`events.TransactionHandler` calls
`service.RecordCompletedTransaction`).

- **Only COMPLETED snapshots matter.** `TransactionCreated` up to
  `ReceiptConfirmed` carry earlier states and are skipped without touching
  the database (no inbox row). `PayoutReleased` moves the snapshot to
  `TRANSACTION_STATE_COMPLETED`, which is final: it records the transaction
  in `completed_transactions`, which unlocks one rating per party and adds
  one to both parties' `deals_count`. `BonusAwarded` (still COMPLETED, the
  next version) and replays change nothing. Like the account handler it reads
  the snapshot, not the `oneof`, so an unknown event type is handled like any
  other; `completed_at` falls back to `metadata.occurred_at`.
- **Idempotency.** The event id goes into `processed_events` in the same
  transaction (a known id is a duplicate), and `completed_transactions` is
  keyed by the transaction id (`ON CONFLICT (id) DO NOTHING`), so a deal
  counts once per transaction, not once per event. That is stricter than
  `metadata.proto`'s once-per-`event_id` rule for facts, on purpose: two
  events carry COMPLETED. No version guard is needed: COMPLETED is final and a
  recorded transaction is only ever inserted, so a late or replayed event
  cannot undo it, and since `completed_transactions` is never purged, a replay
  after the inbox purge counts nothing twice either.
- **Poison records**: besides the content-type, decoding and protovalidate
  checks, a record whose `metadata.aggregate_type` is not
  `RESOURCE_TYPE_TRANSACTION`, or whose snapshot `id` / `version` differs from
  `metadata.aggregate_id` / `aggregate_version`, is dead-lettered at once,
  whatever its state. A record key other than the transaction id is only
  logged (warning).
- **Outages, other failures and offsets** are handled exactly as for the
  account stream: it is the same consumer with the same retry budget,
  backoff and `postgres.IsTransient`.
- **Backfill.** A topic newly added to an existing group has no committed
  offsets, so it starts at its beginning like a new group does: the first
  start with this stream records every transaction order-escrow has already
  completed (within the topic's retention).

### Published: the profile stream

Transactional outbox (`outbox` table, written in the state change's
transaction) plus a relay goroutine (the same relay as identity: franz-go
idempotent producer, acks=all, batches locked with `FOR UPDATE SKIP LOCKED`,
exponential backoff). Published rows are purged after
`PROFILE_SOCIAL_OUTBOX_RETENTION`.

| Topic | Key | Envelope | When |
| --- | --- | --- | --- |
| `agrogram.profilesocial.profile.v1` | user id | `ProfileEvent{metadata, profile, rating_updated}` ([`events/v1/profile.proto`](../../proto/events/v1/profile.proto)) | `RateUser` stored a rating for a user whose profile is ACTIVE |

Metadata: UUIDv7 `event_id`; `event_type` `agrogram.events.v1.RatingUpdated`;
producer `profile-social`; aggregate USER / the rated user id;
`aggregate_version` = the profile version after the rating; correlation id =
`x-request-id`; trace context when present; actor = the rater. The snapshot
carries display name, avatar id, region, district, the new rating summary,
counters, badges (Premium evaluated at the event time) and timestamps. Record
headers: `content-type`, `event-type`, `event-id`, `x-request-id`,
`traceparent`/`tracestate`. A rated user without an ACTIVE profile gets the
rating and aggregate but no event (the snapshot requires a display name).

### Ratings and completed transactions

`completed_transactions` records completed escrow transactions between a
farmer and a reseller; each unlocks one rating per party and adds one to both
parties' `deals_count`, once per transaction id
(`service.RecordCompletedTransaction`). Its rows come from order-escrow's
transaction stream (above): a transaction is recorded when its
`PayoutReleased` is consumed. Until then `RateUser` on it fails closed with
RATING_NOT_ALLOWED; without Kafka (`PROFILE_SOCIAL_KAFKA_BROKERS` empty)
nothing is ever recorded.

## Layout

```
services/profile-social/
├── cmd/profile-social/      main: serve (default) | migrate up|down|status | healthcheck
├── internal/
│   ├── apperr/              Error type -> gRPC status with one common.v1.Error
│   ├── auth/                x-agrogram-* metadata -> Principal; outgoing metadata for catalog
│   ├── catalogclient/       gRPC client for catalog ListListings -> ListingCard (lazy connection)
│   ├── config/              PROFILE_SOCIAL_* environment variables
│   ├── domain/              Profile, Account projection, Rating, CompletedTransaction, pages
│   ├── events/              ProfileEvent + DeadLetter builders, outbox relay, consumer,
│   │                        account and transaction record handlers
│   ├── grpcserver/          handler, interceptors (recovery, principal, logging, validation)
│   ├── health/              Postgres readiness -> gRPC health + /healthz, /readyz
│   ├── log/                 slog (JSON or text)
│   ├── ratelimit/           Redis fixed-window limiter (follow/unfollow)
│   ├── repository/postgres/ pgx v5 repository, outbox/inbox store, IsTransient, integration tests
│   └── service/             use cases: profiles, follows, ratings, account projection, listings
├── migrations/              goose SQL migrations (embedded)
├── Dockerfile               multi-stage, context back/, distroless nonroot
├── Dockerfile.dockerignore  keeps the context to gen/go + services/profile-social
├── compose.yaml             profile-social + profile-social-migrate on top of deploy/docker-compose.yml
├── Makefile                 `make help`
└── .env.example             settings for running on the host
```

Schema (`migrations/00001_init.sql`): `profiles` (PK user id; status,
public fields, `create_request_id`, `version`, counters, rating sum/count,
`deals_count`, account projection with `account_version`), `follows` (PK
`(follower_id, followee_id)`, no self-follow; indexes `(followee_id,
created_at DESC, follower_id DESC)` and `(follower_id, created_at DESC,
followee_id DESC)`), `completed_transactions` (PK transaction id, distinct
parties), `ratings` (unique `(transaction_id, rater_id)` and `(rater_id,
request_id)`, score 1-5, index `(rated_user_id, created_at DESC, id DESC)`),
`processed_events` (inbox, PK event id), `outbox` (as identity's).

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
| `PROFILE_SOCIAL_KAFKA_BROKERS` | empty | Comma-separated brokers; empty disables the consumer of both streams (no badge updates; `RateUser` stays RATING_NOT_ALLOWED) and the relay (warning) |
| `PROFILE_SOCIAL_KAFKA_CLIENT_ID` | `profile-social` | Kafka client id |
| `PROFILE_SOCIAL_KAFKA_CONSUMER_GROUP` | `profilesocial` | Consumer group of both consumed streams (account and transaction), also `DeadLetter.consumer_group` |
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
`agrogram.identity.account.v1` and `agrogram.orderescrow.transaction.v1`
(one group, `profilesocial`) and the outbox relay next to the gRPC server. On
first start the group reads both topics from the beginning, so every account
identity has published gets its PENDING row and every transaction
order-escrow has completed is recorded; a group that so far read only the
account stream starts the transaction topic at its beginning the same way.
`make infra-up` in `back/` creates both topics (the broker does not
auto-create topics); while the transaction topic is missing the consumer
keeps reading the account stream and picks the topic up once it exists.

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
through the account stream and order-escrow through the transaction stream;
it calls catalog for `ListUserListings`. order-escrow calls catalog too
(`CreateTransaction` snapshots the listing through `GetListing`), so catalog
must run before transactions can be created. Local binaries, one terminal
each (each with its `.env`; the `.env.example` files already point
profile-social and order-escrow at catalog on `127.0.0.1:15054`):

```sh
(cd ../identity && make migrate-up && make run)          # 127.0.0.1:15051
(cd ../auth-gateway && make migrate-up && make run)      # 127.0.0.1:15052 (needs make dev-key once)
(cd ../catalog && make migrate-up && make run)           # 127.0.0.1:15054
(cd ../order-escrow && make migrate-up && make run)      # 127.0.0.1:15055 (Java)
make migrate-up && make run                              # 127.0.0.1:15053
```

Or all five in Compose, from the directory that contains `back/`
(profile-social's compose file points at `catalog:50051`; order-escrow's
too):

```sh
docker compose -f back/deploy/docker-compose.yml \
  -f back/services/identity/compose.yaml \
  -f back/services/auth-gateway/compose.yaml \
  -f back/services/profile-social/compose.yaml \
  -f back/services/catalog/compose.yaml \
  -f back/services/order-escrow/compose.yaml \
  up -d --build --wait identity auth-gateway profile-social catalog order-escrow
```

Leaving out catalog's file keeps everything else working: `ListUserListings`
answers empty pages. Ratings unlock only once order-escrow publishes
`PayoutReleased` for a transaction between the two users (the reseller's
`ConfirmReceipt` completes it; see order-escrow's README for the grpcurl
walkthrough): profile-social then records the transaction, and each party
may rate the other once with its id.

### 6. Tests

```sh
make test                     # unit tests, no infrastructure (fakes, pgxmock, miniredis, kfake, bufconn)
make test-race                # the same with -race (needs a C compiler)
make lint                     # go vet (both build tags) + gofmt check
make test-integration         # -tags=integration against PROFILE_SOCIAL_TEST_DATABASE_URL
```

The unit tests cover profile create/update/delete, follows (idempotency,
counters, self-follow, pagination), ratings (eligibility, once per
transaction, aggregate, outbox), the account projection (duplicates,
older-after-newer, same-version repeats, unknown oneof case, event before and
after `CreateProfile`), completed transactions (decoding, earlier states
skipped, the `completed_at` fallback, poison metadata; `PayoutReleased`
applied, its redelivery a duplicate, `BonusAwarded` stale, `deals_count` once;
`RateUser` unlocked end to end through the transaction handler), and the
consumer on a kfake cluster (order, commits, poison and budget dead letters,
outages that are never dead-lettered, a shutdown that leaves the record in
progress uncommitted, both streams in one group, transaction dead letters).
The integration tests create a random schema (`profile_social_test_<hex>`),
migrate it through `search_path`, run the real repository and service
(version guard, concurrent follows, keyset pages, delete, ratings, the
completed-transaction inbox and idempotency, outbox, outages that are
transient), roll the migrations back and forward, and drop the schema.

### 7. Smoke test with grpcurl

With `PROFILE_SOCIAL_GRPC_REFLECTION=true` and identity + auth-gateway
running (section 5); the rating and listing steps also need order-escrow and
catalog:

```sh
PS=127.0.0.1:15053; AG=127.0.0.1:15052; ID=127.0.0.1:15051
uuid() { cat /proc/sys/kernel/random/uuid; }

# sign in (STUB Google token): identity creates the account and publishes AccountCreated
grpcurl -plaintext -d '{"google":{"id_token":"dev-google:1001:dilnoza@gmail.com"}}' \
  $AG agrogram.authgateway.v1.AuthGatewayService/SignIn
USER_ID=<user_id from the response>

grpcurl -plaintext -H "x-agrogram-user-id: $USER_ID" -d "{\"user_id\":\"$USER_ID\"}" \
  $PS agrogram.profilesocial.v1.ProfileSocialService/GetProfile          # NOT_FOUND: still PENDING
grpcurl -plaintext -H "x-agrogram-user-id: $USER_ID" -H 'x-agrogram-roles: RESELLER' \
  -d "{\"request_id\":\"$(uuid)\",\"profile\":{\"display_name\":\"Dilnoza\",
  \"region\":\"REGION_FARGONA\",\"district\":\"Quva\"}}" \
  $PS agrogram.profilesocial.v1.ProfileSocialService/CreateProfile

# badges follow identity: verify as a farmer, turn Premium on
grpcurl -plaintext -H "x-agrogram-user-id: $USER_ID" \
  -d "{\"request_id\":\"$(uuid)\",\"lease_contract_number\":\"PS-$(date +%s)\"}" \
  $ID agrogram.identity.v1.IdentityService/SubmitFarmerVerification
grpcurl -plaintext -H 'x-agrogram-caller: order-escrow' -d "{\"user_id\":\"$USER_ID\",\"is_premium\":true}" \
  $ID agrogram.identity.v1.IdentityService/SetAccountPremium
grpcurl -plaintext -d "{\"user_id\":\"$USER_ID\"}" \
  $PS agrogram.profilesocial.v1.ProfileSocialService/GetProfile          # is_verified_farmer, is_premium: true

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

# ratings fail closed until a transaction between the two completes
grpcurl -plaintext -H "x-agrogram-user-id: $OTHER" \
  -d "{\"request_id\":\"$(uuid)\",\"transaction_id\":\"$(uuid)\",\"score\":5}" \
  $PS agrogram.profilesocial.v1.ProfileSocialService/RateUser            # RATING_NOT_ALLOWED
# complete one in order-escrow: its README's grpcurl walkthrough with F=$USER_ID (an ACTIVE
# offer in catalog) and R=$OTHER, up to ConfirmReceipt, which publishes PayoutReleased
T=<transaction id>
grpcurl -plaintext -H "x-agrogram-user-id: $OTHER" \
  -d "{\"request_id\":\"$(uuid)\",\"transaction_id\":\"$T\",\"score\":5,\"comment\":\"Ishonchli\"}" \
  $PS agrogram.profilesocial.v1.ProfileSocialService/RateUser            # the rating; ALREADY_RATED once more
grpcurl -plaintext -d "{\"user_id\":\"$USER_ID\"}" \
  $PS agrogram.profilesocial.v1.ProfileSocialService/ListRatings         # count 1; GetProfile shows deals_count 1
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

- The profile stream has only `RatingUpdated`, so catalog cannot build owner
  cards (names, avatars) from it yet (`TODO(profile-stream)`).
- The consumer starts a new group at the beginning of the account topic.
  Accounts whose events were deleted by topic retention before profile-social
  first ran get no PENDING row; their badges stay off until their next
  account event (a backfill would need identity to re-publish snapshots).
- Likewise for the transaction topic: a transaction whose COMPLETED events
  were deleted by retention before profile-social first read the topic is
  never recorded, so its parties cannot rate each other and `deals_count`
  misses it (a backfill would need order-escrow to re-publish).
- `ListUserListings` calls catalog live, so a profile shows no listings while
  catalog is down. The proto's service comment has profile-social project
  catalog's listing stream (`agrogram.catalog.listing.v1`) instead;
  `internal/service/listings.go` describes how, should the live call become
  a latency or availability problem.
- The older migration comments (`migrations/00001_init.sql`, on
  `deals_count` and `completed_transactions`) still describe the transaction
  consumer as not built; applied migrations are not edited.
- A `common.v1.Role` value added to the proto later fails protovalidate
  (`defined_only`) until profile-social is regenerated, so such account
  events would be dead-lettered.
- Avatars are not checked against media-pipeline (existence, purpose
  AVATAR, ownership).
- Internal callers are recognized by `x-agrogram-caller` alone; the edge must
  strip client-supplied `x-agrogram-*` headers (mTLS / service identity
  later).
