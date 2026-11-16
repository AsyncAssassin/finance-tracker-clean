# Тестирование

[README](../README.md#тесты) · [GitHub Actions](https://github.com/AsyncAssassin/finance-tracker-clean/actions/workflows/ci.yml)

## Команды

Команды выполняются из корня репозитория. Для Go нужен toolchain из `go.mod`,
для Apps Script Node 22+, для интеграций Docker Compose.

| Команда | Что выполняет |
| --- | --- |
| `make test` | `go test ./...`; тесты настоящих PostgreSQL/Redis включаются переменными окружения |
| `make race` | Тот же Go-прогон с детектором гонок |
| `make test-integration` | Реальные PostgreSQL и Redis, `-race -count=1`: репозитории Auth/Ledger, миграции, кеш |
| `make sheets-test` | Тесты скрипта с поддельными Google-сервисами; live-тест без адреса API пропускается |
| `make e2e` | HTTP-сценарии на запущенном стеке и Node live-тест скрипта; нужны отдельный стек и Node |
| `make cover` | `-race`, atomic-покрытие всех корневых пакетов без `.pb.go`; внешние интеграции зависят от окружения |
| `make cover-all` | Полный профиль `cover.out` с настоящими PostgreSQL и Redis |
| `make check` | gofmt, vet, staticcheck, tidy; при наличии protoc также проверка генерации proto |
| `make vuln`, `make vuln-check` | govulncheck без исключений; обе цели выполняют одну проверку |

[Makefile](../Makefile) описывает локальные цели.
`make check` не запускает весь CI: секреты, модули Caddy/gosu, сборки, HTTPS и e2e
проверяются отдельными задачами workflow.

## Изоляция данных

`make test-integration` и `make cover-all` запускают проект `finance-tracker-clean-dev`
с отдельной базой `finance_test` и Redis DB 15. Тесты выполняют `TRUNCATE` и
`FLUSHDB`, поэтому `TEST_DATABASE_URL` и `TEST_REDIS_ADDR` должны указывать
исключительно на тестовые данные. Учётные данные передаются через `PGUSER`
и `PGPASSWORD`; порты можно задать через `POSTGRES_PORT` и `REDIS_PORT`.
`TEST_REDIS_ADDR` содержит только `host:port`; тестовый клиент всегда выбирает DB 15.

Основной Compose использует Redis ACL без права `FLUSHDB`. Для интеграционных
тестов кеша нужен отдельный Redis режима разработки, а HTTP e2e проверяет
рабочую ACL-конфигурацию без очистки кеша через администратора.

Обычный `go test ./...` без этих адресов пропускает внешние интеграции.
Тесты с памятью, поддельными клиентами и miniredis продолжают выполняться.

Для HTTP e2e используйте новый Compose-проект и свободный loopback-порт.
Пример запуска в отдельной оболочке; он удаляет только свой временный стек:

```sh
(
  set -eu
  FT_TEST_PROJECT="finance-e2e-$(date +%s)"
  FT_TEST_ENV=$(mktemp)
  chmod 600 "$FT_TEST_ENV"
  printf 'JWT_SECRET=%s\n' "$(openssl rand -base64 32)" > "$FT_TEST_ENV"
  for name in REDIS_PASSWORD POSTGRES_PASSWORD AUTH_DB_PASSWORD LEDGER_DB_PASSWORD AUTH_MIGRATION_PASSWORD LEDGER_MIGRATION_PASSWORD; do
    printf '%s=%s\n' "$name" "$(openssl rand -hex 32)" >> "$FT_TEST_ENV"
  done
  export E2E_RUN_ID="$(openssl rand -hex 6)"
  export E2E_SHEETS_EMAIL="sheet-$E2E_RUN_ID@example.com"
  FT_TEST_EMAILS=""
  for name in anna boris input-concurrency isolation-a isolation-b sheet; do
    FT_TEST_EMAILS="$FT_TEST_EMAILS$name-$E2E_RUN_ID@example.com,"
  done
  printf 'AUTH_REGISTRATION_EMAILS=%s\n' "$FT_TEST_EMAILS" >> "$FT_TEST_ENV"
  ft_compose() {
    COMPOSE_PROFILES= HTTP_PORT=127.0.0.1:18080 docker compose \
      --env-file "$FT_TEST_ENV" -p "$FT_TEST_PROJECT" -f compose.yaml "$@"
  }
  trap 'ft_compose down -v; rm -f "$FT_TEST_ENV"' EXIT
  ft_compose up -d --build --wait
  E2E_BASE_URL=http://localhost:18080 E2E_REQUIRE_CACHE=1 make e2e
)
```

Убедитесь, что Node установлен: без него `make e2e` сообщает о пропуске
live-теста скрипта. Не направляйте этот набор на рабочие данные: он создаёт аккаунты
и операции. Некоторые конкурентные и некорректные запросы выполняются только
для loopback-адреса. Повторный прогон на том же Auth в течение одного минутного
окна может получить 429; дождитесь `Retry-After` или используйте свежий тестовый стек.

## Уровни проверок

| Уровень | Проверяется | Граница |
| --- | --- | --- |
| Домен и сценарии | Деньги, даты, month/year/none, лимиты, отчёты, импорт, отмена, повторные ключи | Хранилище в памяти не проверяет SQL |
| HTTP-обработчики | Валидация, JWT, коды и JSON ошибок, request ID, паники, лимиты запросов | Поддельные клиенты не заменяют работающие сервисы |
| gRPC | Настоящие клиент и сервер через bufconn, статусы, deadlines, потоковый CSV | Соединение в памяти не проверяет сеть VPS |
| PostgreSQL | Общий контракт хранилищ, миграции, upsert, блокировки, регистрация и границы сумм | Только отдельная тестовая БД |
| Redis | Отчёты и бюджеты: MISS/HIT, пустой список, TTL, изоляция, инвалидация и повреждённые записи с miniredis и настоящим Redis | TTL бюджетов проверяется и по реальному времени, и ускоренно в miniredis |
| HTTP e2e | Три сервиса, PostgreSQL, Redis, JWT, CSV, изоляция, конкурентные лимиты и дубли | Собранные сервисы и e2e-команда не инструментированы `-race` |
| Локальный HTTPS | Выпуск сертификата через ACME, доверенный TLS, HTTP/1.1 и HTTP/2, проксирование и сохранение сертификата | Локальный CA не проверяет публичный DNS, Let's Encrypt и сеть VPS |
| Apps Script в Node | Меню, ввод, безопасный текст, адрес сервера, сессии; live-тест с реальным API | Google OAuth, права Диска и браузер не проверяются |
| Ручной Google-сценарий | Формы, бюджеты, отправка, ошибки и отчёт в настоящей таблице | Проверка одного Google-аккаунта не подтверждает доступ другого |
| VPS и восстановление | Установка, внешнее HTTPS, сохранность данных, дампа и сертификата | Штатный перезапуск не имитирует отказ питания или диска |

Тесты конкуренции проверяют ограниченные сценарии, например 24 запроса к одному
бюджету или ключу. Это не тест предельной нагрузки. Fuzz-тесты содержат стартовые
наборы входов; обычный `go test` не запускает продолжительную fuzz-кампанию.

Кеш бюджетов дополнительно проверяется на отсутствие чтения БД при HIT,
инвалидацию после upsert, отмену запроса, сбои чтения/записи Redis и ошибку БД.
Тесты гонок воспроизводят сохранение старого результата одновременно с изменением
бюджета, неуспешную инвалидацию и её ожидание активной записи кеша. PostgreSQL-тест
проверяет сброс кеша и при потерянном подтверждении COMMIT.
HTTP e2e проверяет пустой список, повторное чтение, создание и изменение бюджета;
демонстрация попаданий по логам описана в [API](api.md#проверка-кеша).

## Покрытие и CI

Сохранённый [cover.out](../cover.out) получен 15.10.2026 с PostgreSQL, Redis и
`-race`. Это снимок отдельного прогона; он не обновляется от изменения исходников.
Для выбранной версии используйте артефакт её запуска CI или `make cover-all`.

`cover.out` измеряет выполненные инструкции корневого Go-модуля. Сгенерированный
proto-код исключается. Точки входа сервисов имеют непокрытые участки; запуск
контейнеров проверяется отдельно в e2e и не увеличивает этот профиль.
Покрытие не измеряет долю выполненных требований и не доказывает отсутствие ошибок.

Проверка сохранённого профиля без нового прогона:

```sh
go tool cover -func=cover.out
```

[GitHub Actions](../.github/workflows/ci.yml) запускается для push в main,
тегов `v*` и pull request:

| Задача | Содержание |
| --- | --- |
| `lint` | gofmt, vet, staticcheck, tidy основного модуля; vet/tidy модулей Caddy и gosu; запрет импорта go-jose/v3 в Caddy |
| `test` | Все корневые Go-тесты с `-race`, реальные PostgreSQL и Redis, артефакт покрытия |
| `vulnerabilities` | govulncheck приложения, Caddy и gosu без исключений |
| `secrets` | Gitleaks по всей доступной истории без исключений |
| `sheets` | Node-тесты с поддельными Google-сервисами |
| `https` | Тесты совместимости TLS/PKI/ACME/JOSE с `-race`, сборка Caddy, проверка Compose и Caddyfile без публичного выпуска сертификата |
| `e2e` | Сборка приложения, отдельный Compose, HTTP e2e, Node live-тест и границы четырёх ролей БД |

При закрытой регистрации окружение автоматического e2e должно
получать те же `E2E_RUN_ID`, `E2E_SHEETS_EMAIL`, список разрешённых адресов и
пароль Redis, что в примере выше. Workflow создаёт их перед запуском стека
и передаёт идентификаторы обоим сценариям через `GITHUB_ENV`.

В задаче `test` CI и в локальном Makefile используется БД `finance_test`;
HTTP e2e и проверка ролей используют `finance` своего отдельного стека.
В CI тест ролей собирается в отдельный бинарник и запускается в существующей
сети Compose через образ мигратора. Переключение работающего стека на dev-сети
и публикация порта PostgreSQL для этого не нужны.
Успех CI подтверждает его задачи,
а публичный сертификат, VPS и настоящий интерфейс Google проверяются отдельно.

## Проверка ролей БД

На отдельном Compose-стенде `TestDatabaseRoles` проверяет реальные подключения
всех четырёх ролей и SQLSTATE `42501` при запрещённых операциях. Понадобятся
четыре пароля из окружения стенда и адрес без учётных данных:

```sh
TEST_ROLE_DATABASE_URL='postgres://localhost:5432/finance?sslmode=disable' \
  go test -tags e2e -run '^TestDatabaseRoles$' -count=1 -v ./e2e/
```

Пароли передаются в `AUTH_DB_PASSWORD`, `LEDGER_DB_PASSWORD`,
`AUTH_MIGRATION_PASSWORD`, `LEDGER_MIGRATION_PASSWORD`. Стенд поднимается
с `compose.dev.yaml`, только на loopback. Обычные HTTP e2e подтверждают
разрешённые операции через рабочие роли. Тесты репозиториев с `TRUNCATE`
по-прежнему используют администратора отдельной базы `finance_test`.

Модуль gosu проверяется отдельно:

```sh
cd deploy/gosu
GOOS=linux go vet github.com/tianon/gosu
go mod tidy -diff
go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 github.com/tianon/gosu
```

## Проверка Caddy и HTTPS

Модуль Caddy проверяется независимо от основного приложения. Из корня репозитория:

```sh
(
  set -eu
  cd deploy/caddy
  go vet -tags nobadger,nomysql,nopgx ./...
  go mod tidy -diff
  caddy_deps=$(go list -deps -tags nobadger,nomysql,nopgx ./...)
  if printf '%s\n' "$caddy_deps" | grep -E '^github.com/go-jose/go-jose/v3(/|$)' > /dev/null; then
    echo 'Caddy imports the unpatched go-jose/v3'
    exit 1
  fi
  go test -race -count=1 -tags nobadger,nomysql,nopgx \
    github.com/caddyserver/caddy/v2/modules/caddytls \
    github.com/caddyserver/caddy/v2/modules/caddypki \
    github.com/caddyserver/caddy/v2/modules/caddypki/acmeserver \
    github.com/smallstep/certificates/authority/provisioner \
    go.step.sm/crypto/jose \
    github.com/go-jose/go-jose/v4
  go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 -tags nobadger,nomysql,nopgx ./...
)
docker build -f deploy/caddy.Dockerfile -t finance-caddy-check:local .
trivy image --severity HIGH,CRITICAL --exit-code 1 finance-caddy-check:local
```

Тесты upstream-пакетов проверяют совместимость закреплённых зависимостей;
основание выбора версий находится в [SECURITY.md](../SECURITY.md#зависимости-https-образа).
Они не заменяют запросы к собранному образу. При обновлении Caddy дополнительно
проверьте его на отдельном Compose-стенде с тестовыми данными:

1. Поднимите локальный ACME-сервер, например
   [Pebble](https://github.com/letsencrypt/pebble), в отдельной сети. В копии Caddyfile
   укажите его directory и корневой сертификат через `trusted_roots`.
   Включите настоящую проверку HTTP-01, направив тестовое имя на Caddy;
   не используйте режим автоматического одобрения challenge.
2. Дождитесь выдачи сертификата. Передавайте корень выпускающего CA только
   тестовому клиенту через `--cacert` или `SSL_CERT_FILE`, не устанавливая его
   в системное хранилище. Запрос без этого корня должен завершаться ошибкой доверия.
3. Проверьте `/ping` через TLS 1.2/1.3 и HTTP/1.1/2, редирект HTTP с сохранением
   пути и query, `/docs`, OpenAPI и 401 без JWT. Не используйте `curl -k`.
4. Запустите `TestScenario`, `TestUserIsolationAcrossAPIAndCache` и
   `TestLoginRateLimit` из `e2e` через HTTPS с `E2E_REQUIRE_CACHE=1`.
   Они проверяют вход, бюджеты, расходы, CSV, кеш и границы доступа.
5. Остановите только тестовый ACME-сервер и перезапустите Caddy с прежними томами.
   Доверенный HTTPS должен сохраниться без нового выпуска; сравните отпечаток
   сертификата до и после перезапуска. Удалите только ресурсы своего тестового стека.

CI выполняет тесты пакетов и проверку конфигурации. Выпуск через локальный ACME,
проверка сохранения сертификата и публичный HTTPS из
[инструкции эксплуатации](operations.md#публичный-https) выполняются отдельно.

## Проверка зависимостей и образов

`make vuln` и `make vuln-check` запускают одну проверку основного Go-модуля
без исключений. CI отдельно проверяет модули Caddy и gosu; ненулевой код
завершения останавливает соответствующую задачу. Gitleaks проверяет всю
доступную историю, включая удалённые из текущего дерева файлы. Сканер секретов
не заменяет просмотр diff, журналов, артефактов и изображений перед публикацией.

Перед установкой отдельно сканируйте готовые контейнерные образы. Для результата
сохраните дату, версию сканера и его базы, платформу, digest образа и связь
с исходниками. Изменение кода или Dockerfile требует проверки новых образов;
чистый исходный анализ не подтверждает отсутствие проблем в базовом образе.
Задача `vulnerabilities` в CI выполняет govulncheck, сканирование всех образов
в этот workflow не входит.

Модульная находка и достижимый уязвимый код требуют разной оценки. Сопоставляйте
advisory с импортами, функциями бинарника и условиями запуска. Сохраняйте сырой
результат вместе с обоснованием применимости; не скрывайте находку только ради
успешной проверки. Результат для одной платформы и конфигурации не переносится
автоматически на установленный сервер.
