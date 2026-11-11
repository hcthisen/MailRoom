# Mailroom

Mailroom is a self-hosted email marketing app by Scale By Tech for contacts, tags, sign-up forms, linear drip sequences, broadcasts, an email inbox, unsubscribes and Amazon SES delivery handling. It runs as one Go binary with a SQLite database in one Docker container and is released under the MIT licence.

This README is for a developer or coding agent who needs to **change** Mailroom. The other documents are:

| Document | What it covers |
|---|---|
| [PRD.md](PRD.md) | What Mailroom does (requirements). The "Build notes" at the end list where the build differs |
| [ARCHITECTURE.md](ARCHITECTURE.md) | How the code is built: packages, shared contracts, workers, decisions |
| [docs/api.md](docs/api.md) | Guide for agents that operate Mailroom through the REST API (served at `/api/v1/docs`) |
| [docs/import-format.md](docs/import-format.md), [docs/sequence-format.md](docs/sequence-format.md) | The contacts CSV and sequence YAML formats |
| [api/openapi.yaml](api/openapi.yaml) | Snapshot of the generated OpenAPI 3.1 spec |
| `../AGENTS.md`, `../deploy/README.md` | How the kit installs and operates Mailroom on a server |

## Code layout

```text
app/
  cmd/mailroom/        the binary: subcommand dispatch (one cmd_*.go file per command)
  internal/            all Go packages (see the map below)
  docs/                Markdown docs embedded in the binary (package docs)
  static/              admin UI assets embedded in the binary: htmx.min.js (vendored, see VENDOR.md), app.css, app.js, editor.js
  api/openapi.yaml     OpenAPI snapshot, checked by a test
  vendor/              vendored Go modules (go mod vendor), committed for offline builds
  Dockerfile           multi-stage build: golang:alpine → distroless/static, non-root uid 65532
  scripts/test.sh      test runner (plain go test; on Windows it runs the tests in WSL)
```

Package map. Foundation packages come first. A domain package imports only foundation packages and the domain packages built before it (see ARCHITECTURE.md §2).

| Package | What it does |
|---|---|
| `cmd/mailroom` | CLI: `serve`, `admin create`, `token create`, `doctor`, `provision ses/dns`, `dns records`, `backup`, `restore`, `migrate`, `version`, plus the hidden `healthcheck` |
| `internal/config` | Reads the environment once at start and validates it. Creates `<data>/secret.key` if needed |
| `internal/db` | SQLite (modernc, pure Go) with a one-connection write pool and a read pool. Embedded forward-only migrations, tx/time/JSON helpers, `VACUUM INTO` backups |
| `internal/core` | Shared types (Contact, statuses, message kinds, Actor), sentinel errors, and the cross-package interfaces (Renderer, SegmentResolver, TagReferrer, Enroller, EmailProvider, Alerter) |
| `internal/hooks` | Synchronous in-process event bus (TagAdded, StatusChanged, ContactCreated). Handlers run inside the publisher's transaction |
| `internal/tokens` | HMAC-signed public tokens for unsubscribe, click, open and opt-in links, each with its own purpose |
| `internal/outbox` | `Enqueue`, the only way to create a `messages` row. Idempotent through unique constraints |
| `internal/contactstore` | Core contact writes (create, update, tags, status transitions, consent log, opt-out marker), each in a SAVEPOINT |
| `internal/testutil` | Temp DB, fake clock, fake email provider and fixtures (imported by tests only) |
| `internal/server` | HTTP server (autocert or `HTTP_ONLY`), `/healthz`, security headers, request log, worker supervisor, graceful shutdown |
| `internal/api` | Huma v2 setup, error format, token/session auth and scopes, rate limit, Idempotency-Key, ETag/If-Match, pagination, dry run, route registry |
| `internal/auth` | Users (argon2id, TOTP), sessions + CSRF, login limits, API tokens, CLI bootstrap functions |
| `internal/audit` | `audit.Log` (written in the caller's tx) and `GET /api/v1/audit` |
| `internal/jobs` | Background job runner (`jobs` table), resumable jobs, `GET /api/v1/jobs` |
| `internal/settings` | Typed business settings (JSON in the `settings` table), seeded from the environment on first start |
| `internal/contacts` | Contacts, tags, suppressions API: search, status changes, bulk tag job, timeline, GDPR export and erase |
| `internal/segments` | Segment filter model, SQL compiler, count and preview. Implements SegmentResolver and TagReferrer |
| `internal/content` | Emails, base layout, own merge-field engine, Markdown, renderer (implements Renderer), tracking links, text version, system templates, test sends, media |
| `internal/sending` | Sender worker, global token bucket, SES v2 provider, MIME, retries, final eligibility check, SES account poller, sending pause state, alerts (implements Alerter), messages/events/clicks API |
| `internal/system` | Health, version, doctor, backups (local + S3), restore, retention, health-watch alerts, SES status API |
| `internal/provision` | `provision ses`, `provision dns --cloudflare`, `dns records` (AWS SDK and the Cloudflare HTTP API) |
| `internal/events` | SNS webhook `/hooks/ses` (signature v1/v2, pinned topic), bounces, complaints and deliveries, `/u` `/c` `/o` handlers, SNS auto-subscribe |
| `internal/sequences` | Sequences, steps, enrollments, the scheduler, tag triggers (hook subscriber), YAML import/export. Implements Enroller |
| `internal/broadcasts` | Broadcasts, state machine, audience snapshot job, starter worker, circuit breaker, stats, web archive `/archive/{id}` |
| `internal/forms` | Forms CRUD, public `/f/*` endpoints (hosted page, submit with CORS, honeypot, rate limit), double opt-in, expiry worker |
| `internal/imports` | Contacts CSV import with dry run, as a resumable job |
| `internal/inbound` | Receiving email: SES receipt notifications, the `inbound-fetcher` worker (S3, MIME parsing, attachments), the Inbox API and replies (message kind `reply`) |
| `internal/stats` | Dashboard numbers (`GET /api/v1/stats/dashboard`) |
| `internal/app` | Wiring: builds every service, registers API operations, jobs, hooks, public routes, the UI and the workers. Also holds the end-to-end tests |
| `internal/ui` | Admin UI under `/admin/`: html/template + htmx, action registry, parity test |
| `internal/deps` | `//go:build tools` file that pins the third-party modules in go.mod. Never compiled into the binary |
| `docs`, `static` | `embed` packages for the Markdown docs and the UI assets |

Rules worth knowing before changing anything (details in ARCHITECTURE.md §3):

- Business logic lives on each package's `Service`. API handlers and UI handlers call the same methods, and neither contains SQL.
- The write pool has **one** connection. Never start a second write (`d.WithTx`, `d.Write.Exec…`, or a helper that writes) while you hold a transaction, or it waits forever. Functions that must join a caller's transaction take `db.Querier` or `*sql.Tx`.
- Take time from `core.Clock`, never `time.Now()`. Store it with `db.FormatTime`.
- Every write through the API or UI calls `audit.Log` in the same transaction.
- Every email is a `messages` row created with `outbox.Enqueue`. Suppression is checked by the sender right before the SES call.

## Build

You need Go (see the `go` line in `go.mod`; it is built and tested with Go 1.27). Modules come from `vendor/`: Go uses the vendor directory automatically because `vendor/modules.txt` exists, so no network is needed.

```bash
cd app
go build ./...                                  # compile everything
go vet ./...
gofmt -l cmd internal docs static               # must print nothing (vendor/ is third-party code, leave it)
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=1.0.0" -o mailroom ./cmd/mailroom
./mailroom version
```

The static Linux binary is about 43 MB, most of it the AWS SDK. `-X main.version=…` sets what `mailroom version` prints (default `dev`).

The Docker image (build context is `app/`):

```bash
docker build -t mailroom --build-arg VERSION=1.0.0 .
```

On the server the kit builds it with `sudo docker compose build` from `/opt/mailroom/` (see `../deploy/README.md`). The Dockerfile builds with `-mod=vendor` when `vendor/modules.txt` is present, and falls back to `go mod download` otherwise.

## Run locally

`DOMAIN` is required. `MAILROOM_DATA_DIR` defaults to `/data`, so set it for a local run. An `http://` `BASE_URL` needs `HTTP_ONLY=1`, because autocert serves HTTPS only.

```bash
export DOMAIN=example.com MAILROOM_DATA_DIR=./data HTTP_ONLY=1 BASE_URL=http://localhost:8080
# optional, seeds the settings on first start:
export FROM_NAME="Example News" FROM_EMAIL=news@example.com OWNER_EMAIL=you@example.org TIMEZONE=Europe/London

./mailroom serve                                # or: go run ./cmd/mailroom serve
```

`HTTP_ADDR` defaults to `:8080`, and `serve --http-only` works the same as `HTTP_ONLY=1`. In a second shell with the same environment:

```bash
./mailroom admin create --email you@example.org        # prints a generated password once
./mailroom token create --name dev --scope admin       # prints an mr_… API token once
```

Then open <http://localhost:8080/> (it redirects to `/admin/`), or call the API:

```bash
curl -H "Authorization: Bearer mr_…" http://localhost:8080/api/v1/system/health
curl http://localhost:8080/api/v1/openapi.json          # the spec (also .yaml); guide at /api/v1/docs
curl http://localhost:8080/healthz
```

Other environment variables are listed in ARCHITECTURE.md §5. The data directory then holds `mailroom.db` (plus `-wal`/`-shm`), `secret.key` and `backups/`. `media/`, `imports/` and, with autocert, `autocert/` appear once they are used.

**No fake or dev sending mode.** Without the sending user's SES keys (`AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`), the sender and the SES account poller don't start and a warning is logged. Everything else works: forms, sequences and broadcasts queue their messages, and you can inspect them with `GET /api/v1/messages`. Nothing is sent, so broadcasts stay in `sending`, because only the sender completes them. To see real mail, use a sending user's keys and the SES mailbox simulator (`success@`, `bounce@`, `complaint@simulator.amazonses.com`). To exercise the whole flow without AWS, write a test with the `internal/app` harness (`app.Options{Offline: true, Provider: testutil.NewFakeProvider()}`, with workers driven by `sending.SendDue`, `sequences.RunDue` and `broadcasts.Tick`). The SNS subscriber also needs `SNS_TOPIC_ARN` and a public HTTPS URL, so bounces don't arrive locally.

The `ui-doctor` worker runs the doctor checks 30 s after start and then every 10 min. Some of these checks use DNS and HTTPS on the network. They fail harmlessly for `example.com`.

**Windows note:** on managed Windows machines, app-control policy may block freshly built `.exe` files, including `mailroom.exe` and test binaries. Then build for Linux and run the binary in WSL:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o mailroom-linux ./cmd/mailroom
wsl.exe -d Ubuntu -- env DOMAIN=example.com MAILROOM_DATA_DIR=./data HTTP_ONLY=1 BASE_URL=http://localhost:8080 ./mailroom-linux serve
```

## Test

Tests use no network and no Docker. Each test gets a temp SQLite database with the migrations applied (`testutil.NewDB`), a fake clock and a fake email provider.

```bash
bash scripts/test.sh                                    # everything (./...)
bash scripts/test.sh ./internal/sending/ -run TestSender -v
bash scripts/test.sh ./internal/app/ -count=1           # end-to-end tests through the real HTTP handler
```

`scripts/test.sh` runs plain `go test` on Linux and macOS, and you can use `go test ./...` there directly. On Windows (Git Bash, `OS=Windows_NT`) it cross-compiles the tests for Linux (`GOOS=linux GOARCH=amd64 CGO_ENABLED=0`) and runs each test binary inside WSL through `scripts/wsl-exec.sh`, because app control can block unsigned test `.exe` files. It needs a WSL distribution, `Ubuntu` by default. Set `MAILROOM_WSL_DISTRO` to use another one. Don't run plain `go test` on such a machine. When you pass arguments, name the packages yourself, as in the examples: the script uses `./...` only when it gets no arguments at all.

Tests that pin contracts beyond ordinary unit tests:

| Test | Package | Fails when |
|---|---|---|
| `TestSpecSnapshot` | `internal/app` | the generated spec differs from `api/openapi.yaml` |
| `TestOperationsRegisteredOnce` | `internal/app` | an operation is missing or unlisted in `expectedOperations`, lacks a summary or description, or a GET needs the admin scope (only `download-backup` does) |
| `TestWorkers` | `internal/app` | the set of supervised workers changes |
| `TestUIAPIParity` | `internal/ui` | a UI action names an unknown operation, or (with `strictParity`) an API write has no UI action and isn't allowlisted |
| `TestDockerfile`, `TestComposeFile` | `cmd/mailroom` | the Dockerfile or `../deploy/docker-compose.yml` no longer matches the binary, or `ARG GO_VERSION` is older than go.mod's `go` line |
| `TestEmbedded` | `docs` | an embedded document is missing |

`go test -race` needs cgo. The build doesn't rely on it.

## Regenerate the OpenAPI snapshot

Huma generates the spec from the registered operations, and the server serves it at `/api/v1/openapi.json` and `/api/v1/openapi.yaml`. After you change an operation or a request/response type, rewrite the snapshot and review the diff:

```bash
bash scripts/test.sh ./internal/app/ -run TestSpecSnapshot -update
```

On Linux/macOS that is `go test ./internal/app -run TestSpecSnapshot -update`. Without `-update`, the test fails while the snapshot is out of date.

## Add an API operation and its UI action

1. **Service method.** Put the logic on the owning package's `Service`, using core errors (`core.Invalid(field, …)`, `core.ErrNotFound`, `core.ErrConflict`, …). For a write: `d.WithTx(ctx, …)` with the **request** ctx, so that an Idempotency-Key commits together with the change, plus `audit.Log` in the same tx. Take `If-Match` with `api.IfMatchParam` and return `api.ETag(version)`. Where the PRD asks for a dry run, run the work in a transaction and roll it back.
2. **Register it** in the package's `RegisterAPI(a *api.API)` with `api.Register(a, huma.Operation{…}, handler)`. The operation needs `OperationID` (kebab-case, unique), `Method`, `Path` (relative to `/api/v1`), `Summary`, `Description` and `Tags`, and `Register` panics without them. The scope defaults to `read` for GET and `admin` for everything else. Override it with `Metadata: map[string]any{api.MetaScope: api.ScopeRead}`, e.g. for a read-only POST such as a preview. Set `api.MetaSensitive: true` when the response holds a secret shown once. Add `example:"…"` and `doc:"…"` struct tags to the types. Long work submits a job (`runner.Submit(ctx, tx, type, params)` in the same tx) and answers 202.
3. **New package?** Wire it in `internal/app/app.go` (`build`): construct it, add it to the `RegisterAPI` list, call its `RegisterJobs`, add its workers, and pass it to `ui.Deps` if the UI needs it.
4. **List it** in `expectedOperations` in `internal/app/wiring_test.go`.
5. **Regenerate the snapshot** (above).
6. **UI action (PRD G-1).** In `internal/ui/<group>_*.go`, register the action inside the group's `registerGroup(...)` function:

   ```go
   u.handle(Action{Method: http.MethodPost, Path: Prefix + "/contacts/{id}/thing",
       OperationID: "do-contact-thing", Handler: u.contactThing})
   ```

   The handler calls the **same service method** as the API handler. `OperationID` must exist in `api.Routes()`. A GET action must not claim a write operation. `Uses` lists further operations the handler calls. `UIOnly` is only for actions with no API counterpart, such as login. CSRF (non-GET) and the session check come from `handle`. An API write that deliberately has no UI goes in `parityAllowlist` in `internal/ui/parity_test.go`, with a one-line reason.
7. **Docs.** If an agent recipe needs it, add it to `docs/api.md`. Every endpoint named there must exist in the spec.
8. `gofmt`, `go vet ./...`, `bash scripts/test.sh`.

## Upgrade Go, dependencies and the base image

There is no update channel. Upgrade on a development machine, run the tests, then copy `app/` to the server and rebuild there (`sudo docker compose build && sudo docker compose up -d`, see `../deploy/README.md`).

**Go toolchain.** The `go` line in go.mod (`go 1.26.0`) is the minimum language version. It is the minimum that `golang.org/x/*` and `modernc.org/sqlite` require. The image builds with `ARG GO_VERSION` in the Dockerfile (`golang:${GO_VERSION}-alpine`, `GOTOOLCHAIN=local`), so that version must be at least the `go` line. `TestDockerfile` checks this. To get a newer compiler and standard library (security fixes), raise `ARG GO_VERSION` (e.g. `1.27`), or pass `docker compose build --build-arg GO_VERSION=1.27`. Raise the `go` line (`go mod edit -go=1.27.0`) only when the code needs newer language features, or a dependency requires it.

**Go modules:**

```bash
go get -u ./...          # or one module: go get github.com/danielgtaylor/huma/v2@latest
go mod tidy
go mod vendor            # required: builds use vendor/, and a stale vendor/ fails with "inconsistent vendoring"
go build ./... && go vet ./... && bash scripts/test.sh
bash scripts/test.sh ./internal/app/ -run TestSpecSnapshot -update   # if a Huma upgrade changes the spec output; review the diff
go run golang.org/x/vuln/cmd/govulncheck@latest ./...                # known vulnerabilities (needs network)
```

These commands need network access to the Go module proxy. If `go get -u` raises the `go` line, raise `ARG GO_VERSION` too. New third-party modules go into the ARCHITECTURE.md §1 table and `internal/deps/deps.go`. `vendor/` is about 150 MB, because `modernc.org/sqlite` ships its generated code for every platform. It is committed so the server can build offline. `static/htmx.min.js` is upgraded separately, following `static/VENDOR.md`.

**Base images.** The build stage is `golang:${GO_VERSION}-alpine`, and the runtime is `gcr.io/distroless/static-debian12:nonroot`. The runtime image contains only CA certificates, tzdata and the nonroot user (uid 65532). To pick up patched base images without code changes, run `sudo docker compose build --pull`. To move to a newer distroless release (e.g. `static-debian13`), edit the `FROM` line and update the expected string in `TestDockerfile` (`cmd/mailroom/deploy_test.go`). Keep `USER 65532:65532`, because `data/` and `.env` on the server belong to that uid.

## Database migrations

Migrations are SQL files in `internal/db/migrations/`, named `NNNN_description.sql` and embedded in the binary. `0001_init.sql` is the whole v1 schema; `0002_autoincrement_ids.sql` rebuilds `segments`, `emails`, `sequence_steps` and `broadcasts` with AUTOINCREMENT. They are **forward-only**: never edit a migration that may have run on an install. Add a new file instead.

1. Create `internal/db/migrations/0002_<what>.sql`, e.g. `0002_broadcast_breaker_since.sql`. Don't write `BEGIN`/`COMMIT`: each migration runs in its own transaction, on the write connection, with foreign key enforcement off. `PRAGMA foreign_key_check` must come back empty before the commit, or the migration is rolled back. So the standard SQLite table rebuild is safe: create the new table, copy, drop the old one, rename, recreate the indexes. Store times as `db.FormatTime` text (fixed-width UTC, see the header of 0001). Give JSON columns `CHECK (json_valid(...))`, and give mutable API resources a `version` column.
2. Change the Go code that reads and writes the tables, and the ARCHITECTURE/PRD data notes if they are affected.
3. Run the tests. `testutil.NewDB` applies every migration, so all package tests run against the new schema. `internal/db` tests cover the runner.

At start, `mailroom serve` applies the pending migrations. When the database already has a schema, it first writes a backup, `<data>/backups/pre-migrate-v<N>-<time>.db`. If that backup fails, nothing is migrated. `mailroom migrate` does the same offline. A database that has a migration the binary doesn't know is refused ("use a newer binary"). To go back to an older binary, stop the server and run `mailroom restore <pre-migrate backup>`. A restore always leaves sending paused, and carries opt-outs, erasures and sent messages over from the database it replaces (see the kit's deploy/README.md, "What a restore does"): resume sending once you have checked it.

## Security notes

- **Secrets.** `MAILROOM_SECRET`, or else the generated `<data>/secret.key`, signs every unsubscribe, click, open and opt-in link. If you lose or change it, the links in mail that was already sent stop working. Back it up with the data directory. The sending user's AWS keys live only in the server's `.env` (mode 600). Setup keys (the AWS setup user, Cloudflare) are passed as environment variables to a single `provision` run and are never stored. Pass them by name only (`-e NAME`, the value in the environment), never as `NAME=value` on a `sudo` command line: sudo logs its command lines (deploy/README.md §4). Logs never contain secrets, tokens or email bodies. Addresses are logged only at debug level.
- **Data at rest.** `mailroom.db` holds contact data, argon2id password hashes and TOTP secrets. The database files are created with mode 0600. Backups contain everything, so `download-backup` needs an admin token. S3 backups are server-side encrypted. In the bucket, backups under `backups/` (and their overwritten versions) expire after 30 days; `keys/secret.key` is kept. The bucket is versioned, so an overwritten backup or key file can be recovered.
- **API.** Tokens are `mr_` + 52 base32 characters. Only their SHA-256 is stored, and each is shown once. Scopes are `admin` (everything) and `read` (GET, plus the POST previews that store nothing: preview-segment, render-email, preview-email, preview-layout). There is a per-actor rate limit. Responses marked sensitive are never stored for Idempotency-Key replay.
- **UI.** The session cookie is HttpOnly and SameSite=Lax, and Secure when `BASE_URL` is https. Every non-GET needs the CSRF token. Login is limited per address and per email (see ARCHITECTURE.md §7), and TOTP codes can't be reused. The admin UI has a strict CSP (scripts only from its own origin, no inline scripts, no eval), `X-Frame-Options: DENY` and `Cache-Control: no-store`. Email previews render in sandboxed iframes without scripts. Templates are `html/template`, so user content is always escaped.
- **Public endpoints.** SNS messages are accepted only with a valid signature (v1/v2) from `sns.<region>.amazonaws.com`, and only for the pinned `SNS_TOPIC_ARN`. Public tokens are HMAC-signed per purpose, and an unsubscribe token unlocks nothing else. Forms, click/open recording, the web archive, media files and the unsubscribe page are rate-limited per client address. The unsubscribe POST and the SNS webhook are not, on purpose (a one-click unsubscribe must never be refused, and SNS must never lose a bounce); they check an HMAC token or the SNS signature first. The client address is taken from `X-Forwarded-For` only for `TRUSTED_PROXIES`. HTTPS responses carry HSTS.
- **Not built:** restricting the admin UI to IP ranges. Use the server firewall or a reverse proxy for that. There is also no CI in the kit, so run `govulncheck` yourself when you upgrade.
- To report or fix a vulnerability, you own this copy: fix it here, run the tests, and redeploy.

## Licence

MIT, like the rest of the kit (see `../LICENSE`). No listmonk (AGPL) code was copied. Only design ideas were borrowed (PRD §9). The third-party modules in `vendor/` keep their own licences. The htmx licence (0BSD) is recorded in `static/VENDOR.md`.
