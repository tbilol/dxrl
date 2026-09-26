# order-escrow

The escrow transaction (*tranzaksiya*) service: the financial core of AGROGRAM.
A reseller (buyer) and a farmer (seller) trade over a catalog listing; the
reseller's money is held in escrow until the goods are weighed, handed over and
received, then paid out to the farmer. Both sides trade commission-free.

It is the first **Java** service (all others are Go): Java 17, Spring Boot 4.1,
plain grpc-java, Spring JDBC (`JdbcClient`), Flyway, Kafka clients. It honours
the shared contracts exactly: `proto/orderescrow/v1/order_escrow.proto`, the
transaction stream (`proto/events/v1/transaction.proto`, envelope and record
headers from `metadata.proto`), the transactional outbox of the Go services,
the `x-agrogram-*` metadata convention and the error model
(`agrogram.common.v1.Error` in the gRPC status details).

Payments, tax, the bonus rule and weighbridge verification are **stubs**; see
[Stubs and TODOs](#stubs-and-todos).

## Layout

```
build.gradle.kts, settings.gradle.kts, gradle/    Gradle build (wrapper pinned by sha256), version catalog
src/main/java/uz/agrogram/orderescrow/
  OrderEscrowApplication    main: serve | migrate up|status|down | healthcheck
  app/                      Spring wiring (ServeConfiguration)
  config/                   ORDER_ESCROW_* parsing (Go syntax), postgres:// -> JDBC
  auth/                     Principal from x-agrogram-* metadata, UUIDv7
  error/                    AppException + ErrorReason -> gRPC code table, protovalidate mapping
  domain/                   EscrowTransaction aggregate, StateMachine, Command, Amounts/MoneyMath, stub records
  service/                  EscrowService (use cases, idempotency, retries), page tokens
  store/                    repository interfaces; store/jdbc: Postgres implementation
  events/                   TransactionEvents (envelopes), OutboxRelay (Kafka)
  catalog/                  CatalogService client (GetListing)
  grpc/                     gRPC server lifecycle, handlers, interceptors
  health/                   readiness monitor, /healthz /readyz, container probe
  migrate/                  Flyway up/status + explicit down scripts
src/main/resources/         application.yaml, db/migration/V1__init.sql, db/down/V1.down.sql
src/test/java/              unit tests; *IT tagged "integration" (need Postgres)
```

## Generated code: how a Java service consumes `back/gen/java`

`make gen` runs the shared tooling (`back/tools/codegen.sh`, pinned buf) and
writes Java for every proto package into `back/gen/java` (git-ignored; managed
mode prefix `uz`, so `agrogram.catalog.v1` is `uz.agrogram.catalog.v1`). The
build compiles the packages it needs as a separate **`contracts` source set**:

```kotlin
val genJavaDir: File = rootDir.resolve("../../gen/java").normalize()
val contractPackages = listOf("common", "events", "catalog", "orderescrow")
val contracts: SourceSet = sourceSets.create("contracts") {
    java { setSrcDirs(listOf(genJavaDir)); contractPackages.forEach { include("uz/agrogram/$it/**") } }
}
// main and test get contracts.output on their classpaths; bootJar packs it.
```

- The package list must be closed under proto imports (`orderescrow` and
  `catalog` import `common` and `events`; `events` imports `common`).
- `buf.validate.*` is not generated: `build.buf:protovalidate` provides it.
- Runtimes must match the plugins in `back/buf.gen.yaml`: protobuf-java
  **4.36.2** and grpc-java **1.84.0** (`gradle/libs.versions.toml`). Their BOMs
  are imported next to Spring Boot's (which manages older versions); Gradle
  picks the highest, so the pins win.
- The gencode compiles with lint off; the service's own code uses
  `-Xlint:... -Werror`.
- `compileContractsJava` fails with a pointer to `make gen` when the code is missing.

A future Java service copies this block, changes `contractPackages`, and keeps
the two version pins in step with `buf.gen.yaml`.

## Build, test, run

Needs a JDK 17+ (`java` on PATH or `JAVA_HOME`); the wrapper downloads Gradle
9.8.0 (checksum-pinned) and the dependencies into `~/.gradle`.

```sh
cd back/services/order-escrow
make gen                    # once, and after proto changes
make test                   # unit tests, no infrastructure
make lint                   # javac -Xlint -Werror (main + test)
make build                  # build/libs/order-escrow.jar
cp .env.example .env        # then set the password (see back/deploy/.env)
make migrate-up             # apply db/migration to the order_escrow database
make run                    # gRPC 127.0.0.1:15055, HTTP 127.0.0.1:18055
make test-integration       # tests tagged "integration" against ORDER_ESCROW_TEST_DATABASE_URL
```

Without make: `./gradlew test | integrationTest | bootJar`, then
`java -jar build/libs/order-escrow.jar [serve | migrate up|status|down | healthcheck]`.

## Configuration

Environment variables, same names/syntax/defaults as the Go services
(durations `500ms`, `2s`, `1h30m`; booleans `true`/`false`). Invalid values are
all reported at once and the process exits 1.

| Variable | Default | Meaning |
| --- | --- | --- |
| `ORDER_ESCROW_DATABASE_URL` | required | `postgres://user:pass@host:port/order_escrow?sslmode=disable` (converted to JDBC; credentials kept out of the JDBC URL) |
| `ORDER_ESCROW_DATABASE_MAX_CONNS` | `10` | HikariCP pool size |
| `ORDER_ESCROW_KAFKA_BROKERS` | empty | comma-separated; empty turns the outbox relay off (events stay in `outbox`) |
| `ORDER_ESCROW_KAFKA_CLIENT_ID` | `order-escrow` | Kafka client id |
| `ORDER_ESCROW_CATALOG_ADDR` | empty | catalog gRPC `host:port`; empty: CreateTransaction answers DEPENDENCY_UNAVAILABLE |
| `ORDER_ESCROW_CATALOG_TIMEOUT` | `2s` | per GetListing call |
| `ORDER_ESCROW_GRPC_ADDR` | `:50051` | gRPC listen address |
| `ORDER_ESCROW_HTTP_ADDR` | `:8081` | `/healthz`, `/readyz` |
| `ORDER_ESCROW_GRPC_REFLECTION` | `false` | server reflection (grpcurl); local only |
| `ORDER_ESCROW_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` (service loggers; libraries stay at info+) |
| `ORDER_ESCROW_LOG_FORMAT` | `json` | `json` (Spring Boot structured logging, ECS) or `text` |
| `ORDER_ESCROW_OUTBOX_INTERVAL` | `1s` | relay idle poll |
| `ORDER_ESCROW_OUTBOX_BATCH_SIZE` | `100` | relay batch |
| `ORDER_ESCROW_OUTBOX_MAX_BACKOFF` | `30s` | relay backoff cap while Kafka/Postgres fail |
| `ORDER_ESCROW_OUTBOX_PUBLISH_TIMEOUT` | `15s` | per batch |
| `ORDER_ESCROW_OUTBOX_RETENTION` | `168h` | published rows are purged after this; `0` keeps them |
| `ORDER_ESCROW_HEALTH_INTERVAL` | `5s` | Postgres ping for readiness |
| `ORDER_ESCROW_SHUTDOWN_TIMEOUT` | `20s` | gRPC drain on SIGTERM (per shutdown phase) |
| `ORDER_ESCROW_BONUS_PLACEHOLDER_POINTS` | `1` | STUB TODO(bonus): points to the reseller on completion; `0` = no BonusAwarded |
| `ORDER_ESCROW_TEST_DATABASE_URL` | none | integration tests only |

## Behaviour

### State machine

| State | Command (RPC) | Actor | Next state | Event (version after) |
| --- | --- | --- | --- | --- |
| — | CreateTransaction | counterparty of the listing owner | AWAITING_PAYMENT | TransactionCreated (1) |
| AWAITING_PAYMENT | PayIn | reseller | PAYMENT_HELD | PaymentHeld (2) |
| PAYMENT_HELD | SubmitWeighbridgeReceipt | reseller | RECEIPT_SUBMITTED | ReceiptSubmitted (3) |
| RECEIPT_SUBMITTED | ConfirmHandover | farmer | HANDED_OVER | ProductHandover (4) |
| HANDED_OVER | ConfirmReceipt | reseller | RECEIVED | ReceiptConfirmed (5) |
| RECEIVED | (payout, same DB transaction) | order-escrow | COMPLETED | PayoutReleased (6) |
| COMPLETED | (bonus, same DB transaction) | order-escrow | COMPLETED | BonusAwarded (7) |

Everything else is rejected: the wrong party gets NOT_TRANSACTION_PARTY
(PERMISSION_DENIED; staff included, they can only read), the right party in the
wrong state INVALID_STATE_TRANSITION (FAILED_PRECONDITION, metadata
`{"state": "PAYMENT_HELD", "command": "PayIn"}`), and a user who is neither a
party nor staff NOT_FOUND (existence is not revealed). COMPLETED is final.
There are no cancel/refund/dispute RPCs or states in the contract (TODO).

CreateTransaction: a reseller on a farmer's OFFER (needs ROLE_RESELLER, else
PERMISSION_DENIED) or a farmer on a reseller's BUY_REQUEST (needs ROLE_FARMER,
else FARMER_VERIFICATION_REQUIRED); buyer = reseller, seller = farmer. The
listing (catalog `GetListing` with `x-agrogram-caller: order-escrow` and the
user's headers) must be ACTIVE (LISTING_NOT_ACTIVE), not the caller's own
(SELF_TRADE_NOT_ALLOWED) and still have the quantity (INSUFFICIENT_QUANTITY,
metadata `requested_grams`, `available_grams`). A priced listing forbids
`unit_price` in the request; a negotiable one requires it. Catalog down or slow:
DEPENDENCY_UNAVAILABLE (UNAVAILABLE), nothing written. If the caller already
has an open transaction on the listing, it is returned instead (enforced by the
partial unique index `escrow_transactions_open_initiator_key`).

### Idempotency and concurrency

- Every command is **one database transaction** (READ COMMITTED): state change,
  payment/receipt/payout row, history, outbox events and the idempotency record.
- `processed_requests (caller_id, request_id)` stores the operation, the
  transaction, a SHA-256 of the request without its `request_id` and the
  **response returned**. A retry returns that response, with no side effects,
  even after the transaction moved on. The same `request_id` on another
  operation, transaction or payload is VALIDATION_FAILED on `request_id`
  (rule `request_id.reused`). Failed calls are not recorded.
- A command locks the transaction row (`SELECT ... FOR UPDATE`) before reading
  the idempotency record, and writes with `UPDATE ... WHERE version = :read`.
  Any lost race (stale version, unique key, deadlock, serialization failure)
  rolls back and retries the unit (4 attempts, then CONFLICT/ABORTED); the
  retry sees the winner's committed result, so duplicates resolve to the same
  response and a step happens once.
- Creation serializes per listing with `pg_advisory_xact_lock` and computes the
  committed quantity after the lock.

### Money

Integer tiyin (`long`), currency UZS, fixed at creation, never recomputed:

```
subtotal      = round_half_up(unit_price.amount_minor x grams / grams_in(unit))   (BigInteger, exact)
tax           = TaxPolicy (STUB: 0, TODO(tax))
platform_fee  = 0 (TODO(payments): commission-free)
total         = subtotal + tax + platform_fee     what the reseller pays in (payments.amount_minor)
payout_amount = subtotal                          what the farmer receives (payouts.amount_minor)
```

Conservation `total = payout_amount + tax + platform_fee` is enforced by the
`Amounts` constructor, the aggregate's invariants (payment = total, payout =
payout_amount, released exactly once, to the farmer) and CHECK constraints. A
total that rounds to 0 tiyin or overflows is VALIDATION_FAILED.

**Oversell guard:** available = listing `quantity` (not `remaining_quantity`,
which catalog will derive from this stream: TODO(phase3) there) minus the grams
of all of this service's transactions on the listing, completed ones included.
Unpaid transactions keep reserving quantity until a cancel/expiry path exists
(TODO(cancel)).

**Payout design:** the stub payout is immediate, so ConfirmReceipt goes
HANDED_OVER -> RECEIVED -> COMPLETED (+ bonus) in one database transaction: no
transaction can be stranded in RECEIVED and there is exactly one
PayoutReleased (the step can run once; `payouts.transaction_id` and
`payouts.payment_id` are UNIQUE). With a real provider (TODO(payments)) RECEIVED
becomes a waiting state and a worker has to finish it idempotently.

### Events

Topic `agrogram.orderescrow.transaction.v1`, key = transaction id, value =
`TransactionEvent` (Protobuf binary): `metadata` (UUIDv7 `event_id`, full
`event_type`, `occurred_at` = the command's time, producer `order-escrow`,
aggregate TRANSACTION/id/version, `correlation_id` = x-request-id, trace
context, actor), the `TransactionSnapshot` after the event, one oneof case.
Headers: `content-type: application/x-protobuf`, `event-type`, `event-id`,
`x-request-id`, `traceparent`/`tracestate` when present. Every event is
validated with protovalidate before it is written to the outbox. No contact
details and no payment-provider references. The payout and bonus events are
acted by `order-escrow`; their `causation_id` chains ReceiptConfirmed ->
PayoutReleased -> BonusAwarded. profile-social's consumer (records COMPLETED
transactions) accepts every one of them (see `LifecycleTest`).

The relay (`OutboxRelay`) publishes in outbox order with an idempotent producer
(acks=all), `FOR UPDATE SKIP LOCKED` batches, exponential backoff and hourly
purging, exactly like the Go relays. Kafka down never blocks a command.

## Database

Database `order_escrow`, login `order_escrow` (back/deploy/postgres/init).
`db/migration/V1__init.sql`: `escrow_transactions`, `transaction_state_changes`,
`payments`, `payouts`, `weighbridge_receipts`, `processed_requests`, `outbox`
(same columns as the Go services' outbox). Flyway OSS has no undo, so every
migration has an explicit revert script `db/down/V<n>.down.sql`;
`migrate down` runs the latest one and removes its history row in one
transaction (development only: it drops the data).

## Health, logging, shutdown

- `GET /healthz` 200 `ok` while the process runs; `GET /readyz` 200 `ready` when
  Postgres answered the last ping, else 503 `not ready`. The gRPC health service
  (`""` and `agrogram.orderescrow.v1.OrderEscrowService`) mirrors readiness.
- `java -jar order-escrow.jar healthcheck` probes `/readyz` (container HEALTHCHECK).
- One log line per RPC (method, request_id, user_id, caller, code, reason,
  duration); `request_id` is in the MDC of every line of a call.
- SIGTERM: readiness drops, the gRPC server drains running calls for up to
  `ORDER_ESCROW_SHUTDOWN_TIMEOUT`, the relay finishes its batch and closes the
  producer, then the HTTP server and the pool stop.

## Running with the other services

Local binaries (each from its own directory, each with its `.env`):
identity `:15051`, auth-gateway `:15052`, profile-social `:15053`, catalog
`:15054` (`make run` in each), then `make run` here (`ORDER_ESCROW_CATALOG_ADDR=127.0.0.1:15054`).

Compose (from the directory that contains back/):

```sh
docker compose -f back/deploy/docker-compose.yml \
  -f back/services/identity/compose.yaml -f back/services/auth-gateway/compose.yaml \
  -f back/services/profile-social/compose.yaml -f back/services/catalog/compose.yaml \
  -f back/services/order-escrow/compose.yaml up -d --build --wait \
  identity auth-gateway profile-social catalog order-escrow
```

`make compose-up` starts order-escrow alone (catalog must then run separately
or CreateTransaction answers DEPENDENCY_UNAVAILABLE).

### grpcurl walkthrough

With reflection on (`.env.example`), a farmer's ACTIVE offer in catalog
(`LISTING`), and `F`/`R` the farmer's and the reseller's user ids:

```sh
H='-H x-agrogram-user-id:'; A=127.0.0.1:15055
grpcurl -plaintext $A list
T=$(grpcurl -plaintext ${H}$R -H x-agrogram-roles:RESELLER -d "{\"request_id\":\"$(uuidgen)\",\"listing_id\":\"$LISTING\",\"quantity\":{\"grams\":1000000}}" \
  $A agrogram.orderescrow.v1.OrderEscrowService/CreateTransaction | jq -r .transaction.id)
grpcurl -plaintext ${H}$R -d "{\"request_id\":\"$(uuidgen)\",\"transaction_id\":\"$T\"}" $A agrogram.orderescrow.v1.OrderEscrowService/PayIn
grpcurl -plaintext ${H}$R -d "{\"request_id\":\"$(uuidgen)\",\"transaction_id\":\"$T\",\"ticket_number\":\"TB-1\"}" $A agrogram.orderescrow.v1.OrderEscrowService/SubmitWeighbridgeReceipt
grpcurl -plaintext ${H}$F -H x-agrogram-roles:RESELLER,FARMER -d "{\"request_id\":\"$(uuidgen)\",\"transaction_id\":\"$T\"}" $A agrogram.orderescrow.v1.OrderEscrowService/ConfirmHandover
grpcurl -plaintext ${H}$R -d "{\"request_id\":\"$(uuidgen)\",\"transaction_id\":\"$T\"}" $A agrogram.orderescrow.v1.OrderEscrowService/ConfirmReceipt
grpcurl -plaintext ${H}$F -d "{\"transaction_id\":\"$T\"}" $A agrogram.orderescrow.v1.OrderEscrowService/GetTransaction
grpcurl -plaintext ${H}$R -d '{"role":"ROLE_RESELLER"}' $A agrogram.orderescrow.v1.OrderEscrowService/ListTransactions
```

Events: `kcat -b localhost:19092 -t agrogram.orderescrow.transaction.v1 -C -f '%k %h\n'`
(or Kafka UI on :18080).

## Stubs and TODOs

- **STUB TODO(payments):** no provider, no MCHJ account. PayIn records the
  funds as held at once (`payments`, provider `stub`, no provider id,
  `checkout_url` empty, `return_url` ignored); ConfirmReceipt records the
  payout as released at once (`payouts`). No money moves; PAYMENT_FAILED never
  happens. Platform fee 0 (commission-free).
- **STUB TODO(tax):** `TaxPolicy.NONE` returns 0; the hook is the only place to
  plug a regime in (tax on top today; withholding would change the formulas,
  not the conservation).
- **STUB TODO(bonus):** a placeholder number of points (config) goes to the
  reseller on completion (BonusAwarded, `bonus_points`). Consumers must not
  treat it as real.
- **STUB TODO(weighbridge):** receipts are stored and VERIFIED at once;
  RECEIPT_REJECTED never happens. TODO(media): `photo_media_id` is not checked
  with media-pipeline.
- **TODO(cancel) / TODO(refund) / TODO(dispute):** no RPCs or states; unpaid
  transactions reserve quantity indefinitely; held funds can only leave as the
  payout; staff cannot act on transactions.
- **TODO(negotiation):** the listing owner has no step to accept a negotiated
  `unit_price`.
- **TODO(security):** plaintext gRPC to catalog; `x-agrogram-caller` is trusted.
