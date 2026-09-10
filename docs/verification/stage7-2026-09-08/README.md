# Этап 7: общий backend, оставшиеся доработки

База: `dac12af` плюс незакоммиченный этап 6 и изменения этапа 7.
Дата: 10 сентября 2026 года. Локальная реализация и Docker-gate, не production deploy.

**После повторного аудита:** исправлены четыре дефекта — владение вебхуками,
RESTRICT-ссылки cleanup, повтор одноразового OAuth refresh и expired access
при uninstall. Актуальные изменения/проверки: [fixes/README.md](fixes/README.md).
Новая дополнительная миграция Core: `000015_webhook_ownership`.
Повторные gates: Core integration 220 PASS / 2 performance SKIP;
Activity 215 Go PASS / 3 helper SKIP / UI 25 PASS; `make test` PASS.

Исторический результат первого ревью: CORE-01..05 pass, CORE-06 conditional (целевой сервер открыт).
Предложения ревью закрыты координатором до gate. См. [review.md](review.md).

Новые Core-миграции: `000012_oauth_refresh_lease`, `000014_core_redelivery_horizon`
(`000013` свободен). CRM Events: `000008_command_identity_horizon` поверх
`000007` этапа 6. На runtime/production не применялись.

## Что изменилось

| Задача | Результат |
| --- | --- |
| CORE-01 | Claim/lease OAuth refresh: SQL-транзакция не удерживается на время amoCRM HTTP. ADR-0017, `000012` |
| CORE-02 | `disable-installation` / `enable-installation` / `revoke` / `uninstall`; DeleteWebhook + stale dest; история не удаляется. ADR-0018 |
| CORE-03 | OAuth ingress limiter; JSON envelope + `reauth_required` 403; OpenAPI. Инвентаризация [core-03-errors.md](core-03-errors.md) |
| CORE-04 | Межрепличный лимитер не внедрён (один Gateway). Enrichment в существующем бюджете 7/50. ADR-0019 |
| CORE-05 | Горизонт redelivery 7 суток; GC Core + CRM Events inbox/operations + tombstones. `activity-control list/inspect`. ADR-0016 |
| CORE-06 | Fail-fast config, [secrets-rotation.md](../../runbooks/secrets-rotation.md), ADR-0021 (KMS отложен). Целевой сервер не проверялся |
| CORE-07 | Существующие lead-status/webhook/widget/parity tests в том же `activity-ci`; отдельных копий сценариев нет |

Контракты: [ADR-0016](../../adr/0016-technical-history-retention.md),
[ADR-0017](../../adr/0017-oauth-refresh-lease.md),
[ADR-0018](../../adr/0018-installation-lifecycle.md),
[ADR-0019](../../adr/0019-gateway-outbound-budget.md),
[ADR-0021](../../adr/0021-production-secrets.md).

## Проверки и границы доказательств

| Проверка | Результат | Артефакт |
| --- | --- | --- |
| Unit (без PostgreSQL) | PASS: oauthlimit, config, cryptox, amocrm budget, CLI, activitybridge, webhook, maintenance, oauth | координатор |
| `gofmt` / `go vet` targeted | clean | координатор |
| Независимый review | CORE-01..05 pass; CORE-06 conditional; 0 bugs; предложения закрыты | [review.md](review.md) |
| `make openapi-check` | exit 0 | координатор |
| `make ACTIVITY_TEST_PROJECT=amocrm-stage7-test activity-ci` | exit 0; 215 верхнеуровневых Go PASS, 3 штатных helper SKIP; `-race`; UI 25 PASS / 0 SKIP | [Go log](activity-go.txt), [UI log](activity-ui.txt), [checks.json](checks.json) |
| `make TEST_COMPOSE_PROJECT=amocrm-stage7-integration-test integration-test` | exit 0 после исправления счётчика в CORE-01 тесте (sequence вместо table UPDATE в той же TX). Lead-status, webhook, widget routes, OAuth lease, uninstall PASS. 2 штатных SKIP performance | [integration-test.txt](integration-test.txt) |

Compose-проект `amocrm-stage7-test`, БД `*_test`. Runtime-проект `amocrm-activity` не менялся.

## Что не выполнялось

- Живой amoCRM, OAuth SDK, production migrate `000012`/`000014`/`000008`
- SSH/целевые роли, reverse proxy, KMS
- Remote amoCRM OAuth revoke API
- Межрепличный Gateway limiter (нет второй реплики)
- Purge продуктовой истории при uninstall (отдельный процесс)

Первый `integration-test` упал на `TestTokenProviderPersistsPendingRotationWithoutSecondRefresh`: счётчик неудачных persist жил в той же транзакции, что и откатываемый UPDATE. Заменён на sequence. Повторный прогон — PASS.

## Команды воспроизведения

```sh
make ACTIVITY_TEST_PROJECT=amocrm-stage7-test activity-ci
make TEST_COMPOSE_PROJECT=amocrm-stage7-integration-test integration-test
make openapi-check
```

Перед сервером: применить Core `000012`, `000014`, `000015` согласованно с бинарём API/worker;
CRM Events `000007` затем `000008` согласованно с бинарём CRM Events. Down не
возвращает удалённые строки и ротированные токены.
