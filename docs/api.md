# HTTP API и примеры

[README](../README.md) · [OpenAPI](../api/openapi.yaml) · [Архитектура](architecture.md)

Публичные маршруты: регистрация, вход, `/ping`, `/docs`, `/openapi.yaml`.
Финансовые операции требуют `Authorization: Bearer <access_token>`.
Пользователь определяется из проверенного токена; передать другого владельца
через тело, заголовок или параметр запроса нельзя. [Модель доступа](security.md#модель-доступа).

Ответы `/api/`, включая ошибки и CSV, содержат `Cache-Control: no-store`.
Все ответы Gateway содержат `X-Content-Type-Options: nosniff`.

## Маршруты

Эндпоинты с отметкой JWT требуют заголовок `Authorization: Bearer <access_token>`;
без него или с недействительным токеном возвращается `401`.

| Метод и путь | Что делает | Ответ |
|---|---|---|
| `GET /ping` | проверка живости самого Gateway, без его зависимостей | `200 pong` |
| `POST /api/auth/register` | создать аккаунт и сразу получить токен | `201`, `400`, `403`, `408`, `409`, `413`, `429`, `503` |
| `POST /api/auth/login` | войти и получить новый токен | `200`, `400`, `401`, `413` |
| JWT `POST /api/transactions` | добавить транзакцию с проверкой бюджета | `201`, `400`, `409`, `413` |
| JWT `GET /api/transactions` | список транзакций, новые сначала; фильтры `from`, `to`, `category`, `limit`, `offset` | `200` |
| JWT `POST /api/transactions/import` | импорт CSV, итог по каждой строке | `200`, `400`, `408`, `413`, `415` |
| JWT `GET /api/transactions/export.csv` | экспорт в CSV, те же фильтры | `200 text/csv` |
| JWT `POST /api/budget` | создать или заменить бюджет категории (upsert) | `201` создан, `200` обновлён, `400`, `413` |
| JWT `GET /api/budget` | список бюджетов | `200` |
| JWT `GET /api/reports/summary?from=&to=` | сводный отчёт за период; заголовок `X-Cache: HIT\|MISS` | `200`, `400` |
| `GET /openapi.yaml`, `GET /docs` | OpenAPI и Swagger UI | `200` |

Суммы передаются числом с не более чем двумя знаками после запятой (`1250.50`)
или строкой (`"1250,50"`), внутри хранятся в копейках, поэтому `0.10 + 0.20`
всегда ровно `0.30`. Даты: строки `YYYY-MM-DD`. Категории приводятся к нижнему
регистру: «Еда» и «еда» считаются одной категорией.

## Примеры

Выполняйте команды по порядку в одном окне терминала. Для каждого полного прогона
для новой базы создаётся демонстрационный аккаунт с разрешённым email.
Для уже созданного аккаунта используйте вход; прежние расходы останутся в отчёте. Пример
использует только вымышленные данные. Пароль генерируется локально, остаётся в
переменной текущего терминала и не записывается в репозиторий.

Для локального стека оставьте `http://localhost:8080`. При работе со своим
сервером замените `BASE_URL` на его HTTPS-адрес, например `https://api.example.com`.

```sh
BASE_URL=http://localhost:8080
DEMO_EMAIL=anna@example.com
DEMO_PASSWORD=$(openssl rand -hex 16)
LOGIN_JSON=$(printf '{"email":"%s","password":"%s"}' "$DEMO_EMAIL" "$DEMO_PASSWORD")
AUTH_JSON=$(printf '%s' "$LOGIN_JSON" | curl -fsS "$BASE_URL/api/auth/register" \
  -H 'Content-Type: application/json' --data-binary @-)
TOKEN=$(printf '%s' "$AUTH_JSON" | sed -nE 's/.*"access_token":"([^"]+)".*/\1/p')
DEMO_USER_ID=$(printf '%s' "$AUTH_JSON" | sed -nE 's/.*"user":\{"id":([0-9]+).*/\1/p')
test -n "$TOKEN" && test -n "$DEMO_USER_ID" && echo 'Аккаунт создан'
```

Адрес должен входить в `AUTH_REGISTRATION_EMAILS`; в новом локальном стеке
пример README разрешает `anna@example.com`. Сначала создайте аккаунт;
при повторном запуске используйте вход ниже.

Регистрация возвращает `201`, пользователя и токен. Ниже условный пример ответа:
ID и время в вашем ответе будут другими, а вместо `<access_token>` будет JWT.
Токен уже сохранён в `TOKEN`; его не нужно копировать из примера.

```json
{"user":{"id":1,"email":"demo-example@example.com","created_at":"2026-10-05T10:00:00Z"},"access_token":"<access_token>","token_type":"Bearer","expires_at":"2026-10-06T10:00:00Z"}
```

Проверка повторного входа с теми же учётными данными; ответ `200`:

```sh
TOKEN=$(printf '%s' "$LOGIN_JSON" | curl -fsS "$BASE_URL/api/auth/login" \
  -H 'Content-Type: application/json' --data-binary @- \
  | sed -nE 's/.*"access_token":"([^"]+)".*/\1/p')
curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/budget"
```
```json
{"items":[]}
```

Без токена прикладные эндпоинты не отвечают:

```sh
curl -sS -i "$BASE_URL/api/budget"
```
```
HTTP/1.1 401 Unauthorized
Www-Authenticate: Bearer realm="finance-tracker"
{"error":{"code":"unauthorized","message":"an access token is required: send Authorization: Bearer <token> from POST /api/auth/login"}}
```

Создайте два месячных бюджета. Первый запрос проверяет период по умолчанию,
во втором он задан явно. Оба запроса возвращают `201`:

```sh
curl -fsS -i -X POST "$BASE_URL/api/budget" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"category":"Еда","limit":5000}'
curl -fsS -i -X POST "$BASE_URL/api/budget" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"category":"транспорт","limit":2500,"period":"month"}'
```
```json
{"category":"еда","limit":5000.00,"period":"month"}
```

Повторное сохранение бюджета обновляет существующую запись и возвращает `200`.
В этом примере лимит остаётся прежним. Список после него содержит оба бюджета:

```sh
curl -fsS -i -X POST "$BASE_URL/api/budget" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"category":"еда","limit":5000,"period":"month"}'
curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/budget"
```

Добавьте три расхода. Даты указаны явно: сценарий проверяет октябрь 2026 года
независимо от даты запуска. Каждый запрос возвращает `201` и созданную транзакцию
с ID. Эти команды отправляйте по одному разу за прогон.

```sh
curl -fsS -i -X POST "$BASE_URL/api/transactions" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"amount":"1250,50","category":"еда","description":"продукты","date":"2026-10-05"}'
curl -fsS -i -X POST "$BASE_URL/api/transactions" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"amount":300,"category":"транспорт","description":"проезд","date":"2026-10-05"}'
curl -fsS -i -X POST "$BASE_URL/api/transactions" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"amount":700,"category":"здоровье","description":"аптека","date":"2026-10-05"}'
```

Получите список сохранённых транзакций за тот же период:

```sh
curl -fsS -H "Authorization: Bearer $TOKEN" \
  "$BASE_URL/api/transactions?from=2026-10-01&to=2026-10-31&limit=100&offset=0"
```

Дополнительные 4000 на еду в том же месяце превышают лимит: 1250,50 + 4000 > 5000.
Запрос возвращает `409`, транзакция не сохраняется:

```sh
curl -sS -i -X POST "$BASE_URL/api/transactions" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"amount":4000,"category":"еда","description":"превышение лимита","date":"2026-10-05"}'
```
```
HTTP/1.1 409 Conflict
{"error":{"code":"budget_exceeded","message":"budget exceeded for \"еда\" (2026-10): spent 1250.50 + new 4000.00 > limit 5000.00","details":{"amount":4000.00,"category":"еда","limit":5000.00,"period":"month","period_label":"2026-10","remaining":3749.50,"spent":1250.50}}}
```

Отчёт за октябрь содержит только три сохранённых расхода, итог 2250,50:

```sh
curl -fsS -H "Authorization: Bearer $TOKEN" \
  "$BASE_URL/api/reports/summary?from=2026-10-01&to=2026-10-31"
```
```json
{"from":"2026-10-01","to":"2026-10-31","total":2250.50,"categories":[
  {"category":"еда","spent":1250.50,"budget":{"period":"month","limit":5000.00,"remaining":3749.50,"percent":25.01}},
  {"category":"здоровье","spent":700.00,"budget":null},
  {"category":"транспорт","spent":300.00,"budget":{"period":"month","limit":2500.00,"remaining":2200.00,"percent":12}}]}
```

Экспорт в CSV. Список содержит три строки, отклонённого расхода на 4000 в нём нет:

```sh
curl -fsS -H "Authorization: Bearer $TOKEN" -o transactions.csv \
  "$BASE_URL/api/transactions/export.csv?from=2026-10-01&to=2026-10-31"
cat transactions.csv
```
```
id,date,category,amount,description,external_id
3,2026-10-05,здоровье,700.00,аптека,
2,2026-10-05,транспорт,300.00,проезд,
1,2026-10-05,еда,1250.50,продукты,
```

ID в примере условные: на стенде и при повторных прогонах номера будут другими.
Текстовые ячейки, начинающиеся с `=`, `+`, `-` или `@`, экспортируются с ведущим
апострофом, чтобы Excel и Google Таблицы не исполняли их как формулы.

Импортируйте экспорт обратно в тот же аккаунт:

```sh
curl -fsS -X POST "$BASE_URL/api/transactions/import" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: text/csv' \
  --data-binary @transactions.csv
```

Ожидаются `created: 0`, `failed: 3` и `already_exists` у каждой строки:
колонка `id` связывает строки с уже сохранёнными операциями, итог остаётся 2250,50.

## Формат ошибок

Любая ошибка: объект `error` с кодом для программ, сообщением для людей и
необязательными деталями:

```json
{"error":{"code":"validation_error","message":"request validation failed","details":{"amount":"must be greater than 0","category":"must not be empty"}}}
```

| HTTP | `code` | Когда |
|---|---|---|
| 400 | `invalid_json` | тело не разбирается: битый JSON, повторное или неизвестное поле, сумма или дата в неверном формате |
| 400 | `validation_error` | поля не проходят проверку; `details`: поле → причина |
| 400 | `invalid_csv` | при импорте: файл не читается целиком; `details.line`: номер строки |
| 400 | `invalid_request` | ledger отклонил запрос без разбивки по полям (`INVALID_ARGUMENT` без деталей) |
| 401 | `unauthorized` | нет токена, токен подделан или истёк; заголовок `WWW-Authenticate: Bearer` |
| 401 | `invalid_credentials` | при входе: неверный email или пароль (ответ одинаков в обоих случаях) |
| 404 | `not_found` | нет такого маршрута |
| 405 | `method_not_allowed` | метод не поддерживается; допустимые: в заголовке `Allow` |
| 409 | `budget_exceeded` | лимит категории превышен; в `details` лимит, потрачено, остаток |
| 403 | `registration_closed` | регистрация не разрешена для адреса |
| 409 | `quota_exceeded` | достигнута квота аккаунтов, транзакций или категорий |
| 408 | `timeout` | тело auth-запроса не получено за 5 секунд или CSV за `IMPORT_TIMEOUT` |
| 409 | `already_exists` | транзакция с таким `external_id` уже есть; в `details.id` её номер |
| 409 | `email_taken` | при регистрации: аккаунт с таким email уже есть |
| 409 | `conflict` | ledger отказал по состоянию данных без подробностей (`FAILED_PRECONDITION`, `ALREADY_EXISTS`, `ABORTED` без деталей) |
| 413 | `payload_too_large` | auth JSON больше 16 КиБ, другой JSON больше 1 МиБ, CSV больше 5 МиБ |
| 415 | `unsupported_media_type` | файл импорта прислан не как `text/csv` |
| 429 | `rate_limited` | лимит входа или регистрации; задержка в `Retry-After` |
| 503 | `server_busy` | заняты слоты Gateway, Auth или Ledger; `Retry-After: 1` |
| 502 | `ledger_unavailable`, `auth_unavailable` | сервис недоступен (остановлен, не резолвится, отказ соединения) |
| 504 | `timeout` | сервис не ответил за `LEDGER_TIMEOUT` / `AUTH_TIMEOUT` |
| 499 | `client_closed_request` | клиент разорвал соединение, не дождавшись ответа |
| 500 | `internal_error` | прочее; `details.request_id` совпадает с заголовком `X-Request-ID` и с логом |

Пути с повторными слешами или отдельными сегментами `.` и `..` получают
`400 invalid_request` без перенаправления. Это правило относится к пути,
не к query-параметрам; percent-encoding обрабатывается штатным HTTP-роутером.

## Как работает бюджет

`POST/GET /api/budgets` является алиасом `/api/budget` с той же авторизацией.
У пользователя один бюджет на категорию; повторный `POST /api/budget` заменяет
лимит и период. Поле `period` задаёт срок действия лимита:

| `period` | Лимит действует | Проверка при добавлении транзакции |
|---|---|---|
| `month` (по умолчанию) | на каждый календарный месяц | траты категории за месяц даты транзакции + новая сумма ≤ лимит |
| `year` | на каждый календарный год | то же за год |
| `none` | один на всё время | все траты категории + новая сумма ≤ лимит |

Транзакция, которая не помещается в лимит, отклоняется с `409 budget_exceeded`
и не сохраняется; трата ровно до лимита проходит. Проверка и запись выполняются
в одной транзакции хранилища, поэтому параллельные запросы не могут совместно
превысить лимит.

В отчёте `summary` лимит показан за весь период отчёта. Для `month` месячный
лимит умножается на число затронутых календарных месяцев, для `year` аналогично
учитываются годы. `percent` содержит долю потраченного от этого лимита,
`remaining` содержит неотрицательный остаток. Категории с бюджетом попадают
в отчёт, даже если трат не было. Этот остаток относится к диапазону отчёта
и не определяет допустимую сумму следующего расхода: её проверяют по периоду
бюджета, содержащему дату расхода.

Необязательное поле `external_id` (например, идентификатор строки таблицы) делает
повторную отправку той же транзакции безопасной: второй запрос получит
`409 already_exists` с `id` уже сохранённой записи.

## CSV-импорт

`POST /api/transactions/import` принимает CSV-файл телом запроса (`Content-Type: text/csv`,
до 5 МБ и 10 000 строк) и сохраняет каждую строку как `POST /api/transactions`.
Продолжайте после [Примеров](#примеры), используя тот же аккаунт. Импорт добавляет
собственные строки и меняет итог отчёта.

```sh
curl -fsS -X POST "$BASE_URL/api/budget" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"category":"кафе","limit":3000,"period":"month"}'
cat > october.csv <<'EOF'
date,category,amount,description,external_id
2026-10-01,еда,1250.50,продукты,bank-001
2026-10-02,кафе,1800,обед с коллегами,bank-002
2026-10-03,кафе,1500,ужин,bank-003
2026-10-03,транспорт,сто,такси,bank-004
2026-10-04,здоровье,700,лекарства,bank-001
EOF
curl -fsS -X POST "$BASE_URL/api/transactions/import" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: text/csv' --data-binary @october.csv
```

При первом импорте ответ содержит две созданные строки и три ошибки.
ID в примере условные:

```json
{"total":5,"created":2,"failed":3,"rows":[
  {"line":2,"status":"created","id":6},
  {"line":3,"status":"created","id":7},
  {"line":4,"status":"failed","error":{"code":"budget_exceeded","message":"budget exceeded for \"кафе\" (2026-10): spent 1800.00 + new 1500.00 > limit 3000.00","details":{"amount":1500.00,"category":"кафе","limit":3000.00,"period":"month","period_label":"2026-10","remaining":1200.00,"spent":1800.00}}},
  {"line":5,"status":"failed","error":{"code":"validation_error","message":"row validation failed","details":{"amount":"must be a number such as 1250.50"}}},
  {"line":6,"status":"failed","error":{"code":"already_exists","message":"transaction with external_id \"bank-001\" already exists as #6","details":{"external_id":"bank-001","id":6}}}]}
```

Повторный импорт того же файла ничего не задваивает: строки с уже сохранённым
`external_id` получают `already_exists` (даже если бюджет к этому моменту исчерпан).
Строка, повторяющая `external_id` более ранней строки файла, получает `already_exists`, если
ранняя сохранилась; если ранняя была отклонена (например, с ошибкой в сумме), сохраняется
поздняя: так исправленную строку можно просто добавить в конец файла.

**Формат.** Первая строка содержит названия колонок в любом порядке и регистре:
`id`, `date`, `category`, `amount`, `description`, `external_id`, как у экспорта.
Колонки `category` и `amount` обязательны. Файл экспорта импортируется обратно как есть (апостроф, который экспорт
ставит перед `=`, `+`, `-`, `@`, при импорте снимается), и колонка `id` не даёт задвоить
данные: строка, чья транзакция у пользователя уже есть, получает `already_exists`; в другом
аккаунте строка без `external_id` сохраняется с `external_id = export-{id}`, поэтому повторный
импорт того же экспорта туда тоже ничего не задвоит. Для намеренного создания новых
операций нужны новые ключи `external_id` и отсутствие прежнего `id`.
Ошибка дубля передаёт фактический `details.external_id`, в том числе производный
`export-{id}`. Если дубль установлен только по принадлежащему пользователю `id`,
ответ содержит `details.id` без предположений о прежнем ключе.

Разделитель `,` или `;` (Excel в русской
локали пишет `;`), BOM пропускается, сумма может быть с запятой, пустые строки пропускаются.

**Валидация в два уровня.** Gateway разбирает файл целиком до обращения к Ledger: битый
CSV, неизвестная колонка (например, опечатка `ammount`), нет обязательной колонки или больше
10 000 строк: `400 invalid_csv` с номером строки, не импортируется ничего. Строка с неверной
суммой, датой или числом полей помечается ошибкой `validation_error`, остальные строки идут
дальше. Ledger проверяет каждую строку по правилам домена и бюджета; ответ: `200` со
счётчиками и итогом по каждой строке: `created` с `id` или `failed` с ошибкой в том же
формате, что и у одиночных запросов (`validation_error`, `budget_exceeded`, `already_exists`,
`canceled`).

Порядок обработки зависимых строк и общий лимит параллелизма описаны
в [архитектуре импорта](architecture.md#параллельный-импорт).

**Остановка и отмена.** Импорт идёт в контексте запроса (`context.WithCancelCause`):

- если клиент отключился, горутины перестают брать новые строки, а сохранённые остаются;
- если подходит срок вызова Ledger (`IMPORT_TIMEOUT`, отсчитывается от начала вызова), Ledger
  останавливается за пару секунд до срока и успевает вернуть итог: сохранённые строки с `id`,
  недошедшие: `canceled`;
- если упало хранилище (например, перезапуск PostgreSQL), импорт останавливается на первой
  такой ошибке: у этой строки `internal_error`, у остальных: `canceled` с номером строки, на
  которой всё остановилось; ошибки в данных строк импорт не останавливают.

**Сроки импорта.** `IMPORT_TIMEOUT` (по умолчанию 1 мин) ограничивает две фазы по отдельности:
чтение тела запроса, для которого Gateway ставит на соединение собственный срок вместо общего
`ReadTimeout` 15 с, и вызов Ledger, который начинается только после полного чтения и разбора файла
и получает свой срок той же длины. Между фазами идёт разбор CSV,
после каждой фазы остаётся запас 20 с на запись ответа. Поэтому один запрос импорта может длиться
дольше одного `IMPORT_TIMEOUT`: точного общего предела нет, в худшем случае это около двух
`IMPORT_TIMEOUT` плюс разбор файла и запись ответа. Медленная загрузка файла, не уложившаяся в срок
чтения, получает `400 invalid_csv`; пока заняты оба слота импорта, третий получает `503 server_busy`.

Повторная отправка файла с `external_id` досохранит недошедшие строки.

## Проверка кеша

Отчёт `GET /api/reports/summary` Ledger кеширует в Redis на `REPORT_CACHE_TTL` (5 минут).
Как отчёт получен, видно в заголовке ответа `X-Cache` и в логе ledger.
Команды ниже добавляют расход на здоровье в 1 рубль. Если до этого выполнены только
[Примеры](#примеры), итог отчёта станет 2251,50. Выполняйте запросы подряд, до истечения TTL:

```sh
# Продолжение раздела «Примеры»: BASE_URL и TOKEN уже заданы.
# Upsert того же бюджета сбрасывает кеш, даже если отчёт запрашивали раньше.
curl -fsS -o /dev/null -X POST "$BASE_URL/api/budget" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"category":"еда","limit":5000,"period":"month"}'
S="$BASE_URL/api/reports/summary?from=2026-10-01&to=2026-10-31"
curl -fsS -D - -o /dev/null -H "Authorization: Bearer $TOKEN" "$S" | grep -i '^X-Cache:'   # MISS
curl -fsS -D - -o /dev/null -H "Authorization: Bearer $TOKEN" "$S" | grep -i '^X-Cache:'   # HIT
curl -fsS -i -X POST "$BASE_URL/api/transactions" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"amount":1,"category":"здоровье","description":"проверка кеша","date":"2026-10-05"}'  # 201
curl -fsS -D - -o /dev/null -H "Authorization: Bearer $TOKEN" "$S" | grep -i '^X-Cache:'   # MISS
docker compose logs ledger | grep "report cache"
```

Если Redis отключён или недоступен, отчёт приходит без `X-Cache`.
[Инвалидация и восстановление кеша](architecture.md#кеш-отчётов).


## Границы пилота

Регистрация разрешена только адресам из списка администратора. Пустой список
закрывает её. Лимит установки: 100 аккаунтов. На аккаунт доступны 10 000
транзакций и 100 различных категорий, считая объединение расходов и бюджетов.
Новый расход или новая категория сверх квоты дают 409 `quota_exceeded`.
Бюджет существующей категории можно изменить; повтор прежнего `external_id`
возвращает `already_exists`. В CSV квота отклоняет свою строку, другие строки
продолжают обрабатываться. Старые записи автоматически не удаляются.

Категории приводятся к нижнему регистру, нормализуются по пробелам и Unicode NFC.
Допустимые канонически эквивалентные написания дают одну категорию и используют один бюджет,
при описанной нормализации. Применяется простое преобразование в нижний регистр,
без полного Unicode case folding и языковых правил. Ограничение в 64 символа применяется к результату
нормализации. Слишком длинные последовательности комбинируемых знаков,
для которых нормализатор вставил бы разделитель, отклоняются с 400
`validation_error`. Правило едино для расходов, бюджетов и фильтров списка/экспорта.
В CSV отклоняется соответствующая строка, допустимые строки продолжают обработку.
Явно введённый разделитель U+034F не удаляется.

Auth считает до пяти неверных паролей на email за минутное окно. Успешный вход
сбрасывает накопленные ошибки; отмена и отказ ресурса их не расходуют.
После пятой ошибки даже правильный пароль получает 429 до конца окна.
Одновременная попытка для того же email также получает 429 с задержкой 1 с.
Общего минутного лимита входов нет. Регистрация сохраняет лимит 10/мин.

Тело входа и регистрации ограничено 16 КиБ и 5 с, остальные JSON-запросы 1 МиБ.
Gateway допускает до четырёх auth-запросов, не более 32 API-запросов суммарно,
один экспорт и два импорта, по одному импорту на пользователя.
Ledger допускает до 32 обычных gRPC-вызовов и удерживает допуск до завершения
обработчика, включая инвалидацию кеша после отмены запроса. Потоковый экспорт
не обращается к кешу и сохраняет отдельный предел Gateway.
Auth допускает три активные операции, среди них одну регистрацию и одно
вычисление Argon2; ожидание вычисления не больше 250 мс.
Перегрузка даёт 503 `server_busy` с `Retry-After: 1`.
Эти пределы не гарантируют доступность входа при непрерывном потоке запросов.
