# Проверка внешнего аудита этапов 6 и 7

База: `0c0158a4fe6715c379e35688dc173ad22ec31f31`. Проверка и исправления
выполнены локально; production и пользовательские данные не затрагивались.

## Подтверждено и исправлено

1. **P2, Activity 429 не соответствует OpenAPI.** Все семь маршрутов через
   штатную регистрацию Bridge и реальную цепочку JWT/CORS/limiter/consumption
   воспроизвели отсутствие обязательных полей схемы. Для каждого запроса
   использован свежий подписанный JWT; handler не должен вызываться.
   Ответ 429 теперь явно описан как `oneOf`: прежний `WidgetRateLimitError`
   либо полный `ActivityError`. Формат остальных widget endpoints не менялся.
   Проверены оба допустимых варианта; неполный downstream-ответ и неизвестный
   code остаются невалидными. Поля полного ActivityError не стали optional.
2. **P2, max_attempts обходится после batch cleanup.** 101 исчерпанная команда
   и одна допустимая воспроизвели отправку неверной команды. Сценарий проверен
   отдельно для pending_delivery и delivering с истёкшим lease.
   Атомарный claim теперь требует `attempts < max_attempts`; предварительный
   bounded sweep сохраняет наблюдаемое failed/error_code. После исправления
   получатель вызван только для допустимой команды, все 101 исчерпанные строки
   переходят в failed за два тика без увеличения attempts сверх максимума.
3. **P3, противоречие REL-03/CORE-05.** План и актуальный REL-03 описывают
   работающий календарный горизонт и terminal GC с tombstones. Отчёт этапа 6
   явно помечен как исторический до CORE-05. Общий бюджет хранения не объявлен
   конечным: Activity receipts, paused jobs и uncertain effects остаются
   сохраняемыми классами. В ADR-0016 записаны причины и модель оценки роста;
   измерения и бюджет целевой среды остаются открыты в OPS-01/02.

## Регрессии

- `TestWidgetRoutesRateLimitBeforeConsumptionAcrossIntegrations` расширен
  проверкой реальных ответов семи Activity-маршрутов по OpenAPI. Сохраняются
  проверки, что отклонённый запрос не тратит JWT и не создаёт durable admission.
- `TestActivityRateLimitResponseVariants` проверяет ветви схемы и отрицательные
  примеры для каждого маршрута.
- `TestDeliverySkipsExhaustedBeyondCleanupBatch` проверяет оба состояния outbox.

[До исправления](before.txt): оба теста FAIL на ожидаемое правильное поведение.
[После исправления, целевые пакеты](targeted.txt): PASS с `-race`.
[OpenAPI и варианты ошибок](contract.txt): PASS с `-race`.

## Границы

Новых миграций нет. Требование дренировать старых OAuth refresh executors
перед обновлением остаётся в силе (ADR-0017); это условие выпуска, а не новый
дефект текущего протокола. CORE-06, живой пилот и проверка с реальным старым
бинарём этим исправлением не закрываются. Неизвестный исход refresh нельзя
устранять ручным освобождением lease ради повторного использования token.

## Итоговые проверки

- `make TEST_COMPOSE_PROJECT=amocrm-audit-followup-gate-test integration-test`:
  **PASS, 221 верхнеуровневый тест, 2 штатных opt-in performance SKIP**.
  Включает down/up и конкурентные миграции. [Лог](integration-test.txt).
- `make test`: **PASS**, сборки бинарей, gofmt, `go vet ./...`,
  `go test -race -count=1 ./...` в Docker. [Лог](make-test.txt).
- Целевые пакеты: 30 PASS; контракт: 3 PASS. Полный Activity CI повторно
  не запускался: owner/schema/protobuf/UI не менялись. Результаты предыдущего
  Activity CI не выдаются за новый прогон.
- Исходники совпадают с [SHA-256 manifest](source-manifest.json).
  [Машиночитаемые результаты](checks.json). `git diff --check` — PASS.
- Отдельные тестовые Compose-проекты и cache volume удалены;
  [cleanup целевого прогона](targeted-cleanup.txt), полный gate использовал
  штатный trap. Ни миграции, ни тестовые reset не направлялись в runtime.
