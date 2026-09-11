# MOD-03. Перенос модуля на отдельное размещение

Дата: 2026-09-11. Локальный Docker multi-network стенд и data-plane restore.
**Целевой сервер не проверялся.** Совместное evidence с [ops-02.md](ops-02.md):
данные/RPO — там; адреса, сети, fencing, cutover — здесь.

## Что сделано

- ADR: [`0023-product-module-physical-transfer.md`](../../adr/0023-product-module-physical-transfer.md).
- Runbook: [`activity-module-transfer.md`](../../runbooks/activity-module-transfer.md).
- Overlay: [`docker-compose.activity-transfer.yml`](../../../docker-compose.activity-transfer.yml)
  — не подключается без явного `-f`; default `docker-compose.activity.yml`
  сохраняет `name: amocrm-activity`.
- Скрипт: [`deploy/activity/verify-transfer.sh`](../../../deploy/activity/verify-transfer.sh).
- `cmd/service-certs`: только отказ от production CA / usage; 30-дневные
  localhost-сертификаты не расширялись под prod DNS.
- Config-тест статических адресов и foreign DSN (не process_faults).

Горизонтальные multi-instance (leases, shared budget, общее хранилище) **не
в этом этапе**. Gateway остаётся CORE-04 / ADR-0019.

## Команды и результаты

```sh
sh deploy/activity/verify-transfer.sh
```

Проект: `amocrm-stage8-transfer-test`. Exit **0**.

```
== compose config (compute-only overlay) ==
PASS: overlay config has isolated planes and static RPC host:port names
PASS: docker-compose.activity.yml without overlay keeps name amocrm-activity
== compose config (--profile transfer-db) ==
PASS: transfer-db profile exposes second PostgreSQL aliases for owner DB move
== two-instance CRM Events dump/restore ==
PASS: CRM Events restored onto a second PostgreSQL instance; consumer, coverage and command identity preserved
PASS: target Core origin stays unchanged in this data-plane rehearsal (no Core dump moved)
NOTE: live drain/cutover of Activity then CRM Events processes was not executed
RTO_TRANSFER_DB_SECONDS=3
PROJECT=amocrm-stage8-transfer-test
```

Эквивалент проверки конфига:

```sh
docker-compose -p amocrm-stage8-transfer-test \
  -f docker-compose.activity.yml \
  -f docker-compose.activity-transfer.yml \
  config
```

exit 0 (внутри скрипта). Без overlay: `name: amocrm-activity`.

`TRANSFER_LIVE=1` скрипт отвергает: полный gRPC cutover api/worker/activity/
crm-events на этой машине не выполнялся (уже работает runtime
`amocrm-activity`, его не трогали).

## Сценарии размещения

| Сценарий | Что проверено | Что нет |
| --- | --- | --- |
| Compute-only | Overlay: плоскости `core-plane` / `activity-plane` / `events-plane` / `rpc-plane`; DSN-алиасы `core-postgres` / `activity-postgres` / `events-postgres`; RPC `worker:9090`, `activity:9091`, `crm-events:9092` | Живой перенос процесса с сохранением той же БД |
| Перенос owner DB | Dump Events с инстанса A → restore на инстанс B; consumer, coverage, command ID; runtime SELECT; повтор inbox отклонён | Перенос Activity DB тем же live cutover; финальный sync под нагрузкой |
| Независимый порядок | Runbook: сначала Activity, затем Events; Core origin тот же | Фактический последовательный process cutover |

Fencing описан в runbook: стоп writers, graceful 20 с / RPC 5 с, lease 30 с,
не два scheduler на одну БД. Доказательства fencing — существующий
`TestComponentOSProcessFaults`, не повторённый здесь.

## Сертификаты и адреса

Development SAN: identity, `localhost`, `api`/`worker`. Overlay поэтому
оставляет RPC-имена сервисов Compose. Production DNS, которого нет в
`service-certs`, требует CA среды. TLS verification не ослаблялась.
Ротация: [secrets-rotation.md](../../runbooks/secrets-rotation.md).

Статические адреса уже в `config.go`; тест фиксирует defaults и сохранение
`worker.core.test:9090` / `activity.product.test:9091` /
`crm-events.product.test:9092`, плюс отказ Activity от DSN Events и Core
grpc от продуктовых DSN на transfer-алиасах.

## Измерения RTT / budget

До/после живого RPC не измерялись: cutover процессов не было. Локальный
data-plane RTO Events: 3 с. Ресурсы наращивать по измеренному bottleneck
после реального переноса, не replicas всем. Исходящий бюджет amoCRM —
один Gateway.

## Что осталось открытым

- Физический multi-host, закрытые маршруты и firewall целевой сети.
- Production SAN/mTLS trust и доступ к CA.
- Живой drain → cutover Activity, затем независимо CRM Events, с проверкой
  panel/operation IDs/settings/grants/progress на стенде с разными адресами.
- RTT, deadline errors, connection budgets, throughput до/после.
- Данные, записанные после cutover, на откате (нет live writers).
- Горизонтальное масштабирование нескольких экземпляров.
