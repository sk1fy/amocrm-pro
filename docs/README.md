# Documentation index

Активная документация проекта организована по назначению. Исторические планы и
handoff-файлы не являются текущим backlog.

## Active documentation

- [`architecture.md`](architecture.md) — фактическая runtime-архитектура и основные потоки.
- [`project-memory/CONTEXT.md`](project-memory/CONTEXT.md) — устойчивые факты и ограничения.
- [`project-memory/ROADMAP.md`](project-memory/ROADMAP.md) — phase mapping и recovery protocol.
- [`project-memory/BUGS.md`](project-memory/BUGS.md) — открытые и исправленные дефекты.
- [`adr/`](adr/) — принятые архитектурные решения.
- [`runbooks/`](runbooks/) — operator и integration runbooks.
- [`../api/openapi.yaml`](../api/openapi.yaml) — публичный HTTP-контракт.

## Historical documentation

- [`archive/`](archive/) — исходные планы и session checkpoints.
- [`audits/`](audits/) — датированные аудиты конкретных ревизий.

При расхождении источников приоритет такой:

1. текущий код, миграции и runtime-конфигурация — реализованное поведение;
2. ADR — принятое архитектурное намерение;
3. GitHub Issues — status, scope и backlog;
4. checkpoint/audit — историческое evidence для указанной ревизии.
