# Этап 1: baseline и доказательства выполнения

Дата: 8 сентября 2026 года. Это отчёт подготовки, не отчёт о production-выпуске.

## Состояние выполнения

| Задача | Результат | Доказательство |
| --- | --- | --- |
| BASE-01: исходники и локальный runtime | Проверены; целевой удалённый сервер пока не указан | Этот отчёт, [local-runtime.json](local-runtime.json) |
| BASE-01: Issues | Прочитаны текущие статусы и checklist; составлено сопоставление | [issue-index.json](issue-index.json), таблица ниже |
| BASE-01: живой виджет | Зафиксирован предел доказательств скриншотов и прежнего отчёта | Раздел о виджете ниже |
| BASE-02: матрица и примеры | Результат находится в отдельной спецификации | [Матрица данных](../../specs/activity-data-contract.md), [синтетические fixtures](../../fixtures/activity-events-v1.json) |
| BASE-03: продуктовые решения | Рабочий scope первой версии без изменения действующих настроек | Та же спецификация |
| MOD-01: границы модулей | Новый ADR/runbook, автоматическое включение новых продуктовых каталогов в import guard | [ADR-0012](../../adr/0012-separable-product-module-contract.md), [шаблон](../../runbooks/new-product-module.md) |

## Исходники и GitHub

- Checkout `main`, HEAD `8c36827ef041561d2b2d734e5c686f36869812f8`; удалённый GitHub `main` на момент чтения совпадает.
- До этапа 1 runtime-код был чистым; неотслеживаемым был каталог `docs/plans/` с созданным в этой задаче планом.
- Последний CI этой ревизии завершён успешно: [run 34118666561](https://github.com/sk1fy/amocrm-pro/actions/runs/34118666561), создан 7 сентября 2026 года. Это результат baseline, а не CI новых изменений этапа 1.
- При проверке более старых отчётов не переатрибутировались их результаты текущему серверу или новым изменениям.
- Issues читались через `gh`, но не изменялись, не закрывались и не создавались. Сопоставление ниже позволяет агенту избежать повторной реализации уже закрытых частей.

## Локальный runtime

Источник: Docker-проект `amocrm-activity`, Compose `docker-compose.activity.yml`. Проверка read-only: контейнеры не пересоздавались, данные не менялись, миграции не применялись.

| Владелец | Контейнер | Наблюдение |
| --- | --- | --- |
| Core API | `amocrm-activity-api-1` | healthy |
| Core worker/Gateway | `amocrm-activity-worker-1` | healthy |
| Activity | `amocrm-activity-activity-1` | healthy |
| CRM Events | `amocrm-activity-crm-events-1` | healthy |
| PostgreSQL | `amocrm-activity-postgres-1` | healthy, образ `postgres:17-alpine` |

Точные image IDs, даты создания/старта, SHA-256 извлечённых бинарей и migration checksums сохранены в [local-runtime.json](local-runtime.json). У четырёх Go-бинарей build metadata сообщает `go1.25.12`, но не содержит VCS revision; OCI revision label также отсутствует. Поэтому точный исходный commit этих запущенных бинарей **не подтверждён**. Дата контейнера и его healthy не заменяют эту проверку.

| БД | Применено | Сравнение с исходниками |
| --- | --- | --- |
| Core `amocrm_core` | 000001–000011 | Все up-checksums совпадают |
| Activity `amocrm_activity` | 000001 | Up-checksum совпадает |
| CRM Events `amocrm_events` | 000001 | Up-checksum совпадает; **000002_retention_consistency отсутствует** |

Следовательно, локальный runtime не считается проверенным экземпляром всех исправлений аудита 7 сентября. Подтверждён конкретный пробел миграции; наличие/отсутствие остальных кодовых исправлений нельзя вывести только из возраста образа. Применение 000002 требует отдельного release-прохода с предусмотренной проверкой legacy retention; этап 1 этого не выполняет.

Целевой удалённый сервер пока не выбран. Нельзя считать локальные DSN, Compose, бинарные хеши или миграции доказательством его состояния. Для завершения соответствующих пунктов BASE-01 нужен SSH-алиас/адрес и каталог/Compose-проект целевого backend, затем те же read-only проверки.

## Сопоставление плана с актуальными Issues

| Задачи плана | Канонический контекст | Реально оставшаяся работа / расхождение |
| --- | --- | --- |
| BASE-01, весь план | [#12](https://github.com/sk1fy/amocrm-pro/issues/12), [#2](https://github.com/sk1fy/amocrm-pro/issues/2) | Программа открыта; новые подробные Activity slices отдельными Issues в прочитанном списке не представлены |
| BASE-02/03, EVT, ENR, ACT, UI | [#5](https://github.com/sk1fy/amocrm-pro/issues/5) закрыт, [#8](https://github.com/sk1fy/amocrm-pro/issues/8), [#10](https://github.com/sk1fy/amocrm-pro/issues/10) открыты | Готовый API client не переписывать. Checklist #8 про settings не описывает уже реализованные отдельные Activity settings; продуктовый остаток берётся из матрицы |
| MOD-01–03 | [#11](https://github.com/sk1fy/amocrm-pro/issues/11), [#13](https://github.com/sk1fy/amocrm-pro/issues/13) | Часть чеклистов отстаёт: roles, extraction contracts и ADR-0010/0011 уже существуют. Остаток — общий шаблон новых модулей и target deployment evidence |
| CORE-01 | [#4](https://github.com/sk1fy/amocrm-pro/issues/4), [#13](https://github.com/sk1fy/amocrm-pro/issues/13) | Сохранённая транзакция вокруг refresh — текущий остаток. Закрытые #25/#28 и concurrency/reauth tests не реализовывать второй раз |
| CORE-02 | [#9](https://github.com/sk1fy/amocrm-pro/issues/9), [#32](https://github.com/sk1fy/amocrm-pro/issues/32) | Rotate/unregister и полный uninstall остаются открыты. GET/POST/DELETE webhooks, disable, guards уже есть |
| CORE-03 | [#32](https://github.com/sk1fy/amocrm-pro/issues/32), [#21](https://github.com/sk1fy/amocrm-pro/issues/21) | Общий error contract и edge ingress остаются; Activity typed mapping и widget/webhook limiter уже есть |
| CORE-04, REL-02 | [#6](https://github.com/sk1fy/amocrm-pro/issues/6), [#13](https://github.com/sk1fy/amocrm-pro/issues/13), [#21](https://github.com/sk1fy/amocrm-pro/issues/21) | Shared limiter перед несколькими owners — решение, а не отсутствие текущего limiter; fair claiming уже в коде |
| CORE-05, REL-03 | [#21](https://github.com/sk1fy/amocrm-pro/issues/21), [#11](https://github.com/sk1fy/amocrm-pro/issues/11) | Финитный срок технической истории открыт. Bounded reaper #51/#53, gauges #49, webhook payload retention закрыты/реализованы |
| CORE-06, OPS | [#11](https://github.com/sk1fy/amocrm-pro/issues/11), [#13](https://github.com/sk1fy/amocrm-pro/issues/13) | Target KMS/rotation/restore/SLO не доказаны; существующее шифрование, роли и development certificates не отсутствуют |
| CORE-07, QA | [#55](https://github.com/sk1fy/amocrm-pro/issues/55), [#32](https://github.com/sk1fy/amocrm-pro/issues/32) | #55 открыт при выполненных auth/header acceptance. Свежий baseline CI уже success; живой E2E новых функций требует отдельного evidence |

Не закрывать epic только из-за наличия отдельной функции в коде: у него могут оставаться другие критерии. Подробные snapshots статусов и дат доступны в issue-index; текущий статус перед новым slice нужно перечитать.

## Что подтверждает существующий виджет

Постоянный тестер: `/Users/nikpeskov/Projects/sub-projects/amocrm-pro-service-2`, package version `0.4.0`; контрольные суммы локальных assets записаны в [tester-source.json](tester-source.json). Выделенный commit этого клиентского каталога получить не удалось: он находится в родительском репозитории без доступного HEAD. Версия локальных файлов не доказывает версию установленного ZIP.

Предоставленные скриншоты показывают: открылась панель Activity, отображаются сотрудники/отделы, зарегистрированные события, состояние сборщика, verified frontier, последнее успешное чтение и отсутствие показанной ошибки. Это свидетельство работающего браузерного чтения в одном аккаунте; оно не доказывает сценарий sync до succeeded, reauth, независимый сбор после закрытия окна, failover или полноту всего account history.

Прежний [отчёт 7 сентября](../activity-v0-hardening.md) уже фиксирует сообщение владельца об успешной проверке предыдущей версии в установленном виджете. Это не отменяется старым текстом «E2E не выполнено» в более ранних документах. Детальные тестовые сценарии следующего выпуска остаются в QA-02.

JSON относится к другому аккаунту. Он используется как пример глубины полей, без сравнения количества событий со скриншотами и без переноса персональных данных в fixtures.

## Проверки изменения этапа 1

Правка ограничена тестом архитектурных границ и документацией/синтетическими примерами. Новые endpoints, сборщик, миграции и клиентский функционал здесь не реализуются.

Итоговый целевой прогон в Docker `golang:1.25` завершился с exit 0: три верхнеуровневых теста, 16 подтестов, `-race`, без SKIP; затем `go vet ./internal/componentruntime` — exit 0. Лог: [boundaries.log](boundaries.log), сводка: [checks.json](checks.json).

Проверены `TestDomainDependencyBoundaries`, `TestNewProductDirectoriesCannotBypassBoundaries`, `TestStandaloneRejectsForeignConfiguration`. Новый guard автоматически обходит продуктовые каталоги, оставляя явное исключение для Core `leadstatus`; различает собственный подпакет и чужой пакет с похожим префиксом, отвергает Core facilities, прямые HTTP/socket обращения и transport/config imports. Проверены как запрещённые, так и разрешённые зависимости. Это проверка прямых импортов; runtime mTLS/DB guards не заменяются ею.

В fixture 29 уникальных синтетических событий, 11 категорий и 5 отдельных образцов будущего enrichment. Проверены валидный JSON, уникальные case/event IDs, пустой пользователь, `[]`, явный `null`, отсутствующий payload и локальные ссылки документов. Полный прогон fixture через PostgreSQL/RPC намеренно остаётся EVT-01 следующего этапа.

Воспроизводимая команда для изменённой проверки:

```sh
docker run --rm -v "$PWD:/src" \
  -v amocrm-pro-gomod:/go/pkg/mod \
  -v amocrm-pro-gobuild:/root/.cache/go-build \
  -w /src golang:1.25 sh -ec \
  'go test -race -count=1 -v ./internal/componentruntime -run "Test(DomainDependencyBoundaries|NewProductDirectoriesCannotBypassBoundaries|StandaloneRejectsForeignConfiguration)$"; go vet ./internal/componentruntime'
```

Проверки не используют runtime DB, не требуют сброса таблиц и не запускают новые сервисы. Новый remote CI, production deploy и целевой сервер этим результатом не подтверждаются.
