# Finance Tracker

[Русский](README.md) | **English**

[![ci](https://github.com/AsyncAssassin/finance-tracker-clean/actions/workflows/ci.yml/badge.svg)](https://github.com/AsyncAssassin/finance-tracker-clean/actions/workflows/ci.yml)

A Go application for personal expenses and budgets. Users set limits, record
expenses and view category reports in Google Sheets. The HTTP API also supports
CSV import and export.

**Stack:** Go, gRPC, PostgreSQL, Redis, Google Apps Script, Docker Compose, Caddy.

[Try the app](#try-the-app) · [Quick start](#quick-start) · [API example](#api-example) ·
[Google Sheets](#google-sheets) · [Tests](#tests) · [Documentation](#documentation)

## Try the app

To try a hosted instance, request the Google Sheets template, API address and
login details from the project author. Make a personal copy of the spreadsheet
and follow the [10-minute walkthrough](docs/demo.md) (in Russian): budgets,
expenses, a rejected expense and a report. Go and Docker are not required.
Access is provided individually. To run your own instance, use the
[quick start](#quick-start).

## Features

- Expenses with a date, category, amount and description.
- Monthly, yearly or all-time budgets; expenses exceeding a limit are rejected.
- Reports for a date range: spending, remaining amounts and budget usage.
- CSV import with a result for each row, and export for further analysis.
- Separate user data, registration and JWT authentication.
- Google Sheets forms and menus, with duplicate protection on retries.

## Design decisions

**Budget checks under concurrent writes.** Money is stored as integer kopecks.
Checking the limit and inserting an expense happen in one PostgreSQL transaction
with a category lock, including categories that do not have a budget yet.
[Implementation and limits](docs/architecture.md#деньги-и-бюджеты).

**Bounded concurrent import.** Rows sharing a category or `external_id` run in
order. Independent groups run concurrently with a shared worker limit. Retrying
does not create duplicates; cancellation stops remaining work and preserves
committed rows. [Import design](docs/architecture.md#параллельный-импорт).

**Cache protection against stale writes.** A user data version is part of each
report key. Budget or expense changes invalidate previous reports; if Redis
fails, reports are built from PostgreSQL. This mechanism supports one Ledger
instance. [Cache design](docs/architecture.md#кеш-отчётов).

## Architecture

```text
Google Sheets / HTTP client
              |
         HTTPS (Caddy)
              |
           Gateway
          /       \
       gRPC       gRPC
        /           \
      Auth         Ledger ----- Redis
        \           /
          PostgreSQL
```

Gateway handles HTTP, validates JWTs and calls the services over gRPC.
Auth manages accounts and tokens. Ledger handles expenses, budgets, reports
and CSV. Auth and Ledger have separate database schemas, roles and Goose
migrations. Separate migration processes run as the owners of their schemas.

Interfaces separate the domain and use cases from HTTP, gRPC and storage.
See [layers, contracts and persistence](docs/architecture.md).

## Quick start

Requires Docker with Compose 2.24.4 or later. These commands are for a new local
installation. To update an existing stack, preserve its `.env` and volumes and
follow the [upgrade guide](docs/operations.md#обновление-и-откат).

```sh
git clone https://github.com/AsyncAssassin/finance-tracker-clean.git
cd finance-tracker-clean
install -m 600 .env.example .env
printf '\nJWT_SECRET=%s\n' "$(openssl rand -base64 32)" >> .env
for name in REDIS_PASSWORD POSTGRES_PASSWORD AUTH_DB_PASSWORD LEDGER_DB_PASSWORD AUTH_MIGRATION_PASSWORD LEDGER_MIGRATION_PASSWORD; do
  printf '%s=%s\n' "$name" "$(openssl rand -hex 32)" >> .env
done
# Local example; use participant addresses for a pilot installation.
printf 'AUTH_REGISTRATION_EMAILS=anna@example.com\n' >> .env
docker compose up --build -d --wait
curl -fsS http://localhost:8080/ping
```

Registration is closed by default. The email list permits account creation but
does not verify email ownership. Follow the
[participant admission procedure](docs/operations.md#закрытый-пилот-и-квоты).

Expected response: `pong`. Auth and Gateway refuse to start without `JWT_SECRET`.
Compose completes `auth-migrate` and `ledger-migrate` before starting the services.
PostgreSQL stores data in `pgdata`; `docker compose down` preserves that volume,
while `docker compose down -v` deletes it.

The default Compose setup publishes only Gateway at `127.0.0.1:8080`.
PostgreSQL, Redis and gRPC are accessible inside the container networks.
`/ping` checks Gateway itself; login and a budget request also exercise Auth
and Ledger.

**API:** [local Swagger UI](http://localhost:8080/docs),
[OpenAPI specification](api/openapi.yaml).
Running the Go services outside containers requires Go 1.26.9 or later:
[local development](docs/operations.md#локальная-разработка).

## API example

Run the commands sequentially in one terminal. They create a separate demo
account with an allowed email and a random password. The example assumes a new
database; use login for an existing account. Requires `curl` and `openssl`.
For your own public deployment, set `BASE_URL` to its HTTPS address.

```sh
BASE_URL=http://localhost:8080
DEMO_EMAIL=anna@example.com
DEMO_PASSWORD=$(openssl rand -hex 16)
LOGIN_JSON=$(printf '{"email":"%s","password":"%s"}' "$DEMO_EMAIL" "$DEMO_PASSWORD")
AUTH_JSON=$(printf '%s' "$LOGIN_JSON" | curl -fsS "$BASE_URL/api/auth/register" \
  -H 'Content-Type: application/json' --data-binary @-)
TOKEN=$(printf '%s' "$AUTH_JSON" | sed -nE 's/.*"access_token":"([^"]+)".*/\1/p')
test -n "$TOKEN"

curl -fsS -X POST "$BASE_URL/api/budget" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"category":"еда","limit":5000,"period":"month"}'
curl -fsS -X POST "$BASE_URL/api/transactions" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"amount":"1250,50","category":"еда","description":"продукты","date":"2026-10-05","external_id":"example-1"}'
curl -fsS -H "Authorization: Bearer $TOKEN" \
  "$BASE_URL/api/reports/summary?from=2026-10-01&to=2026-10-31"
```

Registration, budget creation and expense creation return `201`. For the new
account, the report total is 1250.50 and the budget remainder is 3749.50.
Repeating the expense with the same `external_id` returns `409 already_exists`.
Another 4000 in the same category and month returns `409 budget_exceeded`.
Protected routes return `401` without a token.

Budget periods are `month`, `year` and `none`; the default is `month`.
`/api/budgets` is an alias for `/api/budget`. Repeating `POST /api/budget`
updates the budget and returns `200`.
[Full walkthrough, routes, errors and CSV](docs/api.md).

## Google Sheets

The client consists of four files in [sheets/](sheets): `Code.gs`, `Login.html`,
`AddTransaction.html` and `appsscript.json`. It requires your own
[HTTPS API](docs/operations.md#публичный-https), reachable from Google.
Menu labels below match the Russian interface.

1. Create a spreadsheet and open **Extensions → Apps Script**.
2. Enable the manifest in editor settings. Copy the four files from `sheets/`;
   name the HTML files `Login` and `AddTransaction`.
3. Save the project, reload the spreadsheet and select
   **Finance Tracker → Подготовить листы** (prepare sheets).
4. Select **Подключение…** (connection) and authorize access to the current
   spreadsheet, forms and external requests. Enter an address such as
   `https://api.example.com`. HTTPS on port 443 with no path is required;
   Google cannot reach your `localhost`.
5. Select **Войти или зарегистрироваться…** (sign in or register), check the
   server address and sign in to a prepared Finance Tracker account.
   Registration is available only for addresses in the administrator's list.
6. Fill in **Бюджеты** (budgets) and save them using the menu. Add an expense
   using the form, or fill in rows and select **Отправить новые строки**
   (send new rows).
7. On **Отчёт** (report), set dates in B1:B2 and load the report using the menu.
   Data starts at A4; D2 shows cache HIT/MISS.

Cell edits are submitted through a menu command. Stored expense IDs and keys
prevent duplicates on retries. See the [demo with expected totals](docs/demo.md)
and [copying a template and resolving access issues](docs/sheets.md).

## Tests

```sh
make test              # Go tests; external integrations are enabled separately
make race              # Go tests with the race detector
make test-integration  # Separate PostgreSQL database and Redis DB 15
make sheets-test       # Apps Script in Node 22+, without the Google UI
make cover-all         # Full profile with PostgreSQL, Redis and -race
```

Run HTTP e2e on a separate test stack:
[commands and verification boundaries](docs/testing.md).
[cover.out](cover.out) is retained for project assessment. Its scope and how
to generate a new profile are described in [testing](docs/testing.md#покрытие-и-ci).
Run results and artifacts are available in
[GitHub Actions](https://github.com/AsyncAssassin/finance-tracker-clean/actions/workflows/ci.yml).
CI checks Go code, integrations, HTTP scenarios, the Sheets script, HTTPS
configuration, dependencies and secrets. Node tests do not replace a run in
the Google interface.

## Deployment limits

The configuration supports a closed pilot with one Auth and one Ledger.
Limits are 100 accounts per installation, 10,000 transactions and 100 categories
per account. Login counters are held in Auth memory; cache coordination between
multiple Ledger instances is not implemented. There is no password recovery
or individual JWT revocation: signing out clears the client session, while the
issued token remains valid until expiry. CSV export is limited to 100,000 rows
and is assembled in memory.
[Access model](docs/architecture.md#модель-доступа) and
[deployment limits](docs/architecture.md#ограничения-развёртывания).

## Documentation

Detailed guides and the security policy are in Russian.

| Document | Contents |
| --- | --- |
| [API](docs/api.md) | Routes, errors, budgets, curl walkthrough, CSV and HIT/MISS |
| [Architecture](docs/architecture.md) | Layers, gRPC, transactions, concurrency, cache and access model |
| [Operations](docs/operations.md) | Environment, development, HTTPS, upgrades and recovery |
| [Google Sheets](docs/sheets.md) | Personal copies, menus, sessions and troubleshooting |
| [Testing](docs/testing.md) | Commands, data isolation, test levels and CI |
| [Report a vulnerability](SECURITY.md) | Supported branch and private contact |
| [Demo](docs/demo.md) | Budgets, expenses, a rejected expense and a report |
| [Project criteria](docs/assessment.md) | Assignment mapping and assessment steps |

This project is part of a Go course. The [assignment mapping](docs/assessment.md)
connects requirements to the implementation and verification scenarios.
