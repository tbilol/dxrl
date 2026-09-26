# catalog

Go service that owns **both listing boards** (farmers' sell offers and
resellers' buy-requests), **listing search** and the **product reference
data**. Contract:
[`proto/catalog/v1/catalog.proto`](../../proto/catalog/v1/catalog.proto)
(`agrogram.catalog.v1.CatalogService`). Conventions (errors, metadata,
validation, events) are in [back/README.md](../../README.md).

catalog owns listings and product types only. A listing stores its owner's
user id; names, avatars, badges and ratings belong to profile-social, and the
owner card is composed at the edge (API gateway / client). catalog never
calls another service. It publishes the listing stream
(`agrogram.catalog.listing.v1`) through a transactional outbox and consumes
order-escrow's transaction stream (`agrogram.orderescrow.transaction.v1`):
escrow transactions commit listing quantity, which drives the remaining
quantity, SOLD_OUT and which transaction parties may read a listing.
profile-social calls `ListListings` (owner filter) for `ListUserListings`;
order-escrow calls `GetListing` to snapshot a listing into a transaction.

| RPC | Who may call | What it does | Errors |
| --- | --- | --- | --- |
| `CreateListing` | a signed-in user: offers need `ROLE_FARMER`, buy-requests `ROLE_RESELLER` | Posts an ACTIVE listing (version 1, remaining = quantity) and writes `ListingCreated` to the outbox in the same transaction. Idempotent on `request_id` (per owner): a retry returns the listing it created. Rate-limited per user | UNAUTHENTICATED; FARMER_VERIFICATION_REQUIRED (FAILED_PRECONDITION, offer without ROLE_FARMER); PERMISSION_DENIED (buy-request without ROLE_RESELLER); PLAN_LIMIT_REACHED (RESOURCE_EXHAUSTED, `metadata {plan, listing_kind, limit}`); VALIDATION_FAILED (also: product retired or not in the reference data); RATE_LIMITED |
| `GetListing` | anyone for ACTIVE listings; the owner, the parties of escrow transactions on it, staff (`ROLE_ADMIN`) and `x-agrogram-caller: order-escrow` for any status | Returns the listing, with the quantity escrow transactions have not committed as `remaining_quantity`. Opening an ACTIVE listing counts a view (not for the owner, staff or services) | NOT_FOUND (missing, or not visible to the caller) |
| `UpdateListing` | the owner only | Changes the `update_mask` fields (product, quantity, price, phone, address, notes, media); `kind` never changes; version + 1; equal values change nothing. Only ACTIVE listings are editable. Remaining = quantity − what escrow transactions committed; a quantity equal to that makes the listing SOLD_OUT | PERMISSION_DENIED (someone else's visible listing); NOT_FOUND; LISTING_NOT_ACTIVE (FAILED_PRECONDITION, `metadata.status`); INSUFFICIENT_QUANTITY (FAILED_PRECONDITION, a quantity below what escrow transactions committed, `metadata {listing_id, requested_grams, committed_grams}`); ABORTED / CONFLICT (stale `expected_version`, `metadata.current_version`); VALIDATION_FAILED |
| `DeleteListing` | the owner only | Soft delete: DELETED, `deleted_at`, version + 1, any prior status. Deleting a deleted listing is a no-op (so retries succeed) | PERMISSION_DENIED; NOT_FOUND; ABORTED / CONFLICT |
| `ListListings` | anyone for ACTIVE listings; the owner (`filter.owner_id` = caller) or staff for other statuses | A board (`filter.kind`) or one owner's listings, newest first, keyset page tokens, any `ListingFilter` field | PERMISSION_DENIED (other statuses for someone else's listings, `metadata.status`); INVALID_ARGUMENT (page token of another query) |
| `SearchListings` | as `ListListings` | Free text + filters + sort (RELEVANCE, CREATED_AT, PRICE per kg with negotiable last, QUANTITY) | as `ListListings`; VALIDATION_FAILED (RELEVANCE ascending, prices not in UZS, `min_price` > `max_price` across units) |
| `ListProductTypes` | anyone | The product reference data of a category (or all) by `sort_order`, retired ones included (flagged), names in uz and ru | — |

Every error is a gRPC status with exactly one `agrogram.common.v1.Error`
(domain `catalog`) in its details; codes follow the `ErrorReason` table.
Request validation is the protovalidate annotations of the proto
(INVALID_ARGUMENT / `VALIDATION_FAILED` with `field_violations`) plus what the
contract cannot express (trimmed text lengths, distinct media ids, a price
whose per-tonne value fits int64, the cross-unit price range).

Callers reach catalog through the edge, which checks the access token and
forwards the user as `x-agrogram-*` metadata (`x-agrogram-user-id`,
`x-agrogram-roles`, `x-agrogram-premium`, `x-agrogram-session-id`,
`x-agrogram-caller`, `x-request-id`, `traceparent`, `accept-language`).
Roles and Premium are the token-time snapshot.

## Listing rules

**Boards and roles.** `LISTING_KIND_OFFER` (farmers' board) needs
`ROLE_FARMER` (granted by identity after e-ijara verification):
FARMER_VERIFICATION_REQUIRED otherwise. `LISTING_KIND_BUY_REQUEST`
(resellers' board) needs `ROLE_RESELLER`, which every active account holds:
PERMISSION_DENIED otherwise (error.proto has no dedicated reason).

**Plan quotas.** An owner may have at most this many ACTIVE listings per
board; Premium is `x-agrogram-premium: true`:

| | Default | Premium |
| --- | --- | --- |
| Offers | 5 (`CATALOG_OFFER_QUOTA_DEFAULT`) | 20 (`CATALOG_OFFER_QUOTA_PREMIUM`) |
| Buy-requests | 5 (`CATALOG_BUY_REQUEST_QUOTA_DEFAULT`) | 25 (`CATALOG_BUY_REQUEST_QUOTA_PREMIUM`) |

- Only **ACTIVE** listings count: the quota limits what is on the board at
  once. CLOSED, SOLD_OUT, BLOCKED and DELETED listings are off the board, so
  deleting (or, later, closing) a listing frees a slot. Whether BLOCKED
  listings should keep holding a slot (so moderation cannot be sidestepped) is
  an open question.
- The boards are independent: a user with both roles has both quotas.
- A user who loses Premium keeps listings above the Default quota but cannot
  post on that board until below it.
- Atomicity: the creation transaction takes a per-owner advisory lock
  (`pg_advisory_xact_lock(hashtextextended('catalog:listings:<owner>', 0))`),
  then looks up the request id (idempotency), checks the product, counts and
  inserts. Concurrent creations of one owner therefore serialize; the
  integration test fires 12 at once and gets exactly 5.

**Visibility.** ACTIVE listings are public. Other statuses (CLOSED,
SOLD_OUT, BLOCKED, DELETED) are visible through `GetListing` to the owner,
the parties of escrow transactions on the listing (its farmer and reseller,
known from the transaction stream; catalog.proto), staff and order-escrow;
the party lookup in `listing_transactions` runs only when the other rules
say no. Lists and search return other statuses only for the caller's own
listings (`filter.owner_id` = caller) or to staff, and reject the request with
PERMISSION_DENIED otherwise (what profile-social's `ListUserListings`
expects). A listing someone may not see is NOT_FOUND, also for update and
delete; a party, who may see it, gets PERMISSION_DENIED there. The contact
phone is part of the listing (it is shown on it) but never published in
events.

**Remaining quantity and SOLD_OUT.** `remaining_grams` is the quantity minus
what escrow transactions have committed (see "Consumed: order-escrow's
transaction stream"), never below 0; a new listing has nothing committed. An
ACTIVE listing with nothing left becomes SOLD_OUT: off the boards and out of
search, no longer counted by the quota, readable as above, not editable, and
final (nothing releases a commitment yet). `UpdateListing` recomputes the
remaining quantity under the listing's row lock, which the consumer takes
too: a quantity below the committed one is INSUFFICIENT_QUANTITY
(`metadata {listing_id, requested_grams, committed_grams}`), an equal one
makes the listing SOLD_OUT.

**Views.** `views_count` counts `GetListing` of ACTIVE listings by anyone but
the owner, staff and services. Views are queued in memory (never blocking or
failing the read), de-duplicated per viewer and listing for
`CATALOG_VIEWS_DEDUP_WINDOW` in Redis (`catalog:views:<listing>:<viewer>`,
`SET NX`; anonymous views always count), and added every
`CATALOG_VIEWS_FLUSH_INTERVAL` with one `UPDATE … FROM unnest(…)` per batch
that does not bump `version` or `updated_at`. A full queue, a Redis failure
(fail open: every view counts) or a failed write only loses or over-counts
views. Queued views are written at shutdown.

## Search

- **Document.** Each listing stores `search_document tsvector`: the product's
  uz and ru names and key (weight A), the district (B) and the notes (C),
  written with every insert and update of those fields. The service
  normalizes the text before Postgres sees it, identically for listings and
  queries: Unicode lower case, `ё` → `е`, apostrophes (`'` `ʻ` `ʼ` `‘` `’`
  `` ` `` `´`) removed so `Bug'doy`, `Bugʻdoy` and `bugdoy` (the key) are one
  word, and every other non-letter/digit separates words. Postgres parses it
  with the built-in `'simple'` configuration, so no extension is needed and
  the result does not depend on the database locale. GIN index on ACTIVE
  listings.
- **Query.** Up to 10 distinct words, each a prefix match (`'kart':*`), all
  required (`&`): `kart` finds kartoshka, `картоф` finds Картофель,
  `kartoshka urgut` needs both. Words are letters and digits only and quoted,
  so user input cannot change the tsquery.
- **Filters** (`ListingFilter`): kind, product types, regions, district
  (exact, case-insensitive), owner, statuses, `min_quantity` (remaining
  grams), `min_price` / `max_price` in any unit (both exclude negotiable
  listings). Prices are compared as `price_per_tonne_minor`, a generated
  column equal to the price per kg × 1000, exact for prices quoted per kg and
  per tonne alike.
- **Sort.** RELEVANCE (`ts_rank`, default when there is text; descending only;
  without text it falls back to CREATED_AT), CREATED_AT, PRICE (per kg;
  negotiable listings last in both directions), QUANTITY (remaining).
  Direction defaults to descending; ties go newest first (created_at, id).
- **Page tokens** are opaque base64url JSON: a layout version, a fingerprint
  (SHA-256 of the operation, the normalized and authorized criteria, the
  search words and the sort) and the keyset position of the last listing
  (sort key, text rank, created_at, id). A token of another query, operation
  or layout is INVALID_ARGUMENT (`page.page_token`); the page size may change
  between pages. `total_size` is left unset (not cheap to count).

## Events

### Consumed: order-escrow's transaction stream

| | |
| --- | --- |
| Topic | `agrogram.orderescrow.transaction.v1` (`CATALOG_KAFKA_TRANSACTION_TOPIC`; `TransactionEvent`, [`events/v1/transaction.proto`](../../proto/events/v1/transaction.proto)), key = transaction id |
| Consumer group | `catalog` (`CATALOG_KAFKA_CONSUMER_GROUP`); a new group starts at the beginning of the topic |
| Dead letters | `agrogram.catalog.deadletter.v1` (`DeadLetter`, [`events/v1/metadata.proto`](../../proto/events/v1/metadata.proto)), key = original key, `consumer_group` = the group, `source_topic` = the transaction topic |

`events.TransactionHandler` decodes each record and hands its snapshot to
`service.ApplyTransactionEvent`, which keeps `listing_transactions`: one row
per escrow transaction (listing, farmer, reseller, quantity, state, version).

- **The commitment is the first event of a transaction catalog sees.**
  Whatever its type (normally `TransactionCreated`, state AWAITING_PAYMENT),
  it inserts the transaction's row, and the listing's remaining quantity
  becomes its quantity minus the quantities of all its rows, never below 0
  (recomputed from the sum, never decremented, so a replay cannot count
  twice); an ACTIVE listing with nothing left becomes SOLD_OUT. A change bumps
  the listing's `version` and `updated_at`. Why at creation: order-escrow's
  oversell guard counts every transaction from its creation, unpaid and
  completed alike (available = listing quantity minus the grams of all its
  transactions), so committing then keeps catalog's remaining quantity equal
  to what order-escrow will still sell. Committing later (at `PaymentHeld` or
  `PayoutReleased`) would advertise quantity order-escrow already refuses
  with INSUFFICIENT_QUANTITY, and listing.proto defines SOLD_OUT as "the
  whole quantity is committed to escrow transactions". Any event commits,
  not only `TransactionCreated`: every event carries the terms, which are
  fixed at creation, so a lost or expired `TransactionCreated` loses nothing.
- **Later events** move the row's `state` and `version` when their version is
  newer (`PaymentHeld` … `PayoutReleased`, then `BonusAwarded`, still
  COMPLETED) and never touch the listing; the sold quantity is derivable from
  `state` later. The handler reads the snapshot, never the `oneof`, so an
  event type newer than this code is applied like any other.
- **No release.** The contract has no cancel or expiry event (order-escrow's
  `TODO(cancel)`): a commitment is permanent and SOLD_OUT is final. A listing
  that is CLOSED, BLOCKED, DELETED or already SOLD_OUT keeps its status; only
  its remaining quantity changes.
- **Idempotency and versions** (the consumer rules of `metadata.proto`). The
  event id goes into `processed_events` in the same database transaction as
  the change; a known id is a duplicate and changes nothing. An older version
  after a newer one is stale. Commitments are keyed by transaction id, so a
  replay after the inbox purge (`CATALOG_INBOX_RETENTION`) is stale too. Each
  event runs under the listing's row lock (`FOR NO KEY UPDATE`), which
  serializes it with `UpdateListing`, `DeleteListing` and the other
  transactions on the listing, including ones read from other partitions or
  by other replicas.
- **Dead-lettered at once** (`attempts` 1, `ERROR_REASON_VALIDATION_FAILED`,
  `metadata.failure = poison`): records that are not
  `application/x-protobuf`, do not decode, fail protovalidate, contradict
  their metadata (`aggregate_type` not TRANSACTION, snapshot `id` / `version`
  not `aggregate_id` / `aggregate_version`) or commit no quantity; and events
  that contradict catalog's data (`domain.ErrInconsistentEvent`): the listing
  does not exist, has another kind, or is not owned by the owner-side party
  (the farmer of an offer, the reseller of a buy-request), or the
  transaction's listing, parties or quantity differ from an earlier event.
  They leave no inbox row. A record key other than the transaction id is
  only logged (warning).
- **Outages** (Postgres unreachable, credentials refused, schema not
  migrated, timeouts, serialization failures; `postgres.IsTransient`) are
  retried with exponential backoff for as long as they last: the record is
  neither skipped nor dead-lettered and the partition does not advance.
- **Any other failure** is retried `CATALOG_CONSUMER_MAX_ATTEMPTS` times with
  backoff, then dead-lettered (`ERROR_REASON_INTERNAL`,
  `metadata.failure = retries_exhausted`) and the consumer moves on.
- **Offsets** are committed after each poll batch, only for records that
  were processed or dead-lettered (at-least-once). A dead letter that cannot
  be published is retried until it is; its record stays uncommitted. A
  rebalance or a crash between processing and the commit only redelivers
  records, which the inbox absorbs.
- **Backfill.** A new group starts at the beginning of the topic, so the
  first start commits every transaction still retained there (7 days: the
  broker default, since `deploy/kafka/create-topics.sh` sets no retention on
  stream topics); older ones are never seen (Known gaps).

### Published: the listing stream

Transactional outbox (`outbox` table, written in the creation transaction)
plus a relay goroutine (the same relay as identity and profile-social:
franz-go idempotent producer, acks=all, batches locked with `FOR UPDATE SKIP
LOCKED`, exponential backoff). Published rows are purged after
`CATALOG_OUTBOX_RETENTION`.

| Topic | Key | Envelope | When |
| --- | --- | --- | --- |
| `agrogram.catalog.listing.v1` | listing id | `ListingEvent{metadata, listing, listing_created}` ([`events/v1/listing.proto`](../../proto/events/v1/listing.proto)) | `CreateListing` created a listing |

Metadata: UUIDv7 `event_id`; `event_type` `agrogram.events.v1.ListingCreated`;
producer `catalog`; aggregate LISTING / the listing id; `aggregate_version`
= the listing version (1); correlation id = `x-request-id`; trace context when
present; actor = the owner. The snapshot has no phone (metadata.proto: never
publish contact details), no owner card and no views. Record headers:
`content-type`, `event-type`, `event-id`, `x-request-id`,
`traceparent`/`tracestate`.

listing.proto has no update, close or delete events yet, so `UpdateListing`,
`DeleteListing` and the consumer's commitments (remaining quantity, SOLD_OUT)
publish nothing (`TODO(events)`).

## Layout

```
services/catalog/
├── cmd/catalog/             main: serve (default) | migrate up|down|status | healthcheck
├── internal/
│   ├── apperr/              Error type -> gRPC status with one common.v1.Error
│   ├── auth/                x-agrogram-* metadata -> Principal
│   ├── config/              CATALOG_* environment variables
│   ├── domain/              Listing, ProductType, filters, queries, quotas, proto conversions
│   ├── events/              ListingEvent + DeadLetter builders, outbox relay, transaction-stream consumer
│   ├── grpcserver/          handler, interceptors (recovery, principal, logging, validation)
│   ├── health/              Postgres readiness -> gRPC health + /healthz, /readyz
│   ├── log/                 slog (JSON or text)
│   ├── ratelimit/           Redis fixed-window limiter (listing creation)
│   ├── repository/postgres/ pgx v5 repository, search query builder, outbox, inbox, transaction projection, integration tests
│   ├── service/             use cases: listings, list/search, page tokens, text normalization, product types, escrow commitments
│   └── views/               background view counter (batching, Redis de-duplication)
├── migrations/              goose SQL migrations (embedded) + seed test
├── Dockerfile               multi-stage, context back/, distroless nonroot
├── Dockerfile.dockerignore  keeps the context to gen/go + services/catalog
├── compose.yaml             catalog + catalog-migrate on top of deploy/docker-compose.yml
├── Makefile                 `make help`
└── .env.example             settings for running on the host
```

Schema:

- `migrations/00001_init.sql`: `product_types` (PK = the `ProductType`
  number; unique `key`; `name_uz`, `name_ru`, `category`, `sort_order`,
  `is_retired`), `listings` (PK UUIDv7; `kind`, `owner_id`, `product_type`
  → `product_types`, `quantity_grams` + `quantity_unit`, `remaining_grams`,
  `price_minor` + `price_currency` + `price_unit` (all NULL = negotiable),
  generated `price_per_tonne_minor`, `phone`, `region`, `district`,
  `address_line`, `latitude`/`longitude`, `notes`, `media_ids uuid[]`,
  `status`, `views_count`, `version`, `create_request_id` (unique per owner),
  timestamps, `search_document tsvector`), `outbox` (as identity's). Indexes:
  ACTIVE listings by `(created_at, id)`, by kind, product and region (each
  then `created_at DESC, id DESC`), `(owner_id, created_at DESC, id DESC)`,
  GIN on `search_document` for ACTIVE listings.
- `migrations/00002_seed_product_types.sql`: one row per defined
  `common.v1.ProductType` (44 crops): id = the enum number; key = the value
  name without `PRODUCT_TYPE_`, lower case, `_` → `-`; uz / ru names from the
  value's comment in `product.proto`; category = its section there
  (GRAIN_INDUSTRIAL, VEGETABLE, LEGUME, MELON, FRUIT, NUT); `sort_order` =
  number × 10; none retired. `migrations/seed_test.go` fails when a defined
  value has no row (or two), a row has no value, or a key, name or category
  differs from product.proto. A product added to the enum gets its row in a
  new migration.
- `migrations/00003_listing_transactions.sql`: `listing_transactions` (PK =
  the transaction id; `listing_id` → `listings`; `farmer_id` and
  `reseller_id`, distinct; `quantity_grams` > 0; `state`, AWAITING_PAYMENT …
  COMPLETED; `version`; the transaction's `created_at`; `recorded_at`, the
  commitment; `updated_at`), indexed by `listing_id`: the projection of
  order-escrow's transaction stream behind the remaining quantity, SOLD_OUT
  and party visibility. `processed_events`: the consumer inbox (as
  profile-social's). The Down drops both tables only: listings keep the
  remaining quantities and statuses the commitments made, and rebuilding the
  projection after migrating up again needs the consumer group reset to the
  start of the topic.

## Configuration

Environment variables only (`CATALOG_` prefix). Missing or invalid values stop
the start-up with one error that lists all of them.

| Variable | Default | Meaning |
| --- | --- | --- |
| `CATALOG_DATABASE_URL` | **required** | Postgres URL (role and database `catalog`) |
| `CATALOG_DATABASE_MAX_CONNS` | `10` | pgx pool size |
| `CATALOG_REDIS_URL` | empty | Redis for the creation rate limit (`catalog:ratelimit:*`) and view de-duplication (`catalog:views:*`); empty disables both (warning) |
| `CATALOG_KAFKA_BROKERS` | empty | Comma-separated brokers; empty disables the outbox relay and the transaction-stream consumer (warning): escrow transactions then reduce no remaining quantity, no listing becomes SOLD_OUT and transaction parties cannot read listings that are not ACTIVE |
| `CATALOG_KAFKA_CLIENT_ID` | `catalog` | Kafka client id |
| `CATALOG_KAFKA_CONSUMER_GROUP` | `catalog` | Consumer group of order-escrow's transaction stream, also `DeadLetter.consumer_group`; must not be blank when brokers are set |
| `CATALOG_KAFKA_TRANSACTION_TOPIC` | `agrogram.orderescrow.transaction.v1` | Topic of order-escrow's transaction stream; must not be blank when brokers are set |
| `CATALOG_GRPC_ADDR` | `:50051` | gRPC listen address |
| `CATALOG_HTTP_ADDR` | `:8081` | `/healthz`, `/readyz` (also used by `catalog healthcheck`) |
| `CATALOG_GRPC_REFLECTION` | `false` | gRPC server reflection (grpcurl); local only |
| `CATALOG_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `CATALOG_LOG_FORMAT` | `json` | `json` or `text` |
| `CATALOG_OFFER_QUOTA_DEFAULT` | `5` | ACTIVE offers per owner, Default plan |
| `CATALOG_OFFER_QUOTA_PREMIUM` | `20` | ACTIVE offers per owner, Premium |
| `CATALOG_BUY_REQUEST_QUOTA_DEFAULT` | `5` | ACTIVE buy-requests per owner, Default plan |
| `CATALOG_BUY_REQUEST_QUOTA_PREMIUM` | `25` | ACTIVE buy-requests per owner, Premium |
| `CATALOG_CREATE_RATE_LIMIT` | `30` | Listing creations per user per window (fails open without Redis) |
| `CATALOG_CREATE_RATE_WINDOW` | `1h` | Rate-limit window |
| `CATALOG_CONSUMER_MAX_ATTEMPTS` | `5` | Retry budget of a failing record (outages excluded) before it is dead-lettered |
| `CATALOG_CONSUMER_RETRY_BACKOFF` | `500ms` | First retry delay, doubled per attempt |
| `CATALOG_CONSUMER_RETRY_BACKOFF_MAX` | `30s` | Retry delay cap (at least `CATALOG_CONSUMER_RETRY_BACKOFF`) |
| `CATALOG_CONSUMER_HANDLE_TIMEOUT` | `10s` | One processing attempt |
| `CATALOG_CONSUMER_BATCH_SIZE` | `100` | Records per poll (offsets are committed per batch) |
| `CATALOG_INBOX_RETENTION` | `720h` | Processed event ids older than this are deleted, hourly (`0` keeps them) |
| `CATALOG_VIEWS_FLUSH_INTERVAL` | `2s` | How often queued views are written |
| `CATALOG_VIEWS_BUFFER_SIZE` | `10000` | Queued views; more are dropped until the next flush |
| `CATALOG_VIEWS_DEDUP_WINDOW` | `24h` | A viewer counts once per listing per window (Redis); `0` counts every view |
| `CATALOG_OUTBOX_INTERVAL` | `1s` | Relay poll interval when idle |
| `CATALOG_OUTBOX_BATCH_SIZE` | `100` | Messages per relay batch |
| `CATALOG_OUTBOX_MAX_BACKOFF` | `30s` | Relay backoff cap while Kafka/Postgres fail |
| `CATALOG_OUTBOX_RETENTION` | `168h` | Published outbox rows older than this are deleted (`0` keeps them) |
| `CATALOG_HEALTH_INTERVAL` | `5s` | Postgres ping period (readiness) |
| `CATALOG_SHUTDOWN_TIMEOUT` | `20s` | Graceful shutdown budget |
| `CATALOG_TEST_DATABASE_URL` | empty | Integration tests only; unset = skipped |

Ports: in the container gRPC `:50051` and HTTP `:8081`; on the host (both
`.env.example` and `compose.yaml`) `127.0.0.1:15054` and `127.0.0.1:18054`
(identity 15051/18051, auth-gateway 15052/18052, profile-social 15053/18053);
`catalog:50051` inside the compose network.

## Commands

Everything runs from `back/services/catalog`. Go targets need Go (the go.mod
asks for 1.26+, toolchain go1.27.1; `GOTOOLCHAIN=auto` downloads it). The
local infrastructure is `make infra-up` in `back/` (see
[deploy/README.md](../../deploy/README.md)); it creates the `catalog`
database and role and the Kafka topics.

### 1. Code generation

```sh
make gen     # the shared back/tools/codegen.sh: pinned buf 1.73.0, writes back/gen/go/go.mod
```

### 2. Settings and migrations (host)

```sh
cp .env.example .env          # then set the passwords from back/deploy/.env
make migrate-up               # goose, embedded SQL: schema, product seed, transaction projection + inbox (00003)
make migrate-status           # all three migrations "Applied"
make migrate-down             # rolls back the latest one: 00003 first, then the seed (fails while listings exist)
```

### 3. Run locally

```sh
make run                      # loads .env; gRPC 127.0.0.1:15054, HTTP 127.0.0.1:18054
curl -i http://127.0.0.1:18054/readyz
```

With `CATALOG_KAFKA_BROKERS` set, `serve` starts the consumer of
`agrogram.orderescrow.transaction.v1` (group `catalog`) and the outbox relay
next to the gRPC server. On first start the group reads the topic from the
beginning, so every transaction order-escrow has published within the
topic's retention commits its quantity. `make infra-up` in `back/` creates
the topic (the broker does not auto-create topics); while it is missing the
consumer logs fetch warnings and picks the topic up once it exists.

### 4. Run in Docker Compose (with the infra stack)

```sh
make compose-up               # builds the image, runs catalog-migrate, waits until healthy
make compose-logs
make compose-down             # removes only the catalog containers
```

### 5. Run alongside the other services

catalog needs only Postgres (and Kafka for its streams) to start.
profile-social calls it for `ListUserListings`; order-escrow calls
`GetListing` when a transaction is created and publishes the transaction
stream catalog consumes. Local binaries, one terminal each (each with its
`.env`; the `.env.example` files already point profile-social and
order-escrow at catalog on `127.0.0.1:15054`):

```sh
(cd ../identity && make migrate-up && make run)          # 127.0.0.1:15051
(cd ../auth-gateway && make migrate-up && make run)      # 127.0.0.1:15052 (needs make dev-key once)
(cd ../profile-social && make migrate-up && make run)    # 127.0.0.1:15053
(cd ../order-escrow && make migrate-up && make run)      # 127.0.0.1:15055 (Java)
make migrate-up && make run                              # 127.0.0.1:15054
```

Or all five in Compose, from the directory that contains `back/` (the
profile-social and order-escrow compose files point at `catalog:50051` by
default):

```sh
docker compose -f back/deploy/docker-compose.yml \
  -f back/services/identity/compose.yaml \
  -f back/services/auth-gateway/compose.yaml \
  -f back/services/profile-social/compose.yaml \
  -f back/services/catalog/compose.yaml \
  -f back/services/order-escrow/compose.yaml \
  up -d --build --wait identity auth-gateway profile-social catalog order-escrow
```

### 6. Tests

```sh
make test                     # unit tests, no infrastructure (fakes, pgxmock, miniredis, kfake, bufconn)
make test-race                # the same with -race (needs a C compiler)
make lint                     # go vet (both build tags) + gofmt check
make test-integration         # -tags=integration against CATALOG_TEST_DATABASE_URL
```

The unit tests cover creation (roles, the four quotas, per-board
independence, only ACTIVE counting, configurable quotas, idempotency,
retired/unknown products, validation and trimming, the rate limit and its
fail-open), visibility (owner, transaction parties, staff, order-escrow,
others), view counting, update (mask, no-op, version guard,
LISTING_NOT_ACTIVE, retired products, re-indexing, INSUFFICIENT_QUANTITY and
SOLD_OUT against the committed quantity), delete (soft, idempotent, version
guard), list/search (statuses and PERMISSION_DENIED, keyset paging, token
binding, sort resolution, price normalization across units, cursor keys for
every sort), text normalization, the SQL builder and row mapping (pgxmock),
the event envelope (no phone), the relay (kfake), the view counter
(miniredis), the gRPC layer (validation, error details, mapping), the seed
against the generated enum and product.proto, and the transaction stream:
decoding every event type and an unknown `oneof` case, poison records, the
handler (key warning, retryable failures, inconsistencies as poison), dead
letters, the consumer on a kfake cluster (order, commits, poison and
inconsistent records dead-lettered at once, outages never dead-lettered, the
retry budget, a shutdown that leaves the record in progress uncommitted,
dead-letter publishing retried, redeliveries), the commitments
(TransactionCreated commits once, duplicates, later and older events, any
first event commits, SOLD_OUT off the boards, the clamp at 0, off-board
statuses kept, inconsistent events writing nothing, outages), one run
through the handler with real records, `IsTransient`, the state mapping
against the migration, and the consumer settings. The integration tests
create a random schema (`catalog_test_<hex>`), migrate it through
`search_path`, check the seed, run the real repository and service (round
trip, outbox row, 12 concurrent creations against a quota of 5, version
guard, views, delete), search (prefixes, uz/ru/apostrophe variants,
multi-word, relevance order, every filter, price and quantity sorts with
negotiable listings last, paging without gaps or repeats), the transaction
projection and inbox (de-duplication, version guard, committed sum, parties,
constraints), commitments through the real store (applied, duplicate,
stale, SOLD_OUT, party visibility, INSUFFICIENT_QUANTITY), ten commitments
and their redeliveries applied concurrently to one listing, the inbox purge
with a replay after it, an unreachable database being transient, roll
migration 00003 and all migrations back and forward, and drop the schema.

### 7. Smoke test with grpcurl

With `CATALOG_GRPC_REFLECTION=true` and auth-gateway + identity running (for
real user ids; any UUID works for catalog alone):

```sh
CAT=127.0.0.1:15054
uuid() { cat /proc/sys/kernel/random/uuid; }
FARMER=$(uuid); RESELLER=$(uuid)

grpcurl -plaintext -d '{"category":"PRODUCT_CATEGORY_VEGETABLE"}' $CAT agrogram.catalog.v1.CatalogService/ListProductTypes

# an offer needs ROLE_FARMER: FARMER_VERIFICATION_REQUIRED for a plain reseller
grpcurl -plaintext -H "x-agrogram-user-id: $RESELLER" -H 'x-agrogram-roles: RESELLER' \
  -d "{\"request_id\":\"$(uuid)\",\"listing\":{\"kind\":\"LISTING_KIND_OFFER\",\"product\":\"PRODUCT_TYPE_KARTOSHKA\",
  \"quantity\":{\"grams\":\"20000000\",\"display_unit\":\"MASS_UNIT_TONNE\"},\"phone\":\"+998901234567\",
  \"address\":{\"region\":\"REGION_SAMARQAND\",\"district\":\"Urgut\"}}}" \
  $CAT agrogram.catalog.v1.CatalogService/CreateListing

# the same as a verified farmer, with a price of 4 500 so'm per kg
grpcurl -plaintext -H "x-agrogram-user-id: $FARMER" -H 'x-agrogram-roles: RESELLER,FARMER' -H 'x-agrogram-premium: false' \
  -d "{\"request_id\":\"$(uuid)\",\"listing\":{\"kind\":\"LISTING_KIND_OFFER\",\"product\":\"PRODUCT_TYPE_KARTOSHKA\",
  \"quantity\":{\"grams\":\"20000000\",\"display_unit\":\"MASS_UNIT_TONNE\"},
  \"price\":{\"amount\":{\"amount_minor\":\"450000\",\"currency_code\":\"UZS\"},\"unit\":\"MASS_UNIT_KILOGRAM\"},
  \"phone\":\"+998901234567\",\"address\":{\"region\":\"REGION_SAMARQAND\",\"district\":\"Urgut\"},\"notes\":\"Sifatli\"}}" \
  $CAT agrogram.catalog.v1.CatalogService/CreateListing
LISTING=<listing.id from the response>

grpcurl -plaintext -d "{\"listing_id\":\"$LISTING\"}" $CAT agrogram.catalog.v1.CatalogService/GetListing
grpcurl -plaintext -d '{"query":"kart","filter":{"regions":["REGION_SAMARQAND"]},"page":{"page_size":10}}' \
  $CAT agrogram.catalog.v1.CatalogService/SearchListings
grpcurl -plaintext -d '{"sort_by":"LISTING_SORT_FIELD_PRICE","sort_direction":"SORT_DIRECTION_ASC"}' \
  $CAT agrogram.catalog.v1.CatalogService/SearchListings
grpcurl -plaintext -H "x-agrogram-user-id: $FARMER" -H 'x-agrogram-roles: RESELLER,FARMER' \
  -d "{\"listing\":{\"id\":\"$LISTING\",\"notes\":\"Yangi hosil\"},\"update_mask\":\"notes\",\"expected_version\":\"1\"}" \
  $CAT agrogram.catalog.v1.CatalogService/UpdateListing
grpcurl -plaintext -H "x-agrogram-user-id: $FARMER" -d "{\"filter\":{\"owner_id\":\"$FARMER\"}}" \
  $CAT agrogram.catalog.v1.CatalogService/ListListings
grpcurl -plaintext -H "x-agrogram-user-id: $FARMER" -d "{\"listing_id\":\"$LISTING\",\"expected_version\":\"2\"}" \
  $CAT agrogram.catalog.v1.CatalogService/DeleteListing

# through profile-social (PROFILE_SOCIAL_CATALOG_ADDR set): the owner's listings
grpcurl -plaintext -d "{\"user_id\":\"$FARMER\"}" 127.0.0.1:15053 agrogram.profilesocial.v1.ProfileSocialService/ListUserListings

grpcurl -plaintext -d '{"service":"agrogram.catalog.v1.CatalogService"}' $CAT grpc.health.v1.Health/Check
```

`ListingCreated` records are visible in Kafka UI (http://127.0.0.1:18080,
topic `agrogram.catalog.listing.v1`). The transaction consumer's progress is
visible there too (consumer group `catalog`); dead letters land on
`agrogram.catalog.deadletter.v1`. To see a commitment, start a transaction on
the listing through order-escrow (its README has the grpcurl walkthrough):
`GetListing` then shows the lower `remaining_quantity`, the buyer can still
read the listing once it is SOLD_OUT, and a quantity below the committed one
fails with INSUFFICIENT_QUANTITY.

## Stubs

`// STUB:` marks every stand-in:

| Where | Stands in for |
| --- | --- |
| `internal/service/listings.go` (`checkMedia`) | The Premium rule for listing video (PREMIUM_REQUIRED): a request carries media ids only, and nothing can tell a video from an image until media-pipeline exists |

## TODOs

- `TODO(events)`: listing.proto has only `ListingCreated`; publish
  `ListingUpdated` / `ListingClosed` / `ListingDeleted` from `UpdateListing`
  and `DeleteListing`, and the new remaining quantity / SOLD_OUT from the
  transaction consumer, once it defines them (`internal/service/listings.go`,
  `internal/service/transactions.go`, `internal/events/events.go`).
- `TODO(proto)`: catalog.proto has no CloseListing RPC and `update_mask`
  cannot name `status`, so status CLOSED ("closed by the owner") cannot be
  reached yet (`internal/service/listings.go`).
- `TODO(edge)`: owner cards (name, avatar, rating, badges) and owner-name
  search are composed by the API gateway / client; catalog sets
  `owner.user_id` only (`internal/grpcserver/handler.go`,
  `internal/service/search.go`, `internal/service/service.go`).
- `TODO(media)`: media carry `media_id` only; type, status, URLs, ownership
  and purpose checks come from media-pipeline (`internal/domain/proto.go`,
  `internal/domain/domain.go`, `internal/grpcserver/handler.go`,
  `internal/events/events.go`, `internal/service/listings.go`,
  `migrations/00001_init.sql`).
- `TODO(security)`: `x-agrogram-caller: order-escrow` is trusted as is
  (`internal/service/service.go`).

## Known gaps

- No cancel or expiry: the contract has no such transaction event
  (order-escrow's `TODO(cancel)`), so every transaction, unpaid ones
  included, reserves its quantity for good and SOLD_OUT is final. A release
  needs a new transaction event (and catalog recomputing the remaining
  quantity without that transaction).
- Commitments publish no `ListingEvent`: listing.proto has no case for a
  changed remaining quantity or SOLD_OUT (`TODO(events)`), so the listing
  stream's consumers keep the `ListingCreated` snapshot (full quantity,
  ACTIVE).
- A transaction whose events were deleted by the topic's retention (7 days)
  before catalog first read the topic is never committed: its quantity stays
  remaining in catalog while order-escrow counts it (a backfill would need
  order-escrow to re-publish snapshots).
- The remaining quantity trails order-escrow by the consumer's lag; for that
  moment a listing may show quantity order-escrow has just committed (its
  oversell guard is authoritative). An owner lowering the quantity in that
  moment can end up below the commitments; the remaining quantity is then
  clamped at 0 (a warning is logged).
- The comment on `remaining_grams` in `migrations/00001_init.sql` still says
  it equals the quantity until the transaction stream is consumed; applied
  migrations are not edited.
- Product labels live in the search documents: relabelling a product in a
  later migration must also recompute `search_document` of its listings.
- Search matches word prefixes, not typos or transliterations (Uzbek
  Cyrillic "картошка" does not find "kartoshka"); `pg_trgm` or a synonym
  table would help and the role may create that extension.
- Views are best effort (see above); `views_count` lags by up to one flush
  interval.
- Internal callers are recognized by `x-agrogram-caller` alone; the edge must
  strip client-supplied `x-agrogram-*` headers.
