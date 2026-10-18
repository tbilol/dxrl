# catalog

Go service that owns **both listing boards** (farmers' sell offers and
resellers' buy-requests), **listing search** and the **product reference
data**. Contract:
[`proto/catalog/v1/catalog.proto`](../../proto/catalog/v1/catalog.proto)
(`agrogram.catalog.v1.CatalogService`). Conventions (errors, metadata,
validation, events) are in [back/README.md](../../README.md).

catalog owns listings and product types only. A listing stores its owner's
user id; names and avatars belong to profile-social, and the owner card is
composed at the edge (API gateway / client). catalog never calls another
service and consumes no stream. It publishes the listing stream
(`agrogram.catalog.listing.v1`) through a transactional outbox.
profile-social calls `ListListings` (owner filter) for `ListUserListings`.

AGROGRAM is listings-only: buyers and sellers contact each other directly
(the listing's phone, later chat). There are no transactions, escrow,
premium plans or roles to post: **any signed-in user may post on either
board**, and the board is the listing's `kind`.

| RPC | Who may call | What it does | Errors |
| --- | --- | --- | --- |
| `CreateListing` | any signed-in user, on either board | Posts an ACTIVE listing (version 1) and writes `ListingCreated` to the outbox in the same transaction. Idempotent on `request_id` (per owner): a retry returns the listing it created. Rate-limited per user | UNAUTHENTICATED; LISTING_LIMIT_REACHED (RESOURCE_EXHAUSTED, `metadata {listing_kind, limit}`); VALIDATION_FAILED (also: product retired or not in the reference data); RATE_LIMITED |
| `GetListing` | anyone for ACTIVE listings; the owner and staff (`ROLE_ADMIN`) for any status | Returns the listing. Opening an ACTIVE listing counts a view (not for the owner, staff or services) | NOT_FOUND (missing, or not visible to the caller) |
| `UpdateListing` | the owner only | Changes the `update_mask` fields (product, quantity, price, phone, address, notes, media); `kind` never changes; version + 1; equal values change nothing. Only ACTIVE listings are editable | PERMISSION_DENIED (someone else's visible listing); NOT_FOUND; LISTING_NOT_ACTIVE (FAILED_PRECONDITION, `metadata.status`); ABORTED / CONFLICT (stale `expected_version`, `metadata.current_version`); VALIDATION_FAILED |
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
`x-agrogram-roles`, `x-agrogram-session-id`, `x-agrogram-caller`,
`x-request-id`, `traceparent`, `accept-language`). Roles are the token-time
snapshot; the only one catalog reads is `ADMIN` (staff).

## Listing rules

**Boards.** `LISTING_KIND_OFFER` (farmers' board, "taklif") and
`LISTING_KIND_BUY_REQUEST` (resellers' board, "so'rov") are open to every
signed-in user; no role or verification is checked. A request without a kind
is VALIDATION_FAILED.

**Listing quota.** One flat tier: every user may have at most
`CATALOG_LISTING_QUOTA_PER_BOARD` (default **10**) ACTIVE listings on each
board. There is no premium tier.

- Only **ACTIVE** listings count: the quota limits what is on the board at
  once. CLOSED, BLOCKED and DELETED listings are off the board, so deleting
  (or, later, closing) a listing frees a slot. Whether BLOCKED listings
  should keep holding a slot (so moderation cannot be sidestepped) is an
  open question.
- The boards are independent: each has its own 10.
- With the per-user creation rate limit, the quota is the only guard on
  posting: there is no seller check.
- Atomicity: the creation transaction takes a per-owner advisory lock
  (`pg_advisory_xact_lock(hashtextextended('catalog:listings:<owner>', 0))`),
  then looks up the request id (idempotency), checks the product, counts and
  inserts. Concurrent creations of one owner therefore serialize; the
  integration test fires 15 at once and gets exactly 10.

**Visibility.** ACTIVE listings are public. Other statuses (CLOSED,
BLOCKED, DELETED) are visible through `GetListing` to the owner and staff
only. Lists and search return other statuses only for the caller's own
listings (`filter.owner_id` = caller) or to staff, and reject the request with
PERMISSION_DENIED otherwise (what profile-social's `ListUserListings`
expects). A listing someone may not see is NOT_FOUND, also for update and
delete. The contact phone is part of the listing (it is shown on it) but
never published in events.

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
  (exact, case-insensitive), owner, statuses, `min_quantity` (posted
  quantity), `min_price` / `max_price` in any unit (both exclude negotiable
  listings). Prices are compared as `price_per_tonne_minor`, a generated
  column equal to the price per kg × 1000, exact for prices quoted per kg and
  per tonne alike.
- **Sort.** RELEVANCE (`ts_rank`, default when there is text; descending only;
  without text it falls back to CREATED_AT), CREATED_AT, PRICE (per kg;
  negotiable listings last in both directions), QUANTITY (posted quantity).
  Direction defaults to descending; ties go newest first (created_at, id).
- **Page tokens** are opaque base64url JSON: a layout version, a fingerprint
  (SHA-256 of the operation, the normalized and authorized criteria, the
  search words and the sort) and the keyset position of the last listing
  (sort key, text rank, created_at, id). A token of another query, operation
  or layout is INVALID_ARGUMENT (`page.page_token`); the page size may change
  between pages. `total_size` is left unset (not cheap to count).

## Events

catalog consumes no stream.

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

listing.proto has no update, close or delete events yet, so `UpdateListing`
and `DeleteListing` publish nothing (`TODO(events)`).

## Layout

```
services/catalog/
├── cmd/catalog/             main: serve (default) | migrate up|down|status | healthcheck
├── internal/
│   ├── apperr/              Error type -> gRPC status with one common.v1.Error
│   ├── auth/                x-agrogram-* metadata -> Principal
│   ├── config/              CATALOG_* environment variables
│   ├── domain/              Listing, ProductType, filters, queries, the listing quota, proto conversions
│   ├── events/              ListingEvent builder, outbox relay
│   ├── grpcserver/          handler, interceptors (recovery, principal, logging, validation)
│   ├── health/              Postgres readiness -> gRPC health + /healthz, /readyz
│   ├── log/                 slog (JSON or text)
│   ├── ratelimit/           Redis fixed-window limiter (listing creation)
│   ├── repository/postgres/ pgx v5 repository, search query builder, outbox, integration tests
│   ├── service/             use cases: listings, list/search, page tokens, text normalization, product types
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
  → `product_types`, `quantity_grams` + `quantity_unit`, `remaining_grams` (dropped by 00004),
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
- `migrations/00003_listing_transactions.sql`: the projection of the former
  escrow transaction stream (`listing_transactions`) and its consumer inbox
  (`processed_events`). Kept because it is applied; 00004 undoes it.
- `migrations/00004_listings_only.sql`: listings only. Drops
  `listing_transactions`, `processed_events` and `listings.remaining_grams`,
  turns SOLD_OUT listings into CLOSED and removes SOLD_OUT from the status
  CHECK. Its Down restores the 00003 schema, empty (remaining = quantity;
  listings it closed stay CLOSED).

## Configuration

Environment variables only (`CATALOG_` prefix). Missing or invalid values stop
the start-up with one error that lists all of them.

| Variable | Default | Meaning |
| --- | --- | --- |
| `CATALOG_DATABASE_URL` | **required** | Postgres URL (role and database `catalog`) |
| `CATALOG_DATABASE_MAX_CONNS` | `10` | pgx pool size |
| `CATALOG_REDIS_URL` | empty | Redis for the creation rate limit (`catalog:ratelimit:*`) and view de-duplication (`catalog:views:*`); empty disables both (warning) |
| `CATALOG_KAFKA_BROKERS` | empty | Comma-separated brokers; empty disables the outbox relay (warning): `ListingCreated` events stay in the outbox table |
| `CATALOG_KAFKA_CLIENT_ID` | `catalog` | Kafka client id |
| `CATALOG_GRPC_ADDR` | `:50051` | gRPC listen address |
| `CATALOG_HTTP_ADDR` | `:8081` | `/healthz`, `/readyz` (also used by `catalog healthcheck`) |
| `CATALOG_GRPC_REFLECTION` | `false` | gRPC server reflection (grpcurl); local only |
| `CATALOG_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `CATALOG_LOG_FORMAT` | `json` | `json` or `text` |
| `CATALOG_LISTING_QUOTA_PER_BOARD` | `10` | ACTIVE listings per user on each board (one flat tier) |
| `CATALOG_CREATE_RATE_LIMIT` | `30` | Listing creations per user per window (fails open without Redis) |
| `CATALOG_CREATE_RATE_WINDOW` | `1h` | Rate-limit window |
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
make migrate-up               # goose, embedded SQL: schema, product seed, 00003, then 00004 (listings only)
make migrate-status           # all four migrations "Applied"
make migrate-down             # rolls back the latest one: 00004 first, then 00003, then the seed (fails while listings exist)
```

### 3. Run locally

```sh
make run                      # loads .env; gRPC 127.0.0.1:15054, HTTP 127.0.0.1:18054
curl -i http://127.0.0.1:18054/readyz
```

With `CATALOG_KAFKA_BROKERS` set, `serve` starts the outbox relay next to the
gRPC server. `make infra-up` in `back/` creates the listing topic (the broker
does not auto-create topics).

### 4. Run in Docker Compose (with the infra stack)

```sh
make compose-up               # builds the image, runs catalog-migrate, waits until healthy
make compose-logs
make compose-down             # removes only the catalog containers
```

### 5. Run alongside the other services

catalog needs only Postgres (and Kafka for the listing stream) to start.
profile-social calls it for `ListUserListings`. Local binaries, one terminal
each (each with its `.env`; profile-social's `.env.example` already points
at catalog on `127.0.0.1:15054`):

```sh
(cd ../identity && make migrate-up && make run)          # 127.0.0.1:15051
(cd ../auth-gateway && make migrate-up && make run)      # 127.0.0.1:15052 (needs make dev-key once)
(cd ../profile-social && make migrate-up && make run)    # 127.0.0.1:15053
make migrate-up && make run                              # 127.0.0.1:15054
```

Or all four in Compose, from the directory that contains `back/` (the
profile-social compose file points at `catalog:50051` by default):

```sh
AUTH_GATEWAY_UID=$(id -u) AUTH_GATEWAY_GID=$(id -g) \
docker compose -f back/deploy/docker-compose.yml \
  -f back/services/identity/compose.yaml \
  -f back/services/auth-gateway/compose.yaml \
  -f back/services/profile-social/compose.yaml \
  -f back/services/catalog/compose.yaml \
  up -d --build --wait identity auth-gateway profile-social catalog
```

### 6. Tests

```sh
make test                     # unit tests, no infrastructure (fakes, pgxmock, miniredis, kfake, bufconn)
make test-race                # the same with -race (needs a C compiler)
make lint                     # go vet (both build tags) + gofmt check
make test-integration         # -tags=integration against CATALOG_TEST_DATABASE_URL
```

The unit tests cover creation (any user on either board, the flat
per-board quota, per-board independence, only ACTIVE counting, a configurable
quota, idempotency, retired/unknown products, validation and trimming, the
rate limit and its fail-open), visibility (owner, staff, others), view
counting, update (mask, no-op, version guard, LISTING_NOT_ACTIVE, retired
products, re-indexing), delete (soft, idempotent, version guard),
list/search (statuses and PERMISSION_DENIED, keyset paging, token binding,
sort resolution, price normalization across units, cursor keys for every
sort), text normalization, the SQL builder and row mapping (pgxmock), the
event envelope (no phone), the relay (kfake), the view counter (miniredis),
the gRPC layer (validation, error details, mapping) and the seed against the
generated enum and product.proto. The integration tests create a random
schema (`catalog_test_<hex>`), migrate it through `search_path`, check the
seed, run the real repository and service (round trip, outbox row, 15
concurrent creations against the quota of 10, version guard, views, delete),
search (prefixes, uz/ru/apostrophe variants, multi-word, relevance order,
every filter, price and quantity sorts with negotiable listings last, paging
without gaps or repeats), migration 00004 down and up (a SOLD_OUT listing
becomes CLOSED, SOLD_OUT is rejected afterwards), all migrations back and
forward, and drop the schema.

### 7. Smoke test with grpcurl

With `CATALOG_GRPC_REFLECTION=true` and auth-gateway + identity running (for
real user ids; any UUID works for catalog alone):

```sh
CAT=127.0.0.1:15054
uuid() { cat /proc/sys/kernel/random/uuid; }
SELLER=$(uuid)

grpcurl -plaintext -d '{"category":"PRODUCT_CATEGORY_VEGETABLE"}' $CAT agrogram.catalog.v1.CatalogService/ListProductTypes

# any signed-in user may post on either board: a sell offer at 4 500 so'm per kg
grpcurl -plaintext -H "x-agrogram-user-id: $SELLER" \
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
grpcurl -plaintext -H "x-agrogram-user-id: $SELLER" \
  -d "{\"listing\":{\"id\":\"$LISTING\",\"notes\":\"Yangi hosil\"},\"update_mask\":\"notes\",\"expected_version\":\"1\"}" \
  $CAT agrogram.catalog.v1.CatalogService/UpdateListing
grpcurl -plaintext -H "x-agrogram-user-id: $SELLER" -d "{\"filter\":{\"owner_id\":\"$SELLER\"}}" \
  $CAT agrogram.catalog.v1.CatalogService/ListListings
grpcurl -plaintext -H "x-agrogram-user-id: $SELLER" -d "{\"listing_id\":\"$LISTING\",\"expected_version\":\"2\"}" \
  $CAT agrogram.catalog.v1.CatalogService/DeleteListing

# through profile-social (PROFILE_SOCIAL_CATALOG_ADDR set): the owner's listings
grpcurl -plaintext -d "{\"user_id\":\"$SELLER\"}" 127.0.0.1:15053 agrogram.profilesocial.v1.ProfileSocialService/ListUserListings

grpcurl -plaintext -d '{"service":"agrogram.catalog.v1.CatalogService"}' $CAT grpc.health.v1.Health/Check
```

`ListingCreated` records are visible in Kafka UI (http://127.0.0.1:18080,
topic `agrogram.catalog.listing.v1`).

## Stubs

None.

## TODOs

- `TODO(events)`: listing.proto has only `ListingCreated`; publish
  `ListingUpdated` / `ListingClosed` / `ListingDeleted` from `UpdateListing`
  and `DeleteListing` once it defines them (`internal/service/listings.go`,
  `internal/events/events.go`).
- `TODO(proto)`: catalog.proto has no CloseListing RPC and `update_mask`
  cannot name `status`, so status CLOSED ("closed by the owner") cannot be
  reached yet (`internal/service/listings.go`).
- `TODO(edge)`: owner cards (name, avatar) and owner-name
  search are composed by the API gateway / client; catalog sets
  `owner.user_id` only (`internal/grpcserver/handler.go`,
  `internal/service/search.go`, `internal/service/service.go`).
- `TODO(media)`: media carry `media_id` only; type, status, URLs, ownership
  and purpose checks come from media-pipeline; video is free for everyone,
  never gated on a plan (`internal/domain/proto.go`,
  `internal/domain/domain.go`, `internal/grpcserver/handler.go`,
  `internal/events/events.go`, `internal/service/listings.go`,
  `migrations/00001_init.sql`).

## Known gaps

- The comment on `remaining_grams` in `migrations/00001_init.sql` describes
  a column 00004 drops; applied migrations are not edited.
- Product labels live in the search documents: relabelling a product in a
  later migration must also recompute `search_document` of its listings.
- Search matches word prefixes, not typos or transliterations (Uzbek
  Cyrillic "картошка" does not find "kartoshka"); `pg_trgm` or a synonym
  table would help and the role may create that extension.
- Views are best effort (see above); `views_count` lags by up to one flush
  interval.
- Internal callers are recognized by `x-agrogram-caller` alone; the edge must
  strip client-supplied `x-agrogram-*` headers.
