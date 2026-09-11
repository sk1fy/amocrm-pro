# Этап 8: тестовый виджет, перенос, пилот и выпуск

База: `d7cb8c7` плюс незакоммиченные изменения этапа 8.
Дата: 11 сентября 2026 года. Локальная реализация и синтетические Docker-репетиции,
**не** production deploy, **не** живой amoCRM SDK и **не** целевой удалённый сервер.

**Статус:** локальные артефакты OPS-01, OPS-02, MOD-03, QA-01 и тестера готовы.
QA-02 и фактический pilot enable OPS-03 остаются открытыми до живого аккаунта
и SSH/ролей целевого сервера.

Повторное ревью выявило оставшиеся ошибки алертов и runbooks. Исправления,
новые regression-проверки и актуальный gate: [fixes/README.md](fixes/README.md).
Ниже сохранены исходные результаты этапа. Для полного restore теперь требуется
согласованный набор dump и обязательная сверка accepted/receiver identity.

## Что изменилось

| Задача | Результат |
| --- | --- |
| Тестер | Backend Tester 2 **0.5.2**: source = vendor = generated AMD; `npm run check` 45 PASS. ZIP не пересобирался (тот же SHA, что в финальном аудите 08.09). README 0.4.0→0.5.2. Живая установка не выполнялась |
| OPS-01 | Контракт объёма и SLO из REL-02; dashboard JSON, Prometheus rules, overlay scrape. Новые bounded-метрики coverage/gap, HTTP size, DB size. Grafana/Alertmanager целевой среды нет |
| OPS-02 | `verify-backup-owners.sh --isolated` PASS, RTO_LOCAL_SECONDS=1. Три dump не атомарны. Роли/ключи целевого сервера не проверялись |
| MOD-03 | ADR-0023, overlay сетей, `verify-transfer.sh` PASS (Events на второй PostgreSQL, RTO_TRANSFER_DB_SECONDS=3). Live drain/cutover и RTT до/после не выполнялись |
| QA-01 | Инвентаризация suite этапов 2–7 + два unit-кейса публичных чтений. Полный `activity-ci` — gate координатора |
| QA-02 | Чеклист живого пилота записан; прогон в аккаунте **не** выполнялся |
| OPS-03 | Release candidate: этот каталог. Pilot enable и выпуск на сервер не выполнялись |

Контракты: [ADR-0022](../../adr/0022-activity-slo-and-observability.md),
[ADR-0023](../../adr/0023-product-module-physical-transfer.md).

## Проверки и границы доказательств

| Проверка | Результат | Артефакт |
| --- | --- | --- |
| OPS-01 unit-метрики | PASS (без Docker) | [ops-01.md](ops-01.md) |
| `verify-backup-owners.sh --isolated` | exit 0; проект `amocrm-stage8-backup-test` | [ops-02.md](ops-02.md) |
| `verify-transfer.sh` | exit 0; проект `amocrm-stage8-transfer-test`; live gRPC cutover нет | [mod-03.md](mod-03.md) |
| QA-01 targeted unit | PASS | [qa-01.md](qa-01.md) |
| Tester `npm run check` | 45 PASS / 0 FAIL / 0 SKIP | [tester.md](tester.md) |
| `make ACTIVITY_TEST_PROJECT=amocrm-stage8-test activity-ci` | exit 0; 228 верхнеуровневых Go PASS, 3 helper SKIP, 0 FAIL; `-race`; UI 25 PASS / 0 SKIP | [Go log](activity-go.txt), [UI log](activity-ui.txt) |
| Независимый review | 3 bug + 2 suggestion + 1 nit; координатор исправил | [review.md](review.md) |
| Живой виджет / целевой сервер | не выполнялось | [qa-02.md](qa-02.md), [ops-03.md](ops-03.md) |

Compose-проекты `amocrm-stage8-*-test` после скриптов сняты. Runtime-проект
`amocrm-activity` не менялся.

## Что не выполнялось

- Живой amoCRM, OAuth SDK, установка ZIP в аккаунт, JWT/CORS/CSP браузера
- SSH, роли backup, KMS, reverse proxy, Grafana/Alertmanager целевого хоста
- Физический multi-host, live drain Activity затем CRM Events
- Горизонтальные несколько экземпляров одного владельца
- Применение owner-миграций 000002–000008 / Core 000012+ к runtime/production

## Команды воспроизведения

```sh
make activity-backup-verify
make activity-transfer-verify
# после свободного Docker:
make ACTIVITY_TEST_PROJECT=amocrm-stage8-test activity-ci
make TEST_COMPOSE_PROJECT=amocrm-stage8-integration-test integration-test
make openapi-check
```

Тестер (вне этого git):

```sh
cd /Users/nikpeskov/Projects/sub-projects/amocrm-pro-service-2
npm run check
```
