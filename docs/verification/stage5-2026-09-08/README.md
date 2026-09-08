# Этап 5: виджет и подробный журнал

База: `8c36827ef041561d2b2d734e5c686f36869812f8` плюс сохранённые изменения этапов 1–4.
Дата: 8 сентября 2026 года. Это локальный отчёт реализации, не production deploy и не живой E2E amoCRM.

**После ревью:** исправлены возврат к выбранным сотрудникам и обновление
карточек после ошибок/ожидания enrichment. По указанию владельца пресеты
заканчиваются в **23:59:50**. Собран ZIP **0.5.1**, пересобран Activity-сайт.
Итог: 174 Go PASS с `-race`, 3 штатных helper SKIP, UI 21 PASS / 0 SKIP;
сайт 32 PASS, тестер 36 PASS. [Исправления и логи](fixes.md).
Ниже сохранён первоначальный результат этапа до исправлений ревью.

Новых owner-миграций и изменений Go-контракта нет. Публичные URL панели и карточки
остались `/panel` и `/events/{id}`.

## Что изменилось

| Задача | Результат |
| --- | --- |
| UI-01 | Сотрудники и отделы выбираются по имени из `panel.users`. Пресеты «Сегодня»/«Вчера» и datetime-local — в timezone аккаунта; неизвестный пояс показан явно. Сводка сохраняет нулевых сотрудников и показывает категории / завершённые задачи. Переход к сотруднику и назад сохраняет период и фильтры; устаревший ответ панели игнорируется |
| UI-02 | Журнал: время, автор, русский `view.title`, сущность. Карточка раскрывается отдельно: до/после, тексты, `source`/`current`. Compact-страница без B/A; `GET /events/{id}` один раз при открытии. Ссылки только для lead/contact/company/customer. Состояния loading/empty/error/partial/unavailable разделены. Обновление не сбрасывает открытую карточку и не дублирует id |
| UI-03 | Короткий статус — coverage, freshness, `empty_reason`. Окна, страницы, лаг, коды ошибок, verification и internals операции — в свёрнутой «Диагностике». Polling 12×5с, повтор с тем же Idempotency-Key и `succeeded` как единственный успех сохранены |

Поверхности: исходный адаптер `examples/activity-v0`; Backend Tester 2 — vendor/AMD из этого адаптера, версия виджета **0.5.0**, ZIP `dist/widget.zip`; Activity-сайт расширен без второго timeline. Allowlist тестера и сайта допускает `GET /api/v1/widget/activity/events/{eventID}` (`^[A-Za-z0-9_.:-]{1,128}$`, без query).

## Использование

```text
GET /api/v1/widget/activity/panel?from=...&to=...&compact=true&group_id=...&categories=tasks,calls
GET /api/v1/widget/activity/events/{eventID}
```

Журнал запрашивает compact. Карточка — полный Event с `view.details`. Период
включительный `[from,to]` в unix; календарные пресеты считаются в timezone
аккаунта после первого ответа панели.

## Проверки и границы доказательств

Итоговый gate:

| Проверка | Результат | Артефакт |
| --- | --- | --- |
| `make ... activity-ci` | exit 0; 174 верхнеуровневых Go PASS, 3 штатных helper SKIP; `-race`; UI 16 PASS / 0 SKIP | [Go log](activity-go.txt), [UI log](activity-ui.txt) |
| Activity-сайт `npm test` / `npm run check` | 29 PASS / 0 FAIL | вне этого репозитория |
| Backend Tester 2 `npm run check` / `npm run pack` | 36 PASS / 0 FAIL; ZIP собран | вне этого репозитория |

Compose-проект `amocrm-stage5-test`, БД `*_test`. Runtime-проект не менялся.

Ключевые сценарии UI:

- compact + `group_id` + `categories`, без `actor_id` и `include_unknown_authors`;
- «Сегодня»/«Вчера» в `Europe/Moscow` и UTC;
- ссылки только на lead/contact/company/customer;
- устаревший ответ панели не перезаписывает новый фильтр;
- открытая карточка переживает refresh; detail запрашивается один раз;
- unavailable и 404 не выдаются за пустую историю;
- нулевые сотрудники directory остаются;
- короткий статус не содержит окна/лага сборщика;
- операция `succeeded` останавливает poll и обновляет панель один раз.

## Что не выполнялось

- Живой amoCRM, OAuth SDK, установка ZIP в аккаунт и приёмка пилота (этап 8)
- Production deploy
- История членства отдела
- Timeline buckets в технической панели (корзины остаются данными API / сайта)
- Повторная реализация UI в vendor/AMD вручную

## Команды воспроизведения

```sh
node --test examples/activity-v0/panel.test.mjs examples/activity-v0/panel.operation.test.mjs
make ACTIVITY_TEST_PROJECT=amocrm-stage5-test activity-ci
```

Для тестера: скопировать `examples/activity-v0/panel.mjs` и `panel.css` в
`vendor/activity-v0/`, затем `npm run activity-assets && npm run pack`.
Owner-миграции этапа 5 нет.
