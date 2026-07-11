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

## BUG-003 — Повторный forced refresh мог ротировать уже новый токен

- **Status:** Resolved in branch, awaiting merge (GitHub Issue `#25`).
- **Class:** OAuth concurrency / data integrity.
- **Impact:** два растянутых во времени ответа `401` могли выполнить две remote
  rotation и сделать сохранённый refresh token недействительным.
- **Fix:** refresh привязан к наблюдаемой `token_version`; уже обновлённая версия
  возвращается без второго remote exchange.
- **Regression check:** `TestClientStaggered401RefreshesObservedVersionOnce` в
  изолированной PostgreSQL 17, `make integration-test` — PASS.

## BUG-004 — Reauthorization сохраняла устаревший webhook intent

- **Status:** Resolved in branch, awaiting merge (GitHub Issue `#26`).
- **Class:** Lifecycle / data convergence.
- **Impact:** повторная авторизация существующей installation не обновляла desired
  webhook settings и не возвращала reconciliation в `pending`.
- **Fix:** conflict upsert обновляет settings/status/error и ставит reconcile job.
- **Regression check:** `TestSaveInstallationReauthorizationRefreshesWebhookIntent`
  — PASS в `make integration-test`.

## BUG-005 — OAuth callback падал из-за SQL alias в ConsumeState

- **Status:** Resolved in branch, awaiting merge (GitHub Issue `#27`).
- **Class:** Runtime / OAuth.
- **Impact:** валидный callback завершался PostgreSQL `42703` до code exchange.
- **Fix:** CTE возвращает именованный `return_url`, а `COALESCE` выполняется во
  внешнем select.
- **Regression check:** concurrent callback и atomic persistence tests — PASS.

## BUG-006 — Refresh finalization конфликтовала с reauthorization

- **Status:** Resolved in branch, awaiting merge (GitHub Issue `#28`).
- **Class:** OAuth concurrency / availability.
- **Impact:** cancellation могла потерять одноразовую rotation; разные порядки
  locks создавали deadlock; старый `401` мог перезаписать новую авторизацию
  статусом `reauth_required`.
- **Fix:** bounded uncancelled finalization, единый lock order и version-fenced
  reauth transition с lifecycle guard.
- **Regression check:** cancellation, deadlock, stale mark и concurrent success
  tests — PASS с `-race` на PostgreSQL 17.

## BUG-007 — Transport error раскрывала webhook key

- **Status:** Resolved in branch, awaiting merge (GitHub Issue `#29`).
- **Class:** Security / secret handling.
- **Impact:** стандартная `url.Error` включала полный request URL с секретным
  webhook key в сохраняемый reconciliation error.
- **Fix:** HTTP client сохраняет только очищенную underlying transport cause.
- **Regression check:** `persisted_transport_error_is_sanitized` — PASS; key не
  присутствует ни в возвращённой, ни в сохранённой ошибке.

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
