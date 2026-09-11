# Backup и restore владельцев Activity

Synthetic rehearsal: [`deploy/activity/verify-backup-owners.sh`](../../deploy/activity/verify-backup-owners.sh).
CRM Events-only скрипт [`verify-backup.sh`](../../deploy/activity/verify-backup.sh)
сохраняется и не заменяется. Перенос размещения —
[activity-module-transfer.md](activity-module-transfer.md). Down-миграции —
[migrate-down.md](migrate-down.md). Ключи —
[secrets-rotation.md](secrets-rotation.md).

Это не проверка целевого сервера и не инструкция к runtime-БД проекта
`amocrm-activity` (`amocrm_core` / `amocrm_activity` / `amocrm_events`).

## Что принадлежит кому

| Владелец | Логическая БД | Что сохранять | Что нельзя считать общим снимком |
| --- | --- | --- | --- |
| Core | Core | installations, encrypted credentials, receipts, outbox, pilots, grants | Продуктовая история CRM |
| Activity | Activity | settings, command receipts | События и coverage |
| CRM Events | Events | events, coverage, source/consumer, inbox/operations/jobs, enrichment, tombstones | Core outbox и OAuth |

Три `pg_dump` при работающих writers дают три разных состояния. Outbox
`accepted` в новом Core dump может не иметь inbox в старом Events dump.
Поддерживаемый путь восстановления здесь — **согласованный набор трёх dump,
снятых при остановленных writers**, с обязательной сверкой перед запуском.
Произвольные независимые live dump таким набором не считаются.

## Получение согласованного набора

1. Закрыть приём новых запросов на запись и операторских команд. Остановить
   Core API, worker (delivery, OAuth refresh, cleanup), Activity и CRM Events
   (scheduler и enrichment), а также внешние задания записи/миграции.
2. Дождаться завершения процессов и SQL-транзакций, исключить автоперезапуск
   старых writers. Graceful shutdown/lease сами по себе не заменяют fencing.
3. Пока все writers остановлены, снять три `pg_dump --format=custom` ролями
   соответствующих владельцев. Ошибка любого dump делает набор неполным.
4. Сохранить общий ID набора, время остановки writers, версии образов и
   owner-миграций, SHA-256 каждого dump и версии ключей Core. Сами ключи
   остаются в защищённом keyring; без них нельзя принять восстановление Core.
5. Возобновить исходный стек только после успешного завершения всех dump.

Наличие `pending_delivery` в таком наборе допустимо: после restore штатный
worker доставит команду либо вернёт `delivery_expired` по календарному горизонту.

## RPO / RTO (операционные цели)

Значения ниже — цели процедуры, не измеренный production SLO.

| Цель | Значение | Основание |
| --- | --- | --- |
| RPO восстановления стека | возраст последнего **полного согласованного набора** | отдельных WAL-shipping в репетиции нет |
| Межвладельческая точка восстановления | состояние на момент остановки всех writers выбранного набора | последовательные dump при отсутствии записей сохраняют согласованное состояние |
| RTO локальной репетиции | время restore трёх БД + проверки ролей; см. `RTO_LOCAL_SECONDS` скрипта | isolated postgres:17, без старта процессов |
| RTO с процессами | restore + migrate checksum + старт + `/ready` + повтор command ID | Compose `stop_grace_period` 20 с, RPC graceful 5 с, lease 30 с |

Production RPO/RTO назначаются после доступа к целевому хосту и его backup
агенту. Не подменять их локальными секундами.

## Роли и доступ к ключам

Как в [`init-db.sh`](../../deploy/activity/init-db.sh):

- `*_owner` — DDL и dump/restore своей БД, `NOSUPERUSER`;
- `*_runtime` — CONNECT + DML своей БД, без DDL и без CONNECT к соседним;
- encryption keyring (`ENCRYPTION_KEYS`) живёт в процессе Core, не в dump
  Activity/Events. Restore Core без того же keyring оставляет ciphertext
  нерасшифруемым. Скрипт проверяет **наличие** ciphertext и `key_version`,
  не печатает секреты и не расшифровывает их.

Backup-роль production должна уметь dump без superuser. Development-пароли
Compose в реальных данных не используются.

## Порядок restore

1. Остановить все writers восстанавливаемого стека (см. согласованный набор выше).
2. Restore **в новые** БД из одного полного набора, не поверх живых.
3. Накатить только недостающие owner-миграции, если dump со старой схемой.
4. Сверить events/coverage/progress, settings, outbox и доступ к keyring;
   выполнить preflight ниже. У каждой команды один получатель: Activity
   receipt **или** Events inbox/operation, соответствующие её Core target.
5. Только после PASS запустить процессы с новыми DSN и проверить `/ready`.
   Core origin сохраняется; до приёмки публичная запись остаётся закрытой.
6. Повторить недавнюю команду с тем же idempotency key и свежей авторизацией:
   ожидается прежний command/operation ID. Проверить настройки и прогресс,
   затем открыть запись. Это проверка дедупликации, а не ремонт `accepted`.

Порядок владельцев при полном аварии-restore:

1. Core (credentials, admission, outbox).
2. CRM Events (история и inbox — иначе Core начнёт redelivery в пустоту либо
   создаст конфликт с отсутствующим tombstone).
3. Activity (настройки; чтение истории идёт в Events).

Не включать два scheduler CRM Events на старом и новом инстансе одновременно.

## Preflight и отказ при dump-skew

Оператор предоставляет `CORE_RESTORE_DSN`, `ACTIVITY_RESTORE_DSN` и
`EVENTS_RESTORE_DSN` через защищённое окружение/libpq service files, отдельно
для каждого владельца. Не передавать чужие DSN runtime-процессам Core/продуктов.
Пока writers остановлены, выполнить:

```sh
sh deploy/activity/verify-restored-commands.sh
```

Проверка только читает БД: для каждого Core `accepted` за последние 7 дней
требует совпадения target, integration, installation, actor и command ID с
receipt Activity либо inbox **и operation** Events. Семидневный горизонт
соответствует `activitybridge.RedeliveryHorizon`; более старые receipts могут
быть штатно удалены GC. Ошибка чтения любой БД тоже завершает проверку с FAIL.
В лог попадает только количество несовпадений, без payload/DSN/tenant IDs.

При FAIL **не запускать writers**: восстановить другой полный согласованный
набор в новые БД и повторить сверку. Если такого набора нет, восстановление
не принято; нужны отдельно спроектированные reconciliation/PITR и оценка
потери данных. Успешная сверка identity — необходимая проверка, но сама по себе
не доказывает согласованность произвольных live dump, настроек и coverage.

`accepted` автоматически не переотправляется. `activity-control retry`
принимает только `failed`; повтор widget-запроса возвращает старую receipt.
Не переводить `accepted` в очередь вручную, не менять command ID и не удалять
inbox/tombstones: это может повторить старые изменения и нарушить порядок
настроек/enable/disable. Истёкший семидневный горизонт не продлевается restore.
Down-миграция не восстанавливает удалённые строки.

## Репетиция

Изолированный compose-проект `amocrm-stage8-backup-test` (не `amocrm-activity`):

```sh
sh deploy/activity/verify-backup-owners.sh --isolated
```

Скрипт отказывается работать, если на кластере уже есть `amocrm_core`,
`amocrm_activity` или `amocrm_events`. Имена проверочных БД оканчиваются на
`_test`. Соседний CRM Events-only сценарий:

```sh
# только в throwaway postgres, не в runtime-проекте
sh deploy/activity/verify-backup.sh
```

## Rollback выпуска

Предыдущие бинарники + предыдущие dump владельцев. Совместимость схемы —
[migrations-compat.md](../verification/stage8-2026-09-08/migrations-compat.md).
`migrate down` удаляет схему; это не штатный rollback.
