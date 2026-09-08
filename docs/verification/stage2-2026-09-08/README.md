# Этап 2: сохранность событий и подробное чтение

База: `8c36827ef041561d2b2d734e5c686f36869812f8` плюс сохранённые изменения этапа 1.
Дата: 8 сентября 2026 года. Это локальный отчёт реализации, не production deploy.

## Что изменилось

| Задача | Результат |
| --- | --- |
| EVT-01 | Все 29 синтетических сценариев проходят HTTP decoder, Gateway и owner PostgreSQL; отдельно проверены RPC/Activity/HTTP, update и дедупликация |
| EVT-02 | Сохраняется `linked_talk_contact_id`; лимиты проверяет также owner. Полный raw архив и карантин не добавляются. Есть opt-in compact projection и полное detail-чтение |
| EVT-03 | Types/prefix/entity filters, asc/desc, cursor v2, одинаковая фильтрация страницы и общих summaries, scoped detail, отказ старых peers для новых options/cursor |

Точный контракт, ограничения и порядок выпуска: [ADR-0013](../../adr/0013-event-detail-read-contract.md).
Обновлённая матрица: [спецификация](../../specs/activity-data-contract.md).
Входы fixture не менялись; ожидание сохранения chat contact дополнено результатом
этапа 2 и прежним baseline: [fixture](../../fixtures/activity-events-v1.json).

## Использование API

Каждый запрос по-прежнему требует новую авторизацию существующего виджета;
tenant/actor определяет Core. Примеры ниже — только пути и параметры, без токенов.

```text
GET /api/v1/widget/activity/panel?from=1788847200&to=1788850800&user_ids=71001&types=task_completed&type_prefix=custom_field_&order=desc&compact=true
GET /api/v1/widget/activity/panel?from=1788847200&to=1788850800&entity_type=lead&entity_ids=31001,31002
GET /api/v1/widget/activity/events/synthetic-chat_reference
```

`types` и `type_prefix` — OR; остальные фильтры — AND. В compact ответе обе стороны
payload равны null и `data.payloads_omitted=true`. Это указание на проекцию, а не на
значения исходного события. Detail возвращает полный payload из CRM Events DB.
Старые запросы без новых options/cursor сохраняют прежнее поведение.

Курсор привязан к scope и нормализованным фильтрам, порядку и compact; limit менять
можно. Смена фильтра требует первой страницы. Старые cursors отвергаются. Между
страницами нет snapshot-гарантии: позднее событие позади позиции требует обновления
списка. Фильтр по префиксу не подменяет будущую классификацию Activity.

## Проверки и границы доказательств

Запуски выполняются через Docker/Make, в отдельном Compose-проекте
`amocrm-stage2-test`, на БД `core_components_test`, `activity_components_test`,
`events_components_test`. Он не использует runtime DB. Стек и volumes удаляются
штатным cleanup после проверки.

Итоговые gates завершились успешно:

| Проверка | Результат | Артефакт |
| --- | --- | --- |
| `make ... activity-ci` | exit 0; 115 верхнеуровневых Go PASS, 3 штатных helper SKIP; `-race`, все обязательные owner/RPC/process случаи выполнены | [Go log](activity-go.txt) |
| UI operation tests | 5 PASS, 0 SKIP | [UI log](activity-ui.txt) |
| `make test` | exit 0; gofmt, vet, race suite и Docker-сборки; DB-зависимые cases этого gate отдельно покрыты Activity gate в его scope | [Make log](make-test.txt) |
| Изолированный impact test | PASS без изменения порогов; idle P95 6.50 ms, loaded P95 5.95 ms; local fixture, не production SLO | Go log |

[Сводка проверок](checks.json) и [SHA-256 manifest исходников](source-manifest.json)
позволяют связать результат с рабочим деревом. Три SKIP — helper entrypoints,
которые запускаются их process controllers. Числа разных suite не складываются.
`activity-ci` удалил собственные контейнеры/volumes; runtime-проект не менялся.

Ключевые сценарии:

- HTTP fixture decoder → настоящий Gateway → collector → JSONB → Query для 29
  событий, включая Unicode, точные числа, неизвестные поля/типы, null/[]/missing;
- повтор ID, изменение contact ID и payload, число уникальных событий без роста;
- B/A ровно 32 768 байт и превышение, oversized upstream response, отклонение всей
  страницы без записи/coverage;
- 100 больших сохранённых событий: компактная страница укладывается в бюджет,
  полный список явно превышает его, одна detail-карточка сохраняет полные данные;
- одинаковое время, asc/desc, совпадающие counts на всех страницах, type/prefix OR,
  scope и filter binding, поздняя вставка, удаление anchor и snapshot при retention;
- публичные Core HTTP handlers с реальным owner и mTLS-путём, детали всех 29
  событий, явный неизвестный положительный actor и нулевой сотрудник;
- отказ чужому tenant и после policy revocation; точный mTLS ACL нового GetEvent;
- capability marker старого peer, прежняя RPC service descriptor без GetEvent,
  повреждённый query и continuation к старому peer;
- прежние process fault/mode-switch и UI operation tests.

В составных HTTP-тестах middleware principal подаётся тестовым адаптером, а
внешний amoCRM заменён синтетическим HTTP transport/API. Это подтверждает путь
данных/owner/RPC/HTTP, но не настоящий OAuth/Web SDK и не установленный ZIP.
Отдельного запуска сохранённого старого бинаря API не было; mixed-version
семантика проверена специальными peers/descriptors, прежние defaults — контрактами.

Первый общий `activity-ci` обнаружил ожидаемый regression: стандартный URL.Query
терял некорректный filter pair. Исправлено на строгий ParseQuery; добавлены проверки
некорректного percent encoding и semicolon. Отдельно устранён rollback corner:
любая непустая continuation теперь требует read_version=2. Эти промежуточные
результаты не выдаются за финальный PASS.

Во втором общем прогоне функциональные проверки прошли, но существующий process
impact test превысил порог относительной задержки ping: idle P95 около 20 ms,
loaded P95 около 228 ms, порог max(3×idle, 100 ms). Одновременно выполнялся
`make test` с Docker-компиляцией. Это не доказательство причины и не PASS;
после завершения сборки выполнен отдельный повтор без конкурентной сборки.
Порог теста и production-код ради этого результата не изменялись.

## Что не выполнялось

- Миграции runtime/production и обновление серверных контейнеров. Новая 000003
  применялась только в изолированной тестовой среде.
- Новые Notes/Task/field/pipeline reads, имена через Events `with`, raw storage,
  карантин, новый collector или копия истории в Activity.
- Новые продуктовые категории, первое событие и расширенные KPI, изменение
  coverage, исправление default directory-фильтра неизвестных авторов — дальнейшие
  задачи плана. Владелец хранит такие события, detail читает их по ID.
- Изменение Backend Tester 2, bridge allowlist/ZIP или Activity-сайта. Новые URL
  доступны в backend, их пользовательская интеграция остаётся этапом UI/пилота.
- Production SLO, произвольное горизонтальное масштабирование и полный реальный
  multi-host перенос. Сохраняется существующая модульность.

## Команды воспроизведения

```sh
make COMPOSE=docker-compose ACTIVITY_TEST_PROJECT=amocrm-stage2-test activity-ci
make test
```

Protobuf сгенерирован `protoc 31.1` (заголовок 6.31.1), `protoc-gen-go v1.36.8`,
`protoc-gen-go-grpc v1.5.1`; преобразования — `api/proto/generate-adapters.py`.
Номера старых полей не менялись. Новая schema должна быть применена владельцем
до запуска нового CRM Events; read_version не подтверждает, что старый Gateway
ранее сохранил contact ID. Gateway/owner/Activity/Core нужно обновлять совместимо,
с остановкой смешанных collectors и предусмотренным rollback.
