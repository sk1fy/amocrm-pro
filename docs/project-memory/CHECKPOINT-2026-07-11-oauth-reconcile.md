# Checkpoint: OAuth concurrency and webhook reconciliation contracts

Дата: 2026-07-11 (Europe/Moscow)

Ветка: `codex/oauth-reconcile-contract-tests`

Base: `d4bb5e6` (merged PR `#22`)

Issue: `#24`; найденные defects: `#25`–`#29`.

Предыдущий checkpoint: [`CHECKPOINT-2026-07-11.md`](CHECKPOINT-2026-07-11.md)

## Цель среза

Зафиксировать контракт OAuth callback/reauthorization и конкурентного token
refresh, проверить webhook reconciliation на реальной PostgreSQL и устранить
обнаруженные data-integrity/security дефекты. Redis не добавлен; Go и БД
запускаются только через Docker/Make.

## Реализовано

- OAuth state атомарно потребляется ровно один раз даже конкурентными callback.
- Сохранение installation, encrypted credentials, reconcile job и audit остаётся
  одной транзакцией и полностью откатывается при ошибке enqueue.
- Reauthorization обновляет desired webhook settings и возвращает reconciliation
  в `pending`.
- `AccessToken` несёт `token_version`; повторный `401` обновляет только ту версию,
  которую реально использовал запрос.
- Успешная одноразовая token rotation финализируется bounded-контекстом даже при
  cancellation вызывающего запроса.
- Переход в `reauth_required` fenced по версии и не перезаписывает более новую
  авторизацию, disabled или uninstalled installation.
- Порядок locks installation/credentials унифицирован и покрыт регрессиями на
  deadlock и конкурентный successful refresh.
- Webhook reconcile contract покрывает converged/missing/429/422/tenant mismatch
  и transport failure; секретный webhook key не попадает в persisted error.
- Docker integration target теперь включает `internal/oauth`.

## Найденные и исправленные дефекты

| Issue | Кратко | Regression evidence |
| --- | --- | --- |
| `#25` | повторная remote rotation после растянутых `401` | staggered-401 test, один exchange |
| `#26` | stale webhook intent после reauthorization | settings/status/job integration test |
| `#27` | SQL `42703` в OAuth state consumption | callback/store integration tests |
| `#28` | потеря rotation, unfenced status write и deadlock | cancellation/fencing/lock tests |
| `#29` | webhook key в transport error | sanitized persistence test |

## Проверки, выполненные через Docker

- `make integration-test` — PASS:
  - отдельная PostgreSQL 17 database с guarded destructive reset;
  - migration `up -> down -> concurrent up` и schema/checksum assertions;
  - race-enabled tests `internal/jobs`, `internal/oauth`, `internal/webhook`;
  - callback replay/atomicity/rollback, coordinated refresh, staggered `401`,
    cancellation finalization, reauth fencing и lock ordering;
  - webhook convergence, retryable/permanent outcomes, tenant guard и redaction.
- `make test` — PASS:
  - Docker runtime builds;
  - `gofmt` check;
  - `go vet ./...`;
  - `go test -race -count=1 ./...`.
- `git diff --check` — PASS до оформления документации.

## GitHub memory / resume order

1. Commit и push этой ветки; создать PR с `Closes #24`–`Closes #29`.
2. Дождаться всех GitHub Actions, не считать наличие workflow доказательством.
3. После merge перейти к Issue `#8`: полный widget idempotency/authorization
   contract, либо к первому bounded workflow из Issue `#10`.
4. Hardening backlog остаётся в `#21`; Redis не добавлять без измерений и ADR.

Безопасность checkpoint: secrets, OAuth tokens, webhook keys, raw payloads и PII
не записывались. Commit SHA и PR URL должны быть добавлены в комментарий Issue
`#24` после push; сам commit, содержащий этот файл, является локальной versioned
точкой восстановления.
