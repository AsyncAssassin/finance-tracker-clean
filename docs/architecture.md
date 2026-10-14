# Архитектура и решения

[README](../README.md) · [HTTP API](api.md) · [Эксплуатация](operations.md)

## Сервисы и слои

Gateway принимает HTTP и вызывает Auth или Ledger по gRPC. Auth выпускает JWT,
Gateway проверяет подпись и передаёт в Ledger идентификатор пользователя.
Ledger реализует операции с бюджетами, расходами и отчётами.

```text
HTTP -> rest -> интерфейс клиента <- grpcclient
                                      |
                                     gRPC
                                      |
                                 grpcserver -> usecase -> domain
                                                  |
                                       интерфейс Repository
                                          /             \
                                      PostgreSQL       memory
```

Домен не импортирует транспорт и хранилища. Gateway использует собственные
интерфейсы клиентов и proto-контракты. Поэтому HTTP-обработчики тестируются
с поддельными клиентами, а один набор контрактных тестов хранилища работает
с реализациями в памяти и PostgreSQL.

| Каталог | Назначение |
| --- | --- |
| [cmd/](../cmd) | Точки входа трёх сервисов и контейнерные healthcheck |
| [internal/ledger/domain](../internal/ledger/domain) | Транзакции, бюджеты, периоды, отчёты |
| [internal/ledger/usecase](../internal/ledger/usecase) | Операции, границы транзакций, импорт |
| [internal/ledger/repo](../internal/ledger/repo) | PostgreSQL, память и общий контракт Repository |
| [internal/ledger/cache](../internal/ledger/cache) | Кеш отчётов |
| [internal/auth](../internal/auth) | Правила аккаунта, регистрация, вход, Argon2id и хранилища |
| [internal/gateway](../internal/gateway) | HTTP, CSV, gRPC-клиенты и преобразование ошибок |
| [internal/pkg](../internal/pkg) | Деньги, даты, JWT, конфигурация, PostgreSQL и gRPC |
| [api/proto](../api/proto) | Контракты gRPC и сгенерированный Go-код |
| [migrations/](../migrations) | SQL-миграции Auth и Ledger |
| [sheets/](../sheets) | Apps Script, формы и тесты Google-клиента |

## Деньги и бюджеты

Суммы представлены `int64` в копейках, в PostgreSQL это `BIGINT`.
Календарная дата хранится как `DATE` без времени. Категории нормализуются в Unicode NFC;
бюджет уникален по паре `(user_id, category)` и обновляется через upsert.
Периоды `month`, `year`, `none` и расчёт отчёта описаны в
[правилах бюджета](api.md#как-работает-бюджет).

Создание расхода выполняется в одной транзакции хранилища: блокировка категории,
получение бюджета, подсчёт расходов за нужный период, проверка лимита и вставка.
В PostgreSQL advisory-блокировка действует и при отсутствии бюджета,
а существующая строка бюджета читается через `SELECT ... FOR UPDATE`.
Это также согласует первую установку бюджета с параллельной записью расхода.

Уникальный `external_id` действует в пределах пользователя. При повторе
возвращается `already_exists` с ID сохранённой операции. Блокировка категории
защищает лимит, а ограничение уникальности в БД защищает от параллельных дублей.

[Транзакция сценария](../internal/ledger/usecase/service.go),
[реализация PostgreSQL](../internal/ledger/repo/postgres/repo.go),
[проверки конкуренции через API](../e2e/input_concurrency_test.go).

## Параллельный импорт

Gateway сначала разбирает CSV. Ledger группирует строки по категории и
`external_id`: совпадение любого ключа связывает строки в одну группу.
Группа обрабатывается последовательно в порядке файла, разные группы параллельно.
Например, при лимите 1000 и расходах 600, 600, 400 одной категории отклоняется
второй расход независимо от планирования горутин.

`IMPORT_WORKERS` ограничивает одновременно записываемые строки всех импортов
одного Ledger. Каждая строка занимает соединение БД и собственную транзакцию;
предел работников не превышает половины `DB_MAX_CONNS`.
Результат сохраняется по индексу входной строки, у каждой позиции один писатель.

Отмена контекста прекращает новую работу; уже сохранённые строки остаются.
Ошибки данных относятся к отдельным строкам. Сбой хранилища останавливает импорт,
несохранённые строки помечаются как отменённые. Повтор с теми же ключами позволяет
досохранить остаток. Инвалидация кеша выполняется после обработки импорта.

[Реализация](../internal/ledger/usecase/import.go),
[тесты порядка, параллелизма и отмены](../internal/ledger/usecase/import_test.go),
[формат и сроки запроса](api.md#csv-импорт).

## Кеш отчётов

Кеш включается вместе с PostgreSQL. В режиме хранилища в памяти `REDIS_URL`
игнорируется, чтобы Redis не пережил исчезнувшие данные приложения.
По умолчанию отчёт хранится пять минут, срок задаёт `REPORT_CACHE_TTL`.

| Ключ | Содержимое | TTL |
| --- | --- | --- |
| `report:v2:ver:{user}` | Версия данных пользователя | Нет |
| `report:v2:summary:{instance}:{user}:{version}:{from}:{to}` | Отчёт за период | `REPORT_CACHE_TTL` |

После изменения данных `INCR` версии делает недействительными все прежние отчёты
пользователя. Инвалидация выполняется также при ошибке подтверждения COMMIT:
запись могла сохраниться, автоматического повтора нет. Отчёты удаляются по TTL;
перебирать ключи не требуется.
Версия читается до построения отчёта. Дополнительный счётчик изменений запрещает
сохранять результат, если во время его построения прошла запись.
Запрос, начавшийся одновременно с записью, может увидеть предыдущее состояние;
после завершения записи новые запросы используют обновлённую версию.

Redis работает без сохранения на диск, с лимитом 64 МиБ и `volatile-lru`:
вытесняются только отчёты с TTL, ключи версий сохраняются.
При недоступности Redis отчёты строятся по БД и приходят без `X-Cache`.
После ошибки Ledger делает паузу обращения к кешу на пять секунд.

Если не удалось инвалидировать кеш, чтение из него отключается на полный TTL
для всего Ledger. Записи во время этой паузы продлевают её. Перед паузой Ledger
дожидается начатых сохранений, каждое ограничено секундой. После перезапуска
новый идентификатор экземпляра исключает чтение отчётов прежнего процесса.
Механизм рассчитан на один Ledger; нескольким репликам нужна общая инвалидация,
устойчивая к сбоям отдельных процессов.

[Адаптер Redis](../internal/ledger/cache/redis/redis.go),
[проверки кеша](../internal/ledger/cache/redis/redis_test.go),
[демонстрация MISS/HIT](api.md#проверка-кеша).

## Хранение и миграции

Auth и Ledger используют одну базу PostgreSQL с отдельными схемами и ролями.
Goose применяет миграции в отдельном процессе до старта сервиса; версии
хранятся в `auth.goose_auth_version` и `ledger.goose_ledger_version`. Внешнего ключа из Ledger к таблице аккаунтов нет:
доверенный Gateway определяет `user_id` из JWT.

| Таблица | Сервис | Данные и ограничения |
| --- | --- | --- |
| `users` | Auth | ID, уникальный нормализованный email, хеш пароля, время регистрации |
| `budgets` | Ledger | Уникальные `(user_id, category)`, лимит в копейках, период |
| `transactions` | Ledger | ID, владелец, сумма, категория, описание, дата, ключ повтора |

Миграции встроены в `cmd/migrate`. Compose запускает его отдельно для каждого
сервиса с ролью владельца своей схемы, затем запускает Auth и Ledger с
ограниченными правами. Для локальной разработки используется `make migrate`.
[Права ролей и переход существующей базы](operations.md#роли-postgresql-и-переход-существующей-базы).

## gRPC и обработка ошибок

Контракты: [Ledger](../api/proto/ledger/v1/ledger.proto) и
[Auth](../api/proto/auth/v1/auth.proto). Деньги передаются как копейки `int64`,
даты как `YYYY-MM-DD`. Генерация: `make proto`, нужны `protoc`, `protoc-gen-go`
и `protoc-gen-go-grpc`; сгенерированные файлы хранятся в репозитории.

| HTTP | gRPC |
| --- | --- |
| `POST /api/auth/register`, `/api/auth/login` | `AuthService.Register`, `Login` |
| `POST/GET /api/transactions` | `LedgerService.CreateTransaction`, `ListTransactions` |
| `POST /api/transactions/import` | `ImportTransactions` |
| `GET /api/transactions/export.csv` | `ExportTransactionsCSV` |
| `POST/GET /api/budget`, алиас `/api/budgets` | `UpsertBudget`, `ListBudgets` |
| `GET /api/reports/summary` | `GetSummary` |

CSV-экспорт передаётся серверным потоком по 64 КиБ, обходя предел одного
сообщения gRPC. Документ всё равно собирается в памяти Gateway и Ledger;
экспорт ограничен 100 000 строками. Это не потоковая обработка всего пути.

| gRPC | HTTP и результат |
| --- | --- |
| `INVALID_ARGUMENT` с `BadRequest` | 400 `validation_error`, ошибки полей |
| `INVALID_ARGUMENT` без деталей | 400 `invalid_request` |
| `FAILED_PRECONDITION` с `BudgetExceeded` | 409 `budget_exceeded` |
| `ALREADY_EXISTS` с `DuplicateTransaction` | 409 `already_exists` |
| `ALREADY_EXISTS` от Auth | 409 `email_taken` |
| `UNAUTHENTICATED` от Auth | 401 `invalid_credentials` |
| `PERMISSION_DENIED` от Auth | 403 `registration_closed` |
| `RESOURCE_EXHAUSTED` с `ErrorInfo: AUTH_BUSY/LEDGER_BUSY` | 503 `server_busy` |
| `RESOURCE_EXHAUSTED` с `ErrorInfo: ACCOUNT_QUOTA/STORAGE_QUOTA` | 409 `quota_exceeded` |
| `RESOURCE_EXHAUSTED` от Auth с `RetryInfo` | 429 `rate_limited`, `Retry-After` |
| Прочие `FAILED_PRECONDITION`, `ALREADY_EXISTS`, `ABORTED` | 409 `conflict` |
| `NOT_FOUND` | 404 `not_found` |
| `UNAVAILABLE` | 502 `ledger_unavailable` или `auth_unavailable` |
| `DEADLINE_EXCEEDED` | 504 `timeout` |
| `CANCELLED` | 499 `client_closed_request` |
| Прочие, включая `INTERNAL` и `UNKNOWN` | 500 `internal_error` |

Детали внутренних ошибок остаются в логах; клиент получает общий текст и
`request_id`. Gateway передаёт `X-Request-ID` в gRPC-метаданных `x-request-id`.
[Преобразование общих статусов](../internal/gateway/upstream/upstream.go).

Вызовы ограничены `LEDGER_TIMEOUT` и `AUTH_TIMEOUT`; для импорта действует
`IMPORT_TIMEOUT`. После возврата недоступного сервиса клиент переподключается.
При штатной остановке gRPC health меняется на `NOT_SERVING`, текущим запросам
даётся `SHUTDOWN_TIMEOUT`. В обычном Compose внутренние порты не публикуются;
[прямые вызовы gRPC](operations.md#прямой-доступ-к-grpc) доступны в режиме разработки.
