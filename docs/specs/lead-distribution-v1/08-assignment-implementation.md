# РС-04: сохраняемое назначение ответственного

Дата: 02.10.2026. Реализована техническая операция Core. Бизнес-очередь и
сохранённые версии решений TeamOS относятся к РС-06; текущий production
ValidateDecision возвращает отказ `decision_not_ready`.

## Runtime

Core предоставляет private worker HTTP API, описанный в
[`api/distribution-openapi.yaml`](../../../api/distribution-openapi.yaml):
POST assignments, GET operation, POST cancel и POST reconcile. Публичных путей
для изменения ответственного в этом этапе не добавлено. Прямой browser доступ
к private listener запрещён deployment границей.

Admission проверяет подписанный company/installation scope, current binding и
capability. Intent, неизменяемая квитанция, обычный Core job, глобальный guard
account+lead, первый result и result outbox записываются одной транзакцией.
Idempotency-Key — canonical UUID; namespace assignment binding+key, а operation
ID дополнительно сериализуется между разными transport keys. Semantic hash
нормализует timestamps в UTC и включает весь исходный envelope и decision.
Повтор одинаковой команды возвращает исходный 202 receipt даже после истечения
validUntil или отзыва текущей binding. Чужой scope не раскрывает operation.

Cancel/reconcile используют namespace binding+action+key и включают operation ID
в immutable hash. Контрольная квитанция и audit actor/reason записываются вместе
с переходом, resultVersion и outbox. В отличие от предложенного пакета РС-02,
реализованный private control API возвращает **200 Operation**, а не 202 receipt;
requestedAt не принимается: timestamp фиксирует сервер. Это явно отражено в
runtime контракте; HTTP fixtures проверяются именно против него.

## Выполнение и восстановление

Worker использует existing jobs lease/attempt как fence, проверяет точный
job/installation/actor/resource scope, current capability/binding, mapping,
активного CRM получателя и актуальное состояние сделки. Подготовка OAuth и
ожидание общего outbound бюджета происходят до свежих identity/resource checks.
Обычный mapped actor проверяется через current CanViewLead, без admin-only
fallback. TeamOS подтверждает точные operation/decision/fence и authorization
до 5 секунд; произвольные browser revisions не являются полномочиями.

Порядок SQL locks: jobs → operation → global guard. После ожидания locks
lease и authorization проверяются повторно по текущему времени. Dispatch intent
сохраняется до PATCH. После commit непосредственно перед Assign снова проверяются
current job/capability/binding/cancel/validUntil и срок короткого разрешения.
Сетевой вызов не выполняется внутри длинной SQL транзакции.

SDK делает один PATCH только responsible_user_id; скрытого повтора PATCH после
401, redirect или неоднозначной ошибки нет. Успех требует валидного сохранённого
ACK и последующего GET, начатого после durable response_finished_at, с updated_at
не старше подтверждённого ACK. Несовпадение target/stage даёт settled conflict.
keep и уже выбранный текущий target возвращают no_change с kept/already_target
без PATCH. GET→PATCH не является CRM CAS: внешняя правка может попасть в окно
между последним чтением и записью; атомарность с ручными CRM изменениями не обещается.

Если процесс упал после intent, ответ потерян или ACK неполный, takeover выполняет
только наблюдение. Matching target или старый ответственный не доказывают авторство
либо отсутствие поздней записи. outcome_unknown сохраняет guard без срока истечения.
Подтверждённый ACK, сохранённый до сбоя локального результата, позволяет восстановить
результат свежим GET без второго PATCH. Поздний worker может сохранить свидетельство
своей отправки, но не может изменить result/guard после потери authoritative fence.

Reconcile требует expectedResultVersion, не меняет target или исходную команду,
не выполняет PATCH и не освобождает неизвестный эффект только по observed state.
Cancel до отправки завершает операцию и снимает guard; после possible dispatch
фиксирует cancellationRequestedAt и оставляет неопределённость/guard. Dead/reaped
job до dispatch безопасно завершается rejected; после dispatch guard сохраняется.

## Сохранность

Миграция 000019 хранит immutable command/receipt, single-dispatch attempt ledger,
observations, append-only results и outbox. Каждая версия результата и outbox
фиксируются атомарно. Глобальный guard не содержит installation в ключе: rebind,
другая интеграция/установка или новый episode не обходят предыдущую неизвестную
операцию. Core cleaner исключает pinned distribution jobs; семидневный общий
TTL не стирает command identity. Down migration отказывается удалять использованные
operations. Пилотный журнал не очищается автоматически; будущая retention policy
должна сохранять unresolved evidence и долговечные replay identities.

## Границы готовности

- Реальный TeamOS decision registry, rule/availability/claim revisions и бизнес-очередь
  ещё не созданы. Добавлен отдельный подписанный ValidateDecision seam с capability
  decision-validation, проверками scope/actor/mapping и безопасным отказом; current
  production operation не может выполнить PATCH без будущего владельческого разрешения.
- Автоматическая push-доставка pending result outbox относится к РС-05. Сейчас Core
  сохраняет outbox и предоставляет GET operation.
- Revoked binding/capability не запрещают узкое чтение/reconcile immutable operation.
  Если обычный OAuth недоступен после disable/uninstall/reauth, возвращается 503;
  guard остаётся. Обход OAuth lifecycle ради reconcile не добавлен.
- Даже полученный HTTP 401/403/404/429 здесь консервативно удерживает неизвестный
  эффект без автоматического повторного PATCH. Причина сохраняется как reauth,
  permission, missing resource или rate limit. Слепое снятие guard по HTTP-коду
  или таймеру не реализовано. Непроверяемое операторское «доказательство» не принимается.
- Живого тестового аккаунта по-прежнему нет. Installed-widget/CRM/deployment пилот
  не выполнялся; результаты ниже доказывают локальную реализацию и протокол.

## Трассировка

| Подзадача | Результат |
| --- | --- |
| РС-04.1.1 | Additive LeadState и отдельный strict GetLeadSnapshot; legacy трёхполевая проверка сохранена |
| РС-04.1.2 | Prepared single-attempt responsible-only PATCH, общий OAuth/budget, классификация dispatch/HTTP/неполного ответа |
| РС-04.2.1 | Atomic admission/job/receipt/guard/result/outbox, UUID idempotency и private API операции |
| РС-04.2.2 | Worker leases/fencing/current preconditions и проверяемый decision seam; production business grant ожидает РС-06 |
| РС-04.3.1 | Crash/timeout reconciliation, durable ACK recovery, unknown без повторной отправки/таймерного release |
| РС-04.3.2 | Source precondition conflict и post-ACK manual conflict; GET→PATCH CAS limitation зафиксирована |

## Проверки

Current-source PostgreSQL race suite прошёл, включая signed HTTP request/response
schema validation, scoped replay/conflict, cancellation/reconciliation receipts
и audit, ordinary actor ACL, global guard через другую установку/rebind,
lease expiry во время SQL ожидания, поздний ответ и старое наблюдение, падение между
ACK и результатом, no_change, reaper exhaustion, реальный cleaner после 7 дней и
отказ rollback использованной миграции. Runtime OpenAPI и JSON Schema компилируются.

- `make test`: passed — formatting, go vet и `go test -race -count=1 ./...`.
- `make integration-current-test`: passed — полный набор PostgreSQL suites с
  текущими исходниками, включая legacy module regressions, новые операции,
  HTTP schemas и обновлённые bounded metrics распределения.
- `make distribution-integration-test`: passed — финальная текущая версия,
  включая отдельный regression key-A/payload-B при двух ранее созданных operations.
- Migration19 up/down/concurrent-migrator проверки выполнены полным integration
  запуском; повторно строить мигратор после последних Go-only исправлений не нужно.
- `git diff --check`: passed.

Первоначальные полные integration запуски обнаружили устаревшее фиксированное
число metric series и неполные wire schema fields. Ошибки исправлены, metric
fixture проверяет реальную distribution count/oldest age, HTTP fixtures проходят
runtime schema validation. Итоговый полный current-source PostgreSQL запуск
завершился с exit 0. Живой CRM и production deployment этим не проверены.
