# Documentation index

Активная документация проекта организована по назначению. Исторические планы и
handoff-файлы не являются текущим backlog.

## Active documentation

- [`architecture.md`](architecture.md) — фактическая runtime-архитектура и основные потоки.
- [`project-memory/CONTEXT.md`](project-memory/CONTEXT.md) — устойчивые факты и ограничения.
- [`project-memory/ROADMAP.md`](project-memory/ROADMAP.md) — phase mapping и recovery protocol.
- [`project-memory/BUGS.md`](project-memory/BUGS.md) — открытые и исправленные дефекты.
- [`adr/`](adr/) — принятые архитектурные решения.
- [`specs/lead-distribution-v1/12-independent-review.md`](specs/lead-distribution-v1/12-independent-review.md) — независимое ревью РС-03–РС-10 и исправления перед PR.
- [`specs/lead-distribution-v1/11-local-acceptance.md`](specs/lead-distribution-v1/11-local-acceptance.md) — локальная матрица РС-10 и оставшиеся live-проверки.
- [`runbooks/lead-distribution-pilot.md`](runbooks/lead-distribution-pilot.md) — наблюдение, остановка, согласованное восстановление и подготовка пилота.
- [`runbooks/lead-distribution-admin.md`](runbooks/lead-distribution-admin.md) — локальная диагностика и безопасное восстановление распределения.
- [`runbooks/`](runbooks/) — operator и integration runbooks.
- [`specs/`](specs/README.md) — действующие продуктовые и интеграционные контракты.
- [`fixtures/`](fixtures/) — небольшие синтетические примеры для спецификаций и тестов.
- [`../deploy/observability/server/README.md`](../deploy/observability/server/README.md) —
  развёртывание и проверка server-observability стека.
- [`../api/openapi.yaml`](../api/openapi.yaml) — публичный HTTP-контракт.
- [`../api/admin-openapi.yaml`](../api/admin-openapi.yaml) — внутренний контракт чтения и команд админки.

- [`verification/staging-sync-2026-10-06.md`](verification/staging-sync-2026-10-06.md) —
  перенос серверных исправлений, проверки и оставшиеся ограничения.

## Historical documentation

- [`archive/`](archive/) — исходные планы и session checkpoints.
- [`audits/`](audits/) — датированные аудиты конкретных ревизий.
- [`plans/`](plans/) — завершённые датированные планы реализации; это не backlog.
- [`verification/`](verification/) — исторические итоговые отчёты по проверкам.

Сырые логи команд, временные JSON-снимки, патчи и checksum-манифесты в Git не
хранятся: итог и существенные ограничения должны быть записаны в Markdown-отчёте,
а воспроизводимые проверки — в тестах и CI.

При расхождении источников приоритет такой:

1. текущий код, миграции и runtime-конфигурация — реализованное поведение;
2. ADR — принятое архитектурное намерение;
3. GitHub Issues — status, scope и backlog;
4. checkpoint/audit — историческое evidence для указанной ревизии.
