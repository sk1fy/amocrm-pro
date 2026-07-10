# ADR-0001: PostgreSQL без Redis в текущей основе

- **Status:** Accepted.
- **Date:** 2026-07-10.
- **Decision owners:** project maintainers.

## Контекст

Сервису нужны основное tenant-хранилище, OAuth credentials, durable webhook inbox, дедупликация, idempotency, audit trail и очередь фоновых работ. Исходный архитектурный план допускал Redis для replay protection, distributed rate limiting, короткого cache и некоторых locks, но текущая основа должна запускаться без Redis.

На раннем этапе дополнительное хранилище увеличивает число failure modes, operational cost и сложность локального/CI окружения. PostgreSQL уже обязателен, а требуемые гарантии первого масштаба выражаются его транзакциями, unique constraints, row locks, partial indexes и lease-based jobs.

## Решение

Использовать PostgreSQL 17 как единственное stateful runtime-хранилище текущей основы. Redis не входит в Compose, конфигурацию, application dependencies или обязательную инфраструктуру.

В PostgreSQL размещаются:

- system of record для integrations/installations и encrypted credentials;
- durable raw webhook deliveries и normalized inbox events;
- job queue и история попыток;
- widget `jti` replay protection и idempotency keys;
- audit log;
- координация через транзакции, unique constraints и lease/row locking.

Возможности, обычно реализуемые через Redis, на первом этапе решаются так:

- replay/idempotency — atomic insert и unique constraints;
- background jobs — таблица `jobs`, конкурентный claim и lease;
- refresh coordination — PostgreSQL locking/optimistic token version;
- rate limiting — process-local или PostgreSQL-backed механизм, выбранный после измерений;
- cache — не вводится до появления подтверждённого bottleneck.

Добавление Redis возможно только отдельным ADR с измерениями, конкретной нагрузкой, failure semantics, ownership и migration/fallback plan. Оно не должно менять PostgreSQL как durable source of truth.

## Последствия

### Положительные

- один durable data plane и меньше operational dependencies;
- атомарность webhook delivery + job и inbox event + job в одной транзакции;
- воспроизводимое Docker-окружение и более простой production bootstrap;
- replay, deduplication и idempotency сохраняются после рестартов;
- меньше риск расхождения cache/queue с основной БД на стартовом этапе.

### Отрицательные и риски

- queue/inbox создают дополнительную write/load нагрузку на PostgreSQL;
- cleanup, retention, vacuum и capacity требуют явного наблюдения;
- process-local rate limiting не координируется между replicas;
- high-throughput queue может потребовать partitioning, отдельную БД или иной broker;
- горячие locks/unique indexes могут стать contention point.

Риски контролируются метриками DB pool/locks/queue age, bounded batches, backoff, retention jobs и нагрузочными тестами до масштабирования.

## Отклонённые варианты

### PostgreSQL + Redis с первого дня

Отклонён как преждевременное усложнение и противоречащий текущему ограничению. Нет измерений, показывающих необходимость второго stateful dependency.

### Redis как primary job queue

Отклонён: webhook acceptance должен быть связан с durable transaction в PostgreSQL, а потеря/рассинхронизация queue недопустима.

### In-memory queue

Отклонён: jobs и webhook events должны переживать restart/crash и поддерживать несколько workers.

## Проверка решения

Перед пересмотром собирать как минимум queue depth/age, job claim latency, PostgreSQL CPU/IO/locks, ingress rate и rate-limit contention. Порог пересмотра фиксируется отдельным ADR на основании production-like load test или эксплуатационных данных, а не предположения.
