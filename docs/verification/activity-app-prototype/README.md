# Этап 9: прототип панелей Activity и подготовка TeamOS

Дата: 11 сентября 2026. Локальная реализация APP-01–04 и контрактные
проверки APP-05. **Не** production deploy, **не** живой amoCRM, **не**
целевой сервер, **не** вкладка TeamOS.

**Актуализация после ревью:** исправления шести замечаний, проверки и
границы готовности приведены в [fixes/README.md](fixes/README.md).
Ниже сохранён исходный отчёт реализации. Ручная браузерная приёмка
оставлена пользователю; она не подменяется автоматическими тестами.

Контракты: [ADR-0024](../../adr/0024-activity-share-panels.md),
[activity-panel-v1.md](../../specs/activity-panel-v1.md),
[инструкция TeamOS](../../specs/team-os-activity-v1-integration.md).

## Что изменилось

| Задача | Результат |
| --- | --- |
| APP-01 | ADR-0024, mapping полей, viewer/operator principals, HTTP на Core |
| APP-02 | Миграция `migrations/activity/000002_panels`, management/viewer API, CLI `panel-*`, proto/OpenAPI |
| APP-03 | Activity-сайт `/#/p/{viewKey}` без widget JWT / opener / site-session |
| APP-04 | Инструкция новой вкладки `/activity-control-v2`; код TeamOS не менялся |
| APP-05 | Контрактные unit/HTTP/parity и UI-тесты; Postgres owner-тест есть, здесь SKIP |

## Локальный запуск

Backend (compose Activity, порт API `127.0.0.1:18080`):

```sh
# env: ACTIVITY_APP_ORIGINS, ACTIVITY_APP_PUBLIC_ORIGIN, ACTIVITY_MANAGEMENT_TOKEN
make activity-up   # или activity-embedded
```

Seed панели через management API, не SQL:

```sh
export API_BASE_URL=http://127.0.0.1:18080
export ACTIVITY_MANAGEMENT_TOKEN=activity-dev-management-token
activity-control panel-create INSTALLATION_UUID INTEGRATION_UUID \
  --name "Смена А" --employees 7,9 --from 09:00 --to 18:00
```

Прототип:

```sh
cd /Users/nikpeskov/Projects/sub-projects/amocrm-pro-activity-site
ACTIVITY_API_ORIGIN=http://127.0.0.1:18080 npm start
# открыть share_url из ответа create, обычно http://127.0.0.1:4173/#/p/{viewKey}
```

Fixture без Core (не доказательство пилота): `ACTIVITY_SITE_FIXTURE=1 npm start`.

## Проверки этой сессии

| Проверка | Результат |
| --- | --- |
| `go test ./internal/serviceapi ./internal/corepolicy ./internal/services/activity ./internal/activitybridge ./internal/servicerpc ./internal/componentruntime ./internal/services ./cmd/activity-control ./api -count=1` | PASS |
| `TestPostgresPanelsIsolationRotateDisableAndViewerDTO` | SKIP без `ACTIVITY_TEST_DATABASE_URL` |
| `TestCreatePanelAndViewTimelineLocalAndMTLSParity` | PASS (in-memory repo) |
| `TestTwoPanelsShareHistoryWithoutDuplicatingEvents` | PASS (одна история, разные ключи) |
| `TestViewerAndOperatorIssueSkipRoleLookup` | PASS |
| `TestShareHTTPRejectsCrossCredentialAndUnknownKey` | PASS |
| сайт `npm test` | 49 PASS / 0 FAIL / 0 SKIP |
| TeamOS `git status` | чистый `main`, файлов этапа 9 нет |
| Brave / живой collector при закрытом приложении | не выполнялось |
| `make activity-ci` | не запускался (занят runtime compose) |

## Сценарии, которые покрыты тестами

- Две панели, пересекающиеся сотрудники: независимые id/ключи; событие из общей истории не дублируется в CRM Events.
- Чужой installation не видит панель (`not_found`) — в Postgres-тесте, SKIP здесь.
- Неизвестный ключ, ключ на management URL, management token на viewer URL.
- Сотрудник и событие вне состава панели → `404`.
- Отключение панели → `404` просмотра; rotate инвалидирует старый ключ.
- Revision mismatch → `409`.
- Повтор create с тем же Idempotency-Key не создаёт вторую панель; сырой ключ в БД не хранится и при replay не возвращается.
- Viewer DTO без `view_key` / revision / share_url.
- Viewer/operator не делают amoCRM role lookup; widget admin-only сохранён.
- CORS: пустой `ACTIVITY_APP_ORIGINS` fail-closed; неизвестный Origin 403.
- UI: итоги из `data.totals`, не из длины страницы; `no_events` ≠ `unverified_empty` ≠ 404 ссылки.

## Не решено и не выдавать за готовое

- Живой Brave walkthrough, OAuth SDK, пилот на сервере.
- Полный `activity-ci` / restart-durable Postgres в этой сессии.
- Фильтр воронок, двухминутные корзины, Telegram, «эффективность».
- Вкладка TeamOS (отдельный scope).
- Лимит 50 панелей проверяется в транзакции без table lock; два одновременных create теоретически могут превысить.
- Replay create/rotate не возвращает секрет повторно (намеренно, ADR-0024).
- Сообщение 401 виджета по-прежнему «widget authentication required»; viewer HTTP пишет «authentication required».
- `GET /employees` отклоняет directory >100.
- Widget-tester путь сайта всё ещё может проксировать `/api/v1/site/*`; share-link этот путь не использует.

Секреты и живые `share_url` в этот отчёт не входят. Синтетический пример ключа в OpenAPI: `synthetic_view_key_not_real`.
