# Исправления ревью этапа 9

11.09.2026. База `d7cb8c7` плюс рабочее дерево этапов 8–9. Шесть замечаний
ревью исправлены в коде; результаты проверок приведены ниже. Браузер не
использовался: ручная приёмка оставлена пользователю. TeamOS не менялся.

| Замечание | Изменение | Проверка |
| --- | --- | --- |
| Неограниченный limiter | Общий бюджет до lookup, cap 4096, TTL 5 минут; сохранён per-key budget | 10 000 разных ключей, cap, cleanup и HTTP 429 до обработчика |
| Пустой PATCH через gRPC | Явные has_name/has_employees/has_window в protobuf; удалено восстановление presence по значению | Реальный mTLS: пустое имя/состав/окно → InvalidArgument; корректный PATCH сохраняется |
| enabled вне hash | В canonical request включён нормализованный bool | PostgreSQL: изменённый enabled с тем же command ID → Conflict |
| Delegation переживает rotate | Версия ключа в IssueRequest, JWT и Principal; Activity сверяет её с БД | Старый токен → NotFound, новый → PASS; отдельно PostgreSQL rotate и disable |
| Пустые таймлайны общего экрана | Сохранение data.events, пагинация overview, дедупликация событий, totals остаются серверными | Node: реальная форма DTO, страницы и renderer до раскрытия сотрудника |
| Неверный PG replay test | Повторяется исходный command ID, проверяются ID панели и отсутствие повторной выдачи ключа | Полный owner DB test доходит до isolation/rotate/disable |

Protobuf и конвертации сгенерированы protoc 31.1, protoc-gen-go 1.36.8,
protoc-gen-go-grpc 1.5.1 и `generate-adapters.py`; генератор теперь запускает
gofmt. Публичные HTTP DTO не расширены внутренней версией ключа. Новых
миграций нет: используется `view_key_version` из `000002_panels`.

При выпуске новой модели delegation обновить Core API, Gateway/policy и
Activity согласованно. Старые viewer delegation без версии не принимаются.
В прототипе до ревью hash create не учитывал enabled; записи такого старого
эксперимента при повторе create могут вернуть Conflict. Они не объявляются
совместимым выпущенным контрактом; исходный отчёт не подтверждал deployment
этапа 9. Сохранённые панели и ключи читаются без миграции.

Код сайта: `/Users/nikpeskov/Projects/sub-projects/amocrm-pro-activity-site`.
Изменены `src/viewer-api.mjs`, `src/app.mjs` и Node-тесты; `npm run check`
пересобирает штатный `dist`.

Открыты ручная приёмка и оставшиеся пункты APP-03/APP-05. Известный race
лимита 50 панелей и планировщик автообновления не входят в эти шесть правок.
Не выполнялись production deploy, включение пилота и физический multi-host.

## Проверки

- `make ACTIVITY_TEST_PROJECT=amocrm-stage9-fixes-test activity-ci`: exit 0,
  **244 Go PASS / 0 FAIL**, 3 служебных helper SKIP, `-race`; **25 UI PASS**.
  Включены PostgreSQL owners, mTLS, реальные процессы и stage8 backup/rules.
  Optional previous API binary не задан; сравнение со старым бинарём не выполнялось.
- Сайт `npm run check`: **51 PASS / 0 FAIL / 0 SKIP**, `dist` пересобран.
- `make test`: exit 0 — Docker build, gofmt, go vet и `go test -race ./...`.
  DB/process fixtures без окружения в этом unit-gate пропускаются; фактические
  owner DB/process проверки подтверждены отдельным Activity gate выше.
- Целевые Go regression-тесты: PASS; их исправления затем проверены общим gate.
- `git diff --check`, gofmt и ссылки отчёта проверены.

Артефакты: [Activity gate](activity-ci.txt), [Go](activity-go.txt),
[UI](activity-ui.txt), [целевые тесты](targeted.txt), [сайт](site-check.txt),
[общий make test](make-test.txt), [SHA-256 исходников](source-sha256.json).

Тестовый Compose-проект очищен вместе с его БД. Runtime `amocrm-activity`
не менялся. Команды для повторения:

```sh
make ACTIVITY_TEST_PROJECT=amocrm-stage9-fixes-test activity-ci
make test
# в проекте amocrm-pro-activity-site:
npm run check
```
