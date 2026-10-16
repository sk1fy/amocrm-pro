# R06: приёмка на тестовом аккаунте, 2026-10-16

Тест разрешён владельцем в текущей сессии. Интерфейс — Brave,
`amotehrkrs.amocrm.ru`, account 31391206, «Запрет сделок от Ракурс».
Серверные операции выполнены через разрешённую пользователем сессию pi в VS Code.
Это тестовый стенд, не выпуск на все аккаунты. Время ниже UTC.

## Проверенная поставка

Core `f181753`, migration25; TeamOS backend `6e11cad` (включает R02).
Пересобраны api/worker Core и company/gateway TeamOS, health/ready проверки прошли.
Frontend и архив виджета не менялись. Сохранены прежние overlays/env/grants.

Backup Core: `/root/backups/task-control-r06-20261016-164527`.
Backup TeamOS: `/root/backups/task-control-r06-teamos-20261016-164845`;
включает dump company и `task_control_connection_backup.json`.

Исходная installation: `5a2c8731-ca70-4aeb-95d2-137509749f5c`, generation=1,
revision=1. Исходный binding: `4d0845ca-71ea-4031-a168-6288c51594a1`,
managed, epoch=2. Draft revision=2; publication version=1, одно правило.
CRM user=10912522, pipeline=11405630, status=89351546.

В настройках amoCRM адрес отключения прежде совпадал с OAuth callback.
Через UI сохранён новый `/api/v1/widget/task-control/disconnect`; redirect URI
установки оставлен прежним. Повторное открытие формы подтвердило сохранение.

## Фактическое отключение

- Около 13:58:50 нажата «Отключить»; amoCRM показала «Отключено»/«Установить».
- По серверной проверке pi реальный подписанный сигнал принят, worker выполнил
  probe текущего OAuth и получил 401. Core: disconnect_confirmed,
  signal_pending=false, generation=2, revision=3. Manual uninstall не выполнялся,
  синтетические события до этого результата не отправлялись.
- Первое обновление TeamOS ещё использовало кеш с freshUntil=14:01:51.
  При штатном запросе 14:02:19 TeamOS получил notInstalled/disconnect_confirmed.
  UI показал «Виджет не установлен. Интеграция неактивна».
- Серверная сверка 14:02:49: старый binding revoked, modeEpoch=3,
  bindingRevision=2, runtime sessions=0; draft/publications сохранены.
  Кеш/таблицы для ускорения проверки не изменялись.

Наблюдаемое время до свежего UI — около 3 мин 30 с, в пределах TTL 5 min.
Время самого получения Core-сигнала отдельно с высокой точностью не измерено;
не приписывать UI-задержку доставке webhook.

## Повторная установка

Около 14:03:40 повторная установка выполнена через amoCRM после отдельного
подтверждения пользователем согласия в форме. UI снова показывает установленный
виджет, но native install callback получил 400 (invalid/expired OAuth state).
Core ещё не получил новую авторизацию. Точное содержимое state не исследовалось;
не считать доказанным, что callback содержал именно ранее истёкший state.

В 14:08:05 выполнена свежая штатная OAuth-авторизация через
`/oauth/amocrm/start?integration_code=task-control-widget` с выбором того же
тестового аккаунта и прежних доступов. Callback вернул active для прежнего
installation ID; Core перешёл в generation=3, evidence=registry.
Таким образом, автоматический end-to-end recovery одной кнопкой amoCRM не
подтверждён: отдельный OAuth start является проверенным recovery-шагом.

## Повтор старого сигнала

В 14:09:07 pi выполнил один контролируемый синтетический replay старой
детерминированной подписи disconnect. Это отдельный тест replay, не свидетельство
второго реального удаления. HTTP 202 → probe 200 → в 14:09:16 evidence=registry,
signal_pending=false, revision=7, generation осталась 3. Подпись и секрет не
выводились. Старый binding остался revoked с sessions=0; сам не восстановился.

## Восстановление правил

Через виджет получены свежие одноразовые link и mode proofs, затем через TeamOS UI:
создана новая связь, сохранён draft revision=2, опубликована версия 1 с одним
правилом, включён managed. Правило эквивалентно исходному: user/pipeline/status
указаны выше; showModal/denyExit/checkResponsible/enabled=true,
customers/archived=false. Комментарий: «Восстановление тестового правила после
приёмки R06». Копирование БД и подмена publication pointers не выполнялись.

TeamOS показал «правила из TeamOS», «Последняя версия: 1. 1 правил».
В amoCRM сохранён включённый флажок TeamOS; существующая тестовая сделка 39246629
перезагружена. Сделки, контакты и задачи для этой приёмки не создавались/удалялись.

Финальная read-only сверка pi, 14:15:56 UTC:

- Core: active, registry, pending=false, generation=3, revision=7.
- Новая связь `6d98a4bc-9982-4f42-8cf6-38711f9979b1`: active, managed,
  modeEpoch=2, bindingRevision=1, generation=3; draft revision=2, одно правило.
  Publication `3bb29351-87c9-44a2-8d37-b90152b9d71e`, version=1, одно правило.
  Значения правила совпадают с исходными.
- Старая связь: revoked, epoch=3, revision=2, generation=1, sessions=0;
  исходный draft revision=2 и publication version=1 сохранены.
- Runtime: одна сессия только новой связи (epoch=2, generation=3).
  После reload сделки в 14:15:14 sessions и bootstrap вернули 200;
  bootstrap выдал полный managed-config (2132 bytes).
- Core api/worker: healthy, live=200; TeamOS company/gateway: healthy,
  readyz=200; публичный auth OPTIONS=204, bootstrap без авторизации=401.

## Локальные проверки и границы

Core: PostgreSQL migration25 up/down/up на одноразовой БД;
`go test -race` для taskcontrol/oauth/amocrm с подключённой тестовой БД;
тесты api/worker/services. TeamOS: генерация DTO/OpenAPI, contract check,
company domain/application/bridge и gateway tests; PostgreSQL lifecycle
integration tests с race после последнего исправления. Все прошли.

Автоматическое обнаружение зависит от настроенного disconnect callback и
доступности подтверждающего probe; timeout/heartbeat не объявляют удаление.
Проверен один тестовый аккаунт, не массовый rollout. Нативная установка без
отдельного OAuth recovery не считается подтверждённой. UI редизайн, общие
справочники и полная новая модель динамических правил относятся к следующим
задачам. В этой приёмке сохранён действующий MVP-контракт правил.
