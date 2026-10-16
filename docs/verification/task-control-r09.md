# КТ-01.R09: покрытие Core

16.10.2026. Базовая реализация Core: `960c7c2c90019e89997043903785ed89970abec1`,
миграции 1–30; пакет виджета 1.2.3. Полная живая приёмка R09 ещё не завершена.

В обязательный PostgreSQL-прогон Docker/CI добавлен `internal/taskcontrol`.
Раньше этот пакет запускался unit target без `TEST_DATABASE_URL`, поэтому
PostgreSQL-сценарии могли пропускаться. Теперь `make integration-test` проверяет
их вместе с остальными пакетами, последовательно (`-p=1`) с race detector.

Пакет проверяет Core JWT/grant, replay одноразовых подтверждений, generation,
подписанные source-запросы, default deny, кеш и поздний ответ старого поколения,
disconnect/reconciliation и runtime descriptor. Это проверки с синтетическим
CRM provider и настоящим PostgreSQL; они не подтверждают работу браузера amoCRM.

Бизнес-код и миграции R09 не меняет. Межрепозиторный стенд, матрица проверок,
готовность API и оставшиеся живые сценарии описаны в backend
`docs/integrations/task-control-r09-acceptance.md`; воспроизводимый запуск —
`scripts/task-control-r09-acceptance.py` в том же репозитории.

Прежняя [живая приёмка R06](../runbooks/task-control-r06-acceptance.md) относится
к другим SHA. Её результат нельзя переносить на новый ZIP и динамический runtime.
