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

## Расследование 503, 21.10.2026

Сервер проверен read-only через pi в терминале VS Code. За окно
15:10–15:27 МСК (12:10–12:27 UTC) найдены три Task Control auth 503:
12:11:36, 12:23:49, 12:24:46 UTC. Ответ сформирован Core handoff; точная ветвь и
длительность старых запросов не доказаны: отдельный listener не имел access log,
а handoff не журналировал ошибки CRM identity, AdmitAssertion и подписи.
Это не доказательство ошибки версии, БД или таймаута amoCRM.

В Distribution подтверждены company widget-runtime 503 с длительностями
1.19–9 секунд, транспортные ошибки amoCRM и throttling в том же окне. 9 секунд
совпадают с Core→TeamOS budget; какая конкретная операция вернула старый 503,
существующие логи не позволяют установить. Последовательные timezone/users/pipelines
в referenceData увеличивали задержку: локальная правка запускает эти независимые
чтения параллельно, сохраняя лимитер и capability fences. Это уменьшение задержки,
а не доказанное устранение всех серверных 503. Добавлена безопасная диагностика
источников и корреляция запросов; фактический результат проверяется после выкладки.

Nginx timeout, рестарты и нехватка ресурсов в проверенном окне не подтверждены.
Webhook 400 относится к 19.10 до обновления worker, это историческая ошибка;
для её снятия требуется штатный reconcile, новая правка кода по этому факту не нужна.
По отчёту pi текущий deployment audit 1.2.4 имеет artifactMatch=true,
acceptancePassed=false, releaseApproved=false. Исторический artifactMatch=false
не относится к текущему кандидату. R10 binary rollback/restore и полная R09
live/security/dynamic/browser-матрица остаются непроведённой приёмкой.

Живая проверка Brave 1.2.4 на тестовой сделке 39230235 подтвердила guard без задачи,
создание задачи ответственному штатной формой, отложенный переход, возврат guard
после завершения тестовой задачи и повторное напоминание на той же странице воронки
после закрытия окна (ожидание 35 секунд). Тестовая задача завершена. Это не новый
прогон всей матрицы R09. В серверном срезе pi received=25, applied=0 для 1.2.4;
это не version rejection и не подтверждение полной application-приёмки. При schema 1
отсутствие v2 ACK ожидаемо. Отдельно проверить applied на активной runtime-вкладке.

## Private references route, 21.10.2026

При продолжении живой R09 owner-сессия TeamOS на тестовом аккаунте получила
`GET /api/v2/task-control/connections/{bindingId}/references` → HTTP 200,
`state=error`, `crm=null`, `crm/references_unavailable`. Чтение installation
той же сессией успешно: runtime 1.2.4, generation 3, revision 9. Режим, schema,
draft и публикация при диагностике не изменялись.

Серверная диагностика установила 404 на private nginx bridge: company вызывает
`/internal/task-control/accounts/{accountId}/references`, а private include
содержал только `/internal/v1/task-control/accounts/`. Запрос не доходил до Core.
Grant включён с `references_read=true`; это не отказ grant и не CRM timeout.

В private include добавлен только точный references route с числовым account ID.
Upstream, Host и X-Forwarded-Proto совпадают с существующим private маршрутом.
Он остаётся внутри прежнего private TLS vhost с прежними allow/deny и signed
service-grant authentication; публичный include не расширен.

Изолированный nginx smoke проверяет 17 публичных и 7 приватных путей: references
доходит до Core stub только через private listener, публичный доступ и лишние
суффиксы/операции получают 404. Это routing test, не живая приёмка TLS/ACL/CRM.
Bounded startup retry дополнен обработкой закрытого до HTTP-ответа соединения.

После merge оператору нужно сохранить backup private snippet, обновить его из
main внутри существующего private TLS server, выполнить `nginx -t` и reload,
сохранив ACL и закрытый 8443. Затем запустить штатный references refresh и проверить
завершение операции/свежий CRM snapshot. SQL clearing/cache seed не нужны.
Validate/publish, live v2 ACK и полная R09/R10 остаются открытыми до этих проверок.
