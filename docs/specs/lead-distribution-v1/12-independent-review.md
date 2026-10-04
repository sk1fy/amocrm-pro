# Независимое ревью РС-03–РС-10

Дата: 04.10.2026. Источник требований и комментариев:
[Rakurs Infrastructure 2.0](https://app.clickup.com/90122072803/v/l/li/901222819723).
Прочитаны описания 80 карточек и их комментарии. Ревью выполнено по коду,
контрактам и регрессионным сценариям, отдельно от прежних отчётов реализации.
Живой пилот и изменение статусов ClickUp в этот выпуск не входят.

## Найденные проблемы и исправления

| Уровень | Этап | Проблема | Исправление |
| --- | --- | --- | --- |
| P1 | 04–06, 10 | Истёкший frozen intent при Core 404 бессрочно держал claims сделки и группы | Core фиксирует terminal no-attempt под общими admission locks; TeamOS принимает только доказанный результат и освобождает claims без RR turn |
| P1 | 04 | Подтверждение single-lead PATCH принималось только как `_embedded.leads`; обычный успешный ответ превращался в unknown | Поддержана модель `id/updated_at` и совместимый collection; неполные, чужие и противоречивые ответы остаются unknown |
| P1 | 04–06 | Core заменял decision_expired/recipient_unavailable на operation_unresolved; TeamOS не мог пересчитать безопасно отклонённое решение | Коды сохранены в wire-контракте; paired сценарий проверяет возврат живого эпизода в waiting/decision_recalculation |
| P1 | 07, 10 | Включение группы до сохранения observe-правила могло открыть окно live-назначения | TeamOS сохраняет pause → правило/режим → enable с актуальными revision; ошибка оставляет группу на паузе |
| P2 | 07 | Неудачная повторная проверка unknown action могла забыть pending request ID | Исходный intent сохраняется до подтверждённого результата |
| P2 | 07–08 | После 403 интерфейсы сохраняли ранее загруженные закрытые данные | Данные скрываются при потере прав; ошибки доступности не уничтожают черновик |
| P2 | 07–08 | Отсутствующий executionMode отображался как рабочий режим | Неизвестный режим показывается явно |
| P2 | 08 | Таймер обновления, совпавший с долгой записью, останавливал polling виджета | Обновление возобновляется после завершения записи |
| P2 | 09 | Reconcile скрывался у applying/confirming с доказательствами для восстановления | Доступность действия соответствует паре state/effect; Core проверяет CAS и наличие попытки |
| P2 | 09 | Ошибка health после 404 диагностики скрывала недоступность Core | Недоступность показывается явно |
| P2 | 09 | Успех команды GET-reconcile скрывал оставшийся unknown самого назначения | Безопасные assignment/effect/evidence поля сохраняются и показываются отдельно |

Формат ответа single-lead PATCH сверен с официальным примером
[amoCRM](https://www.amocrm.ru/developers/content/crm_platform/tags-api).
Это контрактная проверка и локальные HTTP fixtures, не запрос к живому аккаунту.

## Границы безопасности и совместимость

Новый endpoint описан в [runtime API](../../../api/distribution-openapi.yaml)
и [ADR-0036](../../adr/0036-distribution-expired-admission.md).
Новые миграции для исправлений не требуются; исходные миграции этапов включены
в PR. Порядок выпуска: Core, TeamOS backend, затем интерфейсы.
Старый Core не подтверждает expiry: TeamOS сохраняет IDs и claims до обновления.

Настоящий uncertain PATCH нельзя завершить только по таймеру, 404 или совпавшему
GET. Сохранённые attempts/ACK, result versions и account-wide guard защищают
сделку после рестарта и смены binding. GET→PATCH всё ещё не является атомарным
CAS с ручным изменением amoCRM.

РС-03 проверен на HMAC/scope/nonces, capability, mappings и текущие права.
РС-05 — на frozen inbox/outbox, receipt, версии и повторную доставку.
РС-06 — на durable очередь, графики, claims и подтверждённый RR turn.
РС-07–09 — на серверные права, ошибки, повторные действия и диагностику.
РС-10 — на наблюдение, границу live, восстановление и совместимость веток.

## Проверки

- Core: current-source Docker `gofmt`, `go vet ./...` и полный
  `go test -race -count=1 ./...` — PASS; OpenAPI и `make vulncheck` — PASS.
- TeamOS backend: все unit/lint/контракты — PASS; PostgreSQL/race distribution
  и финальная lost-ACK/restart регрессия — PASS.
- TeamOS: build/contract, 399 unit, 17 E2E и 7 финальных focused — PASS;
  lint без ошибок, 5 прежних предупреждений academy.
- Виджет: 14 unit/lifecycle/packaging, 20 browser SDK fixture, preview/release
  и воспроизводимость ZIP — PASS.
- Paired Core↔TeamOS HTTP/HMAC, две owner DB: 13 сценариев — PASS,
  включая новый expiry fence, no second PATCH и согласованный backup/restore.
- Core `make integration-test`: полный PostgreSQL-набор, миграции
  up/down и конкурентные миграторы — PASS; новые expiry/guard/lock tests — PASS.
- Admin: полный итог проверок ожидается.

После очистки Docker первое выполнение выявило ограничения тестового запуска:
120 секунд на холодную компиляцию paired и параллельные пакеты с общей reset DB
и пятисекундным advisory-lock timeout. Paired теперь имеет ограниченный
600-секундный startup; PostgreSQL-пакеты выполняются последовательно, а их
внутренние concurrent/race сценарии сохранены. Старый перегруженный unit-прогон
также превысил timing bound rate-limit теста; полный current-source прогон выше
прошёл без изменения этого теста и продуктовых ограничений.

Используются отдельные PostgreSQL и реальные private HTTP/HMAC обработчики;
CRM/SDK и браузерные данные — синтетические fixtures.

Живые зависимости РС-03.1.2, РС-08.3.2, РС-10.2.1/10.2.2/10.3.1 остаются
отдельной приёмкой. OAuth, установленный ZIP/SDK, настоящий PATCH и остановка
старого writer не выполнены этим ревью. Гейты G1–G6 сохраняются.
