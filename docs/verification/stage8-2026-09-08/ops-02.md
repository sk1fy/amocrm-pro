# OPS-02. Backup/restore всех owner DB и обновление

Дата: 2026-09-11. Локальная Docker-репетиция, **не** целевой сервер и **не**
runtime-проект `amocrm-activity`. Совместное evidence с
[mod-03.md](mod-03.md): restore данных — здесь; размещение/switchover — там.

`deploy/activity/verify-backup.sh` не менялся (synthetic CRM Events only).

## Что сделано

- Скрипт всех владельцев: [`deploy/activity/verify-backup-owners.sh`](../../../deploy/activity/verify-backup-owners.sh).
- Runbook: [`activity-backup-restore.md`](../../runbooks/activity-backup-restore.md).
- Совместимость миграций 2–7: [`migrations-compat.md`](migrations-compat.md).
- Процессные отказы не переписывались: `TestComponentOSProcessFaults`,
  `TestComponentProcessesAndModeSwitch`, `TestStandaloneRejectsForeignConfiguration`.
- Добавлен focused config-тест статических адресов и чужого DSN:
  `TestLoadPreservesStaticAddressesAndRejectsForeignDSN`. `make test` /
  `make activity-ci` в этой сессии не запускались.

## Команда и результат

```sh
sh deploy/activity/verify-backup-owners.sh --isolated
```

Compose-проект: `amocrm-stage8-backup-test` (postgres:17-alpine, роли как в
`init-db.sh`). Exit **0**. Runtime `amocrm-activity` остался running(5).

Вывод (повтор после правки NOTICE, тот же сценарий):

```
PASS: Core owner dump/restore preserved outbox, receipts, pilots, grants and credential ciphertext presence (values not printed)
PASS: Activity owner dump/restore preserved settings and command receipts
PASS: CRM Events owner dump/restore preserved events, coverage, source progress, consumer, jobs, enrichment, tombstones and command/operation identity
PASS: repeated command IDs stay unique after restore; expired tombstone still rejects replay
PASS: runtime roles keep DML on own restored DB, no DDL, no CONNECT to foreign owner DBs
NOTE: three dumps are not one atomic snapshot; Core outbox vs Events history may diverge by dump skew
RESTORED: core_owners_64_restored_test activity_owners_64_restored_test events_owners_64_restored_test
RTO_LOCAL_SECONDS=1
```

Первый прогон того же скрипта до тишины NOTICE: `core_owners_65_*`, тоже
`RTO_LOCAL_SECONDS=1`, exit 0. Имена БД только `*_test`. Кластер с
`amocrm_core` / `amocrm_activity` / `amocrm_events` скрипт отвергает.

Что восстановлено (синтетика):

| Владелец | Проверено после restore |
| --- | --- |
| Core | outbox `accepted`, receipt, pilot, grant `activity`, ciphertext OAuth и webhook destination **присутствуют**, `key_version=1`; значения не печатались |
| Activity | settings 2/7 дней, command receipt с тем же UUID |
| CRM Events | событие, coverage, source window, consumer enabled, job, inbox=operation id, enrichment link, tombstone |

Повтор INSERT того же command ID отклонён уникальностью; tombstone `expired-command`
по-прежнему отвергает replay. `*_runtime` читает свою restored-БД, не создаёт
таблицы, не коннектится к чужой.

## RPO / RTO

Операционные цели, не production SLO.

| | Значение |
| --- | --- |
| RPO владельца | последний успешный dump этой БД |
| Межвладельческий RPO | не определён: три dump не атомарны |
| RTO dump/restore трёх БД (локально) | **1 с** на синтетике (`RTO_LOCAL_SECONDS`) |
| RTO с процессами | не измерялся; оценка: restore + старт + `/ready` + lease 30 с при kill |
| Перенос Events на второй инстанс | **3 с** (`RTO_TRANSFER_DB_SECONDS` в MOD-03) |

Dump-skew: не dual-write; fencing writers; restore Core → Events → Activity;
повтор command ID, не новый. Keyring Core в dump Activity/Events не входит:
без того же `ENCRYPTION_KEYS` ciphertext не расшифровать. Production key
access не проверялся.

## Readiness, shutdown, Core без продукта

Не перезапускались. Существующие доказательства:

- Core `/ready` и widget ping/lead-status при остановленном Activity:
  `TestComponentProcessesAndModeSwitch` (durable 202 / outbox, доставка после
  возврата получателя).
- Gateway SIGKILL, lease fencing, crash/resume страницы, replay после потери
  RPC: `TestComponentOSProcessFaults`.
- HTTP panel/карточка как `unavailable`, не пустая история: MOD-02 /
  `TestStage6ActivityUnavailableIsExplicitNotEmptyHistory`.
- Compose `stop_grace_period: 20s`; RPC `GracefulStop` 5 с в `runtime.go`.

## Миграции 2–7

Таблица: [migrations-compat.md](migrations-compat.md). На runtime не
применялись. Down не возвращает данные. Порядок: owner up, затем новый бинарь.

## Что осталось открытым

- Роли и backup-агент **целевого сервера**.
- Реальный доступ к production keyring / secret store (ADR-0021, KMS не выбран).
- Restore живых объёмов и измеренный production RTO.
- Повтор process-тестов в этой сессии (`make activity-ci` запрещён заданием).
- Живой cutover процессов — [mod-03.md](mod-03.md).
