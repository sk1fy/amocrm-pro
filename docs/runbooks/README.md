# Runbooks

- [Команды админки и восстановление квитанций](admin-commands.md)

- [`integrations.md`](integrations.md) — multi-widget provisioning, secret rotation, service grants and disable/restore.
- [`admin-read-api.md`](admin-read-api.md) — optional internal admin read listener, token and actor headers.
- [`widget-capacity.md`](widget-capacity.md) — widget API limits, fair queue rollout and capacity diagnostics.
- [`amocrm-outbound-budget.md`](amocrm-outbound-budget.md) — исходящие лимиты amoCRM по паре и аккаунту, переопределение для аккаунта и диагностика перегрузки.
- [`migrate-down.md`](migrate-down.md) — guarded full migration rollback.
- [`private-widget-e2e.md`](private-widget-e2e.md) — private integration/browser E2E preconditions and checks.
- [`activity-v0.md`](activity-v0.md) — separate service deployment, pilot admission, diagnostics and embedded/gRPC switching.
- [`activity-observability.md`](activity-observability.md) — pilot SLO, scrape overlay, dashboard import and alert rules.
- [`activity-backup-restore.md`](activity-backup-restore.md) — multi-owner dump/restore, RPO/RTO and migration rollback.
- [`activity-module-transfer.md`](activity-module-transfer.md) — moving Activity or CRM Events compute/DB without changing widget origin.

Runbooks must use sanitized examples and must not contain credentials,
production payloads or environment-specific recovery secrets.
