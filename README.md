# golang-flash

Flash sales where the cart cannot hoard the stock, written in **Go** and run on **Kubernetes**. It is the Go and Kubernetes
version of [dotnet-flash](https://github.com/joshbarros/dotnet-flash): the same saga, the same HTTP API and the same Angular
UI, so the browser specs and the smoke script of the .NET system run against this one unchanged.

The scarce number is the **pieces left** in a deal, and the **deals** module is its only writer. A shopper can **hold pieces
and pay later**: the pieces leave the shelf at once, but the hold carries an expiry, no receipt is issued while it is
pending, and a sweeper in the deals module (every API replica runs one) gives the pieces back, and the shopper's allowance,
and tells the order, when the time runs out. Confirming and expiring race for the same row: both are a single conditional
`UPDATE` on the hold, so exactly one wins, with several replicas sweeping at once (`FOR UPDATE SKIP LOCKED`) and with a
confirm landing on the very millisecond of the deadline. Two further rules hold in the same transactions: a shopper may hold
at most **`max_per_shopper`** pieces of a deal (an atomic upsert, rolled back with a failed attempt), and the pieces left are
taken with one conditional `UPDATE`, so when two shoppers fight for the last piece one commits a hold and the other is told
the deal is short. A reference that contains the poison token `POISON` is committed as rejected by the receipts module, the
message is dead-lettered, and a compensating command gives the pieces back. A later refund does the same, but it does not
roll back the receipt that was already issued.

What is different here is how it runs. **Kubernetes is what runs the stack**: Postgres, Kafka, RabbitMQ, **two API replicas**
and two web replicas, started in a local [kind](https://kind.sigs.k8s.io) cluster with Kustomize manifests. Nothing is faked
for the demo: the same pipeline (`make cluster-up`, the smoke script, the browser specs, a script that kills pods while holds
are pending) runs in CI on a fresh kind cluster.

![The Flash UI signed in as the administrator: a deal with one order sold, one held with a pay-by time and a Confirm button, and one expired](docs/screenshot.png)

*The UI reads everything from the API; shown signed in as the seeded administrator on the cluster. The first order was held
and confirmed, the second is held and still has a pay-by time, the third was never confirmed and expired, so its pieces are
back on sale.*

## Order statuses

```mermaid
flowchart LR
    Requested -->|pieces held| Held
    Requested -->|short or over the limit: no hold written| Rejected
    Held -->|receipt issued| Sold
    Held -->|pending hold: confirmed in time| Held
    Held -->|pending hold: not confirmed in time| Expired
    Held -->|poison reference: receipt rejected| Rejected
    Held -->|refund| Refunded
    Sold -->|refund| Refunded
```

`Refunded` and `Rejected` (after a hold) put the pieces back through a `ReleasePieces` command, and Deals releases a hold at most once (an expired hold is released the same way, by the sweeper, and moves the order to `Expired`), and gives the shopper their allowance back with it. The receipt has its own status (`Issued` or `Rejected`) and a refund never changes it. If the refund gets there before the final mark, the late mark finds the order is no longer `Held` and does nothing.

## How the modules talk

```mermaid
sequenceDiagram
    participant C as Client
    participant O as Orders
    participant T as Deals
    participant E as Receipts

    C->>O: POST /api/orders
    Note over O: order + outbox row, one transaction
    O-->>C: 202 Requested
    O->>T: pieces.reserve (relay → RabbitMQ, copy to Kafka)
    Note over T: inbox claim, per-shopper limit upsert, conditional UPDATE, hold, outbox row
    alt pieces left cover the order and the shopper is within the limit
        T->>O: pieces.held (Requested → Held, with an expiry if the order asked to hold)
        T->>E: pieces.held (the receipt waits while the expiry is set)
        opt pending hold
            C->>O: POST /api/orders/{id}/confirm
            O->>T: pieces.confirm (conditional UPDATE: pins the hold unless the sweeper got there first)
            T->>E: pieces.held (no expiry: now the receipt is issued)
        end
        alt reference is clean
            Note over E: receipt, outbox row
            E->>O: receipt.issued (Held → Sold)
        else reference contains POISON
            Note over E: committed Rejected, then the message is dead-lettered
            E->>T: pieces.release (compensating command)
            E->>O: receipt.rejected (Held → Rejected)
            T->>O: pieces.released
        end
    else deal short, or the shopper is over the limit
        Note over T: the attempt is rolled back, so a limit increment never outlives a failed reserve
        T->>O: pieces.short (Requested → Rejected, no hold, reason Short or Cap)
    end
    Note over T: the sweeper, every instance, every second: unconfirmed holds past their expiry
    T->>O: pieces.expired (Held → Expired), pieces and allowance back
    C->>O: POST /api/orders/{id}/refund
    O->>T: pieces.release (Held or Sold → Refunded)
    T->>O: pieces.released
```

| Module | Owns | Consumes | Emits |
| --- | --- | --- | --- |
| **identity** (`internal/identity`) | `identity` schema: users, roles, refresh tokens. | | |
| **orders** (`internal/orders`) | `orders.orders`: status, pieces, total, shopper reference. Validation, the poison rule, the total. | `pieces.held`, `pieces.short`, `pieces.expired`, `receipt.issued`, `receipt.rejected`, `pieces.released` | `pieces.reserve`, `pieces.reserve`, `pieces.confirm`, `pieces.release` (refund) |
| **deals** (`internal/deals`) | `deals.deals.pieces_left` (the only writer after the deal opens), `deals.shoppers` (what each shopper holds) and `deals.piece_holds` (with the expiry of a pending hold). | `pieces.reserve`, `pieces.release`, `pieces.confirm` (prefetch 1) | `pieces.held`, `pieces.short`, `pieces.released` |
| **receipts** (`internal/receipts`) | `receipts.receipts`. | `pieces.held` | `receipt.issued`, `receipt.rejected`, `pieces.release` (poison) |
| **platform** (`internal/platform`) | The relay, the consumer loop with retry and dead-letter, the Kafka log, problem+json and the middleware, config, migrations. Each module owns its own `outbox_messages` and `inbox_messages` tables. | | |

RabbitMQ topology is declared in code (`internal/platform/messaging/rabbit.go`): exchange `flash.work`, one queue per module
(`deals.commands`, `receipts.work`, `orders.events`), a `.retry` queue per module (2 s TTL, then back to the main queue, 3
attempts), and a `.dlq` per module behind `flash.dlx`. A transient failure retries; a poison message goes straight to the
dead-letter queue after its rejection is committed. After a trip through the retry queue the broker's routing key is the queue
name, so the relay stores the original key in the message `type` and the consumer dispatches on that. Kafka has one topic,
`flash.events`, one partition, replication factor 1, keyed by order id.

## Architecture

One binary (`cmd/api`), four modules in `internal/`. Each module has its own Postgres **schema** (`identity`, `orders`,
`deals`, `receipts`), its own embedded SQL migrations and its own outbox and inbox tables. No module imports another; the one
cross-module read (a deal's existence and price) goes through a small interface that the orders module declares and the deals module
satisfies, and `internal/app` is the only package that knows all of them.

```mermaid
flowchart TB
    main["cmd/api: serve | migrate"] --> app["internal/app: wiring, middleware chain, workers"]
    app --> identity & orders & deals & receipts
    identity["internal/identity: users, roles, JWT, refresh tokens, two-factor"]
    orders["internal/orders: schema orders"]
    deals["internal/deals: schema deals, the pieces, the cap, the holds and the sweeper"]
    receipts["internal/receipts: schema receipts"]
    identity & orders & deals & receipts --> platform["internal/platform: httpx, messaging, pgdb, config"]
    orders -. "Catalog.Find" .-> deals
```

Modules still talk only through messages: RabbitMQ carries the commands, Kafka keeps the event log, and the outbox and inbox
make delivery at-least-once and consumption idempotent. The dependencies are small on purpose: `pgx`, `amqp091-go`,
`franz-go`, `golang-jwt` and `x/crypto`; the HTTP layer is the standard library's `net/http` mux.

## On Kubernetes

```mermaid
flowchart LR
    host["127.0.0.1:8112 · :5232 · :15722"] -->|NodePort| web & api & mgmt
    subgraph ns["namespace flash (kind: one node)"]
        web["web ×2: nginx + Angular"] -->|/api| api
        api["api ×2: Go (init container: migrate)"] --> pg[("postgres StatefulSet")]
        api --> kafka[("kafka StatefulSet (KRaft)")]
        api --> rmq[("rabbitmq StatefulSet")]
        mgmt["RabbitMQ management"] --- rmq
    end
```

| What | How it is set up |
| --- | --- |
| API | A Deployment of **2 replicas**, `maxUnavailable: 0`, a PodDisruptionBudget (`minAvailable: 1`), spread over nodes. Nothing in it assumes a single instance: the relay claims outbox rows with `SKIP LOCKED`, the stock is taken with a conditional `UPDATE`, the auth rate limit and the session version live in Postgres. |
| Migrations | An **init container** runs `api migrate` before any replica serves (each module migrates behind a Postgres advisory lock, so several pods starting together are safe) and the API runs with `MIGRATE_ON_START=false`. |
| Probes | `startupProbe` and `livenessProbe` on `/api/health`; `readinessProbe` on `/api/health/ready`, which checks that every module's schema is there. |
| Data stores | One StatefulSet each for Postgres 17, Kafka 3.8 (KRaft, one node) and RabbitMQ 3.13, with a PVC each. Kafka and RabbitMQ keep their data across a pod restart. |
| Security | Pods run as non-root with a read-only root filesystem for the API, no privilege escalation, all capabilities dropped and the `RuntimeDefault` seccomp profile; the namespace enforces the `baseline` Pod Security level and warns on `restricted`. A NetworkPolicy lets only the API reach Postgres, Kafka and RabbitMQ (kind's `kindnet` enforces it). |
| Config and secrets | A ConfigMap (`flash-config`) and a Secret (`flash-secrets`). The kind overlay generates the demo Secret from `deploy/k8s/overlays/kind/secrets.env` (git-ignored, written by `scripts/kind-secrets.sh`, random signing key). A real cluster brings its own Secret under the same name. |
| Reaching it | `deploy/kind/cluster.yaml` publishes three NodePorts on `127.0.0.1` only: the web app on **8112**, the API on **5232**, RabbitMQ's management UI on **15722**. nginx reaches the API as `api.flash.svc.cluster.local` through the cluster DNS. |

The base manifests (`deploy/k8s/base`) carry the production defaults (email confirmation required, no dev mailbox, 20 auth
requests a minute). The kind overlay relaxes exactly what a demo needs: no email confirmation, the dev mailbox on (kept in
Postgres, so both replicas see the same messages), a delay on the final mark so the smoke script can land a refund inside the
window, a short hold and a fast sweeper.

## Authentication and authorization

| Piece | How it works |
| --- | --- |
| Accounts | Passwords need 10+ characters with upper case, lower case and a digit, and 4 distinct characters, at most 128 (anything longer is refused before it is hashed). They are hashed with **argon2id** (19 MiB, 2 passes, per-hash salt). Five wrong passwords (or codes) lock the account for 15 minutes (`429`). A wrong password and an unknown email give the same `401`, and take the same time. |
| Email | Registration mails a confirmation link; with `AUTH_REQUIRE_CONFIRMED_EMAIL` on (the default) sign-in is refused (`403`) until it is opened. "Forgot password" mails a one-time reset link and answers `202` for every address, known or not. The links carry an HMAC token bound to the user, the purpose, an expiry and the user's *security stamp*, which rotates when the password changes, so each link works once. Messages go through a mailer interface: SMTP when `MAIL_HOST` is set, the log otherwise. |
| Two-factor | TOTP (RFC 6238, any authenticator app): `POST /api/auth/mfa/setup` returns the key and an `otpauth://` URI, `enable` proves it with a code and returns eight one-time recovery codes, `disable` needs the password and a code. Once on, sign-in needs a code (`401 A verification code is required.`). |
| Access token | JWT, 15 minutes, held **in memory** by the Angular app and sent as `Authorization: Bearer`. HS256 with a key of at least 32 bytes, or **RS256** when `JWT_RSA_PRIVATE_KEY_PEM` is set, in which case the public key is served at `/.well-known/jwks.json`. Only the configured algorithm is accepted (no `none`, no HS/RS confusion). |
| Revocation | Each token carries the user's session version. A password change, a reset, turning two-factor off and **"sign out everywhere"** bump it (one atomic `UPDATE`), and every token sold earlier stops working on every replica within `JWT_REVOCATION_CACHE_SECONDS` (default 5). |
| Refresh token | 384 random bits in an `HttpOnly`, `SameSite=Strict`, `Path=/api/auth` cookie (`Secure` over HTTPS). Only its SHA-256 is stored. Every refresh **rotates** it with a conditional `UPDATE`, so two concurrent refreshes with the same token cannot both win. Presenting a token that was already used revokes the whole family. Refresh and logout also need an `X-Requested-With: flash-web` header, so another site cannot trigger them. |
| Roles | `Customer` (default at registration), `Staff`, `Admin`. An administrator is seeded from `ADMIN_EMAIL` / `ADMIN_PASSWORD` on first start. |
| Rules | Anonymous: `/api/health*`, `/api/auth/register|login|refresh|logout` and the email flows. Every other route needs a token. Any signed-in user: read deals, order, confirm, read and refund **their own** orders and receipts. `Staff` and `Admin`: open deals, read everything, read the event log and the dead letters. Another user's order or receipt is `404`, not `403`, so ids do not leak. The owner of an order is the caller; a body field cannot set it. |
| Hardening | A rate limit on `/api/auth/*` per client (`RATE_LIMIT_AUTH_PER_MINUTE`), **counted in Postgres** so every replica shares one count, security headers on every response (`nosniff`, `DENY`, strict CSP on the API, `no-referrer`), a correlation id echoed in `X-Correlation-Id` and in every problem body, JSON logs (`LOG_JSON`), forwarded headers trusted only when `FORWARDED_HEADERS` is on, a panic handler, and a readiness endpoint that checks every module schema. |

## Run it

You need Go 1.26, Docker, [kind](https://kind.sigs.k8s.io), kubectl and Node 22 (for the Angular app and the browser specs).

```bash
make test-unit                  # no Docker
make deps                       # Postgres, Kafka and RabbitMQ in Docker, for `go run` and the integration tests
make test-integration           # a real Postgres: the database flash_test
make cluster-up                 # builds both images, creates the kind cluster, applies the manifests, waits until Ready
./scripts/smoke.sh              # last line: golang-flash smoke passed
npm ci && npx playwright install chromium
npx playwright test             # the browser specs against http://127.0.0.1:8112
./scripts/resilience.sh         # kills an API replica and RabbitMQ in the middle of a deal (about two minutes)
make cluster-down
```

| Service | On your machine |
| --- | --- |
| Web (nginx, proxies `/api`) | <http://127.0.0.1:8112> |
| API (`/api/health`, `/api/health/ready`) | <http://127.0.0.1:5232> |
| RabbitMQ management (`platform` / `platform`) | <http://127.0.0.1:15722> |
| `make deps`: Postgres / Kafka / RabbitMQ | 5492 / 9142 / 5722 (management 15722, so stop the cluster first) |

Sign in as `admin@flash.local` / `FlashAdmin2026` in the demo cluster, or register a customer.

## Try it with curl

The web app is at <http://127.0.0.1:8112> (`admin@flash.local` / `FlashAdmin2026` in the demo stack, or register a customer). From the shell:

```bash
API=http://127.0.0.1:5232
login() { curl -s -X POST $API/api/auth/login -H 'content-type: application/json' -d "{\"email\":\"$1\",\"password\":\"$2\"}" | python3 -c 'import sys,json; print(json.load(sys.stdin)["accessToken"])'; }
ADMIN="Authorization: Bearer $(login admin@flash.local FlashAdmin2026)"

curl -s -X POST $API/api/auth/register -H 'content-type: application/json' \
  -d '{"email":"ana@example.com","password":"Passw0rd-Demo1"}'      # 201; its accessToken goes in $ANA below
ANA="Authorization: Bearer <accessToken>"

curl -s -H "$ADMIN" -X POST $API/api/deals \
  -H 'content-type: application/json' \
  -d '{"code":"GENESIS-01","name":"Night One","aisle":"Home","brand":"Acme Goods","piecesTotal":8,"maxPerShopper":4,"priceCents":8000}'

curl -s -H "$ANA" -X POST $API/api/orders \
  -H 'content-type: application/json' \
  -d '{"dealCode":"GENESIS-01","reference":"SHP-1001","pieces":4}'
# 202, status Requested, totalCents 35840. Use the returned id. A fifth piece for the same account would be refused (limit 4):

curl -s -H "$ANA" $API/api/orders/<id>               # Sold after a few seconds, with a receipt number
curl -s -H "$ANA" $API/api/receipts/<id>
curl -s -H "$ANA" $API/api/deals/GENESIS-01        # piecesLeft 4
curl -s -H "$ADMIN" $API/api/orders/<id>/events        # the Kafka log for this order

curl -s -H "$ANA" -X POST $API/api/orders/<id>/refund \
  -H 'content-type: application/json'                       # pieces go back on sale; the receipt stays
```

An order is validated at the door: shopper reference required (at most 40 characters), 1 to 6 pieces. A deal's `maxPerShopper` (default 2, at most the pieces on sale) caps what one account can hold in it; an order that would pass it is rejected with the reason `Cap` and writes no hold. The poison match is a case-sensitive substring: `SHP-POISON-7` and any longer token containing `POISON` match.

## Develop

```bash
make deps && make run                 # the API on :5232 against the local dependencies (migrates itself)
npx nx serve web                      # http://localhost:4312, proxies /api to :5232
WEB_URL=http://localhost:4312 npx playwright test
make lint                             # gofmt and go vet
make logs                             # follow the API logs of every replica in the cluster
make cluster-status
```

## What is tested, and where it runs

- **Unit tests** (no Docker, under a second each): the order rules and the wording of the refusals, password hashing and policy, RFC 6238 test vectors for
  two-factor, access tokens (round trip, tampering, expiry inside and beyond the skew, `alg=none`, a token signed with another
  key, issuer and audience, RS256 and the algorithm confusion attack, the JWKS), one-time email tokens, the retry policy and
  the poison rule, the wire names of the messages and of the Kafka envelope, the problem+json responses, the correlation id and
  security headers, owner-or-staff access, config validation.
- **Integration tests** (`internal/integration`, a real Postgres, the real middleware chain and routes, brokers replaced by a
  loop that hands each outbox row to the handlers its routing key is bound to): register, login, lockout, refresh rotation,
  replay revoking the family, concurrent refreshes (exactly one wins), logout, change password, reset, email confirmation, the
  full two-factor lifecycle, the shared rate limit, RS256 and the JWKS, every route's 401/403/404 and ownership, paging,
  validation at the door; the deals module (**concurrent reservations never oversell**, one shopper racing themselves cannot pass the cap, a refused
  try costs neither pieces nor allowance, a release gives both back exactly once) and the **hold expiry** (a pending hold
  keeps its pieces and issues no receipt until it is confirmed; an unconfirmed hold expires and returns the pieces with the
  shopper's allowance; confirming an expired hold or a sold order is a conflict; **four sweepers over twenty expired holds
  expire each exactly once**; **twelve rounds of a confirm and the sweeper racing for the same hold, a millisecond either side
  of the deadline, always have exactly one winner**; a refund of a pending hold gives the pieces back and the sweeper leaves it
  alone); the whole saga (order to sold and a receipt, the cap with the allowance coming back on a refund, poison
  dead-lettered and the pieces released, a refund after the sale, a late final mark that loses to a refund, a repeated delivery
  that has no second effect, a final mark that arrives before the held mark and is retried); the outbox (several relays drain disjoint batches, a failed publish rolls the batch
  back, trimming keeps the inbox longer than the outbox) and migrations (six replicas migrating at once apply each file once,
  a broken file rolls back). Both suites run with the race detector in CI.
- **`scripts/smoke.sh`** drives the cluster over HTTP: authentication and ownership first (401, 403, 404), a password reset
  that ends the old sessions, validation, the happy path, the last piece with concurrent shoppers, the per-shopper limit, a hold that is confirmed and one that is left to expire (the demo holds for twelve seconds), the poison reference (`receipts.work.dlq` gains one message, pieces and allowance restored), the dead letters listed and replayed, and a refund that lands between
  the document and the final mark.
- **Playwright** (`e2e/specs`, 17 specs, Chromium, against the web port of the cluster): sign-in and its refusals, the
  password policy, the session surviving a reload through the cookie, a password reset through the emailed link, two-factor
  sign-in, "sign out everywhere", the API refusing bad input and the page saying why, a deal opened and bought from and refunded in the UI, the last piece going to one request while the same shopper asking again is held to the limit, a shopper who cannot go past the limit and gets the allowance back on a refund, **a held order with a pay-by time that is confirmed in time and one that expires**, a poison order that is rejected, the Kafka event log.
- **`scripts/resilience.sh`** is the one only Kubernetes makes possible: it takes two holds, confirms one, kills an API replica and the RabbitMQ pod while the other is still pending, and checks afterwards that the confirmed hold was sold exactly once with one receipt and its pieces still held, and that the other expired exactly once with its pieces back on the shelf.
- **CI** (`.github/workflows/ci.yml`) has two jobs. `verify`: gofmt and `go vet`, unit and integration tests with `-race`
  (Postgres 17 service container), `govulncheck`, `npm audit`, Prettier, the Angular type check, lint and build, and the
  Kustomize overlay rendering. `e2e`: creates a kind cluster, runs `make cluster-up`, the smoke script, the browser specs and
  the resilience script, and uploads cluster state, logs and traces on failure. Dependabot watches Go modules, npm, Actions and
  Docker.

## API

JSON over HTTP, the same as dotnet-flash's. Everything except health and `/api/auth/register|login|refresh|logout` and the
email flows needs a bearer token.

| Method and path | What it does | Responses |
| --- | --- | --- |
| `GET /api/health`, `GET /api/health/ready` | Liveness; readiness (every module schema reachable). | `200`, `503` |
| `POST /api/auth/register` | Creates a customer and mails a confirmation link. Signs them in unless confirmation is required. | `201` signed in, `202` check your inbox, `400` policy, `409` email taken, `429` |
| `POST /api/auth/login` | Signs in (with `code` when two-factor is on); sets the refresh cookie. | `200`, `401`, `403` unconfirmed, `429` locked or rate limited |
| `POST /api/auth/refresh`, `POST /api/auth/logout` | Rotate or revoke the refresh cookie. Need `X-Requested-With: flash-web`. | `200`/`204`, `400` header missing, `401` |
| `POST /api/auth/confirm-email`, `resend-confirmation` | Confirm an address with the emailed token; ask for a new link (always `202`). | `204`/`202`, `400` |
| `POST /api/auth/forgot-password`, `reset-password` | Mail a reset link (always `202`); set a new password with it (ends every session). | `202`/`204`, `400` |
| `POST /api/auth/change-password`, `logout-everywhere` | Signed in: change the password (ends the other sessions and returns a fresh one); end every session on every device. | `200`/`204`, `400`, `401` |
| `GET /api/auth/mfa`, `POST /api/auth/mfa/setup`, `enable`, `disable` | Two-factor status and lifecycle. | `200`/`204`, `400`, `409` |
| `GET /.well-known/jwks.json` | The public key set when RS256 is on. | `200`, `404` |
| `GET /api/auth/me` | The caller. | `200`, `401` |
| `POST /api/deals` | Opens a deal with its pieces on sale, a price in cents and an optional `maxPerShopper` (default 2). Staff. | `201`, `400` invalid, `403`, `409` code exists |
| `GET /api/deals`, `GET /api/deals/{code}` | Deals with the live pieces left. | `200`, `404` |
| `POST /api/orders` | Accepts an order (1 to 6 pieces) for the caller and starts the saga. `"hold": true` keeps the pieces without paying until `/confirm` or the expiry. | `202` Requested, `400` invalid, `404` unknown deal, `409` duplicate reference (a deal short of pieces or over the limit is rejected later, by the saga) |
| `GET /api/orders?dealCode=&page=&pageSize=` | The caller's orders, newest first, a page at a time (`pageSize` at most 100, default 100). Staff: everyone's. The total is in `X-Total-Count`. | `200` |
| `GET /api/orders/{id}` | One order and its status. Owner or staff. | `200`, `404` |
| `POST /api/orders/{id}/confirm` | Pays for a held order whose hold has not run out (it pins the hold; the receipt follows). Owner or staff. | `202`, `404`, `409` nothing to confirm or hold expired |
| `POST /api/orders/{id}/refund` | Moves a held (pending or not) or sold order to `Refunded` and puts the pieces back. The receipt is not touched. Owner or staff. | `202`, `404`, `409` wrong status |
| `GET /api/orders/{id}/events` | The Kafka event log for one order. Staff. | `200`, `403` |
| `GET /api/admin/dead-letters`, `POST /api/admin/dead-letters/{queue}/replay` | What waits in each dead-letter queue, with the first messages; publish them again. Staff. A message that still fails for the same reason is dead-lettered again. | `200`, `403`, `503` brokers off |
| `GET /api/receipts/{orderId}` | The receipt row. Owner or staff. | `200`, `404` |

Errors come back as `application/problem+json` with a `title` and the `correlationId`.

## Configuration

The process is configured from the environment (the ConfigMap and Secret in the cluster). Defaults work against `make deps`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://flash:flash@localhost:5492/flash?sslmode=disable` | The one Postgres database; each module has its own schema. |
| `RABBITMQ_URL` / `KAFKA_BROKERS` | `amqp://platform:platform@localhost:5722/` / `localhost:9142` | Work queue and event log. |
| `MESSAGING_ENABLED` | `true` | Switch the relay and the consumers off (the integration tests do). |
| `JWT_KEY` | none | HS256 signing key, at least 32 bytes. Required; the process refuses to start without it (or `JWT_RSA_PRIVATE_KEY_PEM`). |
| `JWT_ACCESS_MINUTES` / `JWT_REFRESH_DAYS` / `JWT_SECURE_COOKIES` | `15` / `14` / `false` | Token lifetimes; set `JWT_SECURE_COOKIES` when TLS ends in front of the API. |
| `JWT_RSA_PRIVATE_KEY_PEM` / `JWT_KEY_ID` | unset / `flash-1` | Sign with RS256 and publish the public key at `/.well-known/jwks.json`. |
| `JWT_REVOCATION_CACHE_SECONDS` | `5` (`2` in the demo) | How long a replica trusts a cached session version before re-reading it. |
| `ADMIN_EMAIL` / `ADMIN_PASSWORD` | unset | The administrator created on first migration. |
| `AUTH_REQUIRE_CONFIRMED_EMAIL` / `AUTH_PUBLIC_URL` | `true` / `http://localhost:4312` | Sign-in waits for email confirmation; the address links in emails point to. |
| `MAIL_HOST` / `MAIL_PORT` / `MAIL_USER` / `MAIL_PASSWORD` / `MAIL_FROM` / `MAIL_START_TLS` | unset | SMTP for confirmation and reset emails. Unset means the log only. |
| `MAIL_DEV_MAILBOX` | `false` (`true` in the demo) | Keep the last messages in Postgres and serve them at `/api/dev/mailbox`. Never on in production. |
| `RATE_LIMIT_AUTH_PER_MINUTE` | `20` (`1000` in the demo) | Requests a minute per client to `/api/auth/*`. |
| `FORWARDED_HEADERS` | `false` (`true` behind nginx) | Trust `X-Forwarded-For` and `-Proto` from the reverse proxy. |
| `OUTBOX_RETENTION_DAYS` / `INBOX_RETENTION_DAYS` | `7` / `30` | Sent outbox rows and old inbox receipts are deleted after this long. The inbox is kept longer because it must outlive any redelivery. |
| `ORDERS_FINAL_MARK_DELAY_MS` | `0` (`1500` in the demo) | Milliseconds the final mark waits. A latency knob so the smoke script can land a refund inside the window. |
| `MIGRATE_ON_START` | `true` | `false` in the cluster, where the init container migrates. |
| `LISTEN_ADDR` / `LOG_JSON` | `:8080` / `false` | |
| `TEST_DATABASE_URL` (tests only) | `postgres://flash:flash@localhost:5492/flash_test?sslmode=disable` | The Postgres the integration tests use; the database is created if it is missing. |

## Look inside and fix things

```bash
make cluster-status                                   # pods, services, disruption budgets
kubectl -n flash logs -f deploy/api --all-containers   # JSON logs (one line per request, one per worker event)
kubectl -n flash exec postgres-0 -- psql -U flash -d flash -c 'select routing_key, sent_at from orders.outbox_messages order by id desc limit 10'
kubectl -n flash delete pod -l app.kubernetes.io/name=api --wait=false   # a replica dies; the Service keeps answering
open http://127.0.0.1:15722                           # RabbitMQ management, platform / platform
```

- **Dead-lettered messages** sit in `receipts.work.dlq` (and the two other `.dlq` queues). Staff can list and replay them from
  `/api/admin/dead-letters`, or inspect them in the management UI. Retries wait in `<queue>.retry` for 2 seconds.
- **A port is already in use**: `lsof -nP -i :8112`. The kind cluster and `make deps` both want RabbitMQ's management port
  (15722); stop one before starting the other.
- **Start clean**: `make cluster-down && make cluster-up` (the PVCs go with the cluster) and `make deps-down` for the
  dependencies.
- **A pod is not Ready after `make cluster-up`**: `kubectl -n flash describe pod <name>` and the logs of the `migrate` init
  container, which is where a wrong database password shows up first.

## Known limits

- It runs on **kind**, a one-node cluster on your machine. Nothing here is a cloud deployment: there is no ingress controller,
  no TLS, no autoscaler (the HPA needs a metrics server) and one broker of each kind. The manifests are plain Kubernetes and
  Kustomize, and the stateful services are the part you would replace with managed ones.
- The expiry is checked by the sweeper, not by a timer per hold: between the deadline and the next sweep (a second in the
  base manifests, half a second in the demo) a late shopper can still confirm, because the confirm only fails once the row
  says so. The conditional `UPDATE` still guarantees that the hold is either confirmed or expired, never both.
- The cap is per account. Nothing stops one person registering a second account, so the limit is only as hard as the account
  is to duplicate.
- Revocation is eventual by design: a replica may honour a revoked access token for up to `JWT_REVOCATION_CACHE_SECONDS` seconds.
  Set it to `0` to check the database on every request.
- Delivery is at-least-once, not exactly-once. The inbox makes repeats harmless for the saga; the Kafka log can hold a
  duplicate event after a relay crash.
- `GET .../events` reads the Kafka topic for one key, so it scans the topic. Fine here, not something to expose at scale.
- Sign-in is email and password plus optional TOTP. There are no external identity providers and no WebAuthn passkeys.
- The browser specs drive Chromium at a desktop size only, and the `e2e` CI job needs a few minutes to build and start the cluster.

## Layout

```
cmd/api            the binary: `serve` and `migrate`
internal/app       wiring, the middleware chain, the background workers
internal/identity  users, roles, JWT, refresh tokens, two-factor, email flows      (schema identity)
internal/orders      order rules, the saga's first step, confirm and the handler   (schema orders)
internal/deals       the deal, the cap, the pieces, the holds, the sweeper         (schema deals)
internal/receipts    receipts                                             (schema receipts)
internal/platform  config, pgdb (pool, migrations), httpx, messaging (outbox, relay, consumers, Kafka log, dead letters)
internal/integration  the integration suite     internal/testutil  the shared test database
apps/web           Angular 21, styled with Tailwind 4 utilities only (shared with dotnet-flash)
e2e                Playwright specs
deploy/k8s         Kustomize base and the kind overlay       deploy/kind  the cluster     deploy/compose.deps.yaml  local dependencies
scripts            smoke.sh, resilience.sh, kind-secrets.sh
Dockerfile         two targets: api (distroless) and web (nginx)
```

## Other systems in this series

- [dotnet-flash](https://github.com/joshbarros/dotnet-flash): this system on ASP.NET Core and Docker Compose, with the same API and UI.
- [golang-sole](https://github.com/joshbarros/golang-sole): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-mint](https://github.com/joshbarros/golang-mint): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-pledge](https://github.com/joshbarros/golang-pledge): the same kind of system for a different scarce number, also Go on Kubernetes.
- [golang-bistro](https://github.com/joshbarros/golang-bistro): the same kind of system for a different scarce number, also Go on Kubernetes.
- The `dotnet-*` series (freight, clinic, ledger, warehouse, academy, compute, hotel, tickets, carbon, charging, promo, mint, pledge, bistro, flash, encore, atelier, sole) has the whole family on ASP.NET Core; each adds one more way to keep a scarce number honest.
