# Проверка и исправления внешнего аудита AUD-01–AUD-05

Дата: 2026-09-19. Базы: Core `59cb3f0`, Admin `0eac389`.
Результаты относятся к изменениям поверх этих коммитов и получены
локально до публикации веток.

## Подтверждение и изменения

Все пять замечаний подтвердились в актуальных исходниках.

| ID | Репозиторий | Исправление |
| --- | --- | --- |
| AUD-01 | Admin | Жёсткий предел 10 000 счётчиков, ключи фиксированного размера, отказ по IP без добавления email, очистка один раз за минуту |
| AUD-02 | Admin | PATCH читает сотрудника под `FOR UPDATE`, сохраняет непереданные поля и строит diff по защищённому чтению |
| AUD-03 | Core | gRPC v1.83.2 и согласованные транзитивные зависимости; TLS/RPC permissions не менялись |
| AUD-04 | Admin | Сотрудник, отзыв сессий и аудит фиксируются одной транзакцией; также собственные сессии и logout |
| AUD-05 | Admin | Credentials дают одинаковый 401; инфраструктурная ошибка — безопасный 500 с request_id; UI различает 401, 429 и недоступность |

В оба Makefile и CI добавлена проверка `vulncheck` с govulncheck v1.1.4.
Новых миграций и изменений Core admin API нет.

## Уточнение зависимостей

Рекомендованная аудитом gRPC v1.83.1 закрывает
[GO-2026-6348](https://pkg.go.dev/vuln/GO-2026-6348) и
[GO-2026-6061](https://pkg.go.dev/vuln/GO-2026-6061), но текущий scanner
также сообщает [GO-2026-6443](https://pkg.go.dev/vuln/GO-2026-6443).
Итоговая версия — v1.83.2. Эксплуатация не воспроизводилась; xDS не
используется, хотя анализатор отмечает общий транспортный путь.

Дополнительно найден достижимый
[GO-2026-5970](https://pkg.go.dev/vuln/GO-2026-5970) через x/text/pgx.
Core теперь использует x/text v0.41.0, Admin — v0.39.0.

Финальный govulncheck выполнен в Docker, Go 1.25.14, linux/arm64:

- Core: `No vulnerabilities found`, exit 0.
- Admin: 0 достижимых уязвимостей и 0 в импортируемых пакетах, exit 0.
  На уровне модулей остаётся 21 запись: 20 в SSH/OpenPGP частях x/crypto,
  одна в x/sys/windows. Admin импортирует x/crypto/argon2; указанные
  уязвимые пакеты не импортируются. Suppression/исключения не добавлялись.

## Проверки

Admin: PostgreSQL integration прошёл полностью на финальных зависимостях.
Подтверждены управляемая гонка rename/disable и rename/demotion, шесть
сценариев отката при ошибках аудита/отзыва сессий, различие credentials,
ошибки INSERT сессии и недоступного pool. Проверки docs/scripts прошли.

| Проверка | Результат |
| --- | --- |
| Core: make test | PASS: gofmt, vet, race, включая RPC/mTLS |
| Core: make openapi-check | PASS |
| Core: make integration-test | PASS: миграции и PostgreSQL |
| Core: make activity-ci | PASS: межсервисные, process/fault, synthetic restore, Prometheus и 25 UI-тестов |
| Core: make build | PASS: api, worker, migrate, integrations |
| Admin: make lint | PASS: Go, TypeScript, ESLint, Prettier |
| Admin: make test | PASS: Go race, Vitest 27 файлов / 93 теста |
| Admin: make integration-test | PASS |
| Admin: make e2e | PASS: 26 Playwright-сценариев |
| Admin: make docs-check scripts-test | PASS: ссылки и 11 проверок скриптов |
| Govulncheck обоих модулей | exit 0; детали недостижимых находок выше |

В бинарниках из фактически пересобранных `amocrm-api:local` и
`amocrm-worker:local` через `go version -m` подтверждены Go 1.25.14,
gRPC v1.83.2 и x/text v0.41.0. Проверки локальные, Docker linux/arm64;
на момент этой локальной проверки remote CI ещё не запускался.

Первый прогон остановился из-за заполненного диска Docker; удалены только
неиспользуемые образы без тегов старше суток. При конкурирующих сборках
старые тесты с короткими таймаутами упали; последовательный повтор
PostgreSQL suite прошёл без изменения таймаутов и ожиданий.
Для финальных сборок использован официальный Buildx v0.37.1 с проверенным
SHA-256 во временном каталоге и отдельной конфигурацией Docker.
Постоянные настройки Docker не менялись. Тестовые Compose-стеки удалены
штатным cleanup, рабочий pilot не перезапускался.

## Границы

Совместный E2E реальных Admin + Core, production proxy, история секретов,
проверка развёрнутых образов и настоящие резервные копии не проверялись.
Fixture Playwright и Activity/mTLS suite не заменяют эту приёмку.
Исходный риск распределённых команд Activity из аудита 2026-09-14
этими исправлениями не закрывается.

См. [ADR-0030](../adr/0030-go-vulnerability-check.md) и
[runbook](../runbooks/go-vulnerabilities.md).
