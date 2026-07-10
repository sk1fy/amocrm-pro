# Bugs и внешние блокеры

Реестр содержит только воспроизводимые дефекты и реальные внешние блокеры. Не добавлять в evidence secrets, tokens, raw webhook payloads, PII или закрытые GitHub данные.

## BUG-001 — GitHub connector не мог создавать/обновлять Issues

- **Status:** Resolved (GitHub Issue `#15`).
- **Class:** External blocker / permissions.
- **Severity:** Blocks remote project-memory synchronization; не блокирует runtime-разработку.
- **Observed:** 2026-07-10.
- **Error:** `403 Resource not accessible by integration` при попытке write operation для GitHub Issue.
- **Impact:** На момент инцидента нельзя было создавать и обновлять remote project memory.
- **Workaround:** Versioned recovery-copy остаётся в `docs/project-memory/` и `docs/adr/`.
- **Likely owner:** владелец GitHub App/connector installation и permissions репозитория/organization.
- **Resolution criteria:** connector identity получает минимально необходимые Issue/Project write permissions; тестовая sanitized запись создаётся и обновляется; URL и дата успешной проверки фиксируются здесь.
- **Resolution:** connector получил write access; этапы, программа, ADR backlog и defects созданы в Issues `#2`–`#18`.

## BUG-002 — Worker Docker build блокировался неиспользуемым import

- **Status:** Resolved.
- **Class:** Build regression.
- **Affected file:** `cmd/worker/main.go`.
- **Symptom:** компиляция worker останавливалась из-за неиспользуемого import `net/http`.
- **Cause:** import остался после изменения HTTP health server wiring, но пакет больше не использовался.
- **Fix:** неиспользуемый import удалён; в текущем исходнике `cmd/worker/main.go` его нет.
- **Regression scope:** Docker build/test target для worker, а не host Go build.
- **Evidence boundary:** дефект найден во время Docker build и исправлен в исходнике; полная Compose validation всё ещё продолжается и не считается завершённой этой записью.
- **Prevention:** сохранять Docker compile/test target обязательным до сборки runtime images в CI.

## Шаблон новой записи

```md
## BUG-NNN — Краткое название

- **Status:** Open / In progress / Resolved / Won't fix.
- **Class:** Runtime / Data / Security / Build / External blocker.
- **Severity:** ...
- **Observed:** YYYY-MM-DD.
- **Affected version/commit:** ...
- **Reproduction:** только sanitized шаги.
- **Expected:** ...
- **Actual:** ...
- **Impact:** ...
- **Root cause:** unknown до подтверждения.
- **Workaround:** ...
- **Fix:** ...
- **Regression check actually run:** команда, environment, result.
- **GitHub Issue:** URL или причина отсутствия.
```
