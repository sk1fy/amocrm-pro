# Исправления повторного аудита этапа 6

Дата: 10 сентября 2026. База `dac12af8e1b8ef16fbac5e137c7238eaa0db335d` плюс
незакоммиченная реализация этапа 6 и эти исправления. Runtime/production не менялись.

## Исправлено

1. **Поздний replay и откат настроек.** CRM Events сохраняет inbox и operations
   после GC завершённых jobs. Прежний command ID возвращает прежний результат;
   изменённый payload сохраняет Conflict. Квитанции пока без TTL: Core допускает
   произвольный простой исполнителя и операторский retry. Конечное хранение
   command identity остаётся REL-03/CORE-05, а не объявляется безопасным по
   одной лишь границе в семь суток.
2. **Исчезновение paused backfill.** Paused jobs исключены из GC. При reauth,
   потере прав и disable сохраняются исходный диапазон, checkpoint и связи.
   После нового sync задание возобновляется. Оба сценария ревью перенесены
   в постоянные регрессионные тесты с контролем без GC и проверкой с GC.
3. **L-03: полная транспортная цепочка.** Новый тест использует PostgreSQL,
   штатные gRPC/mTLS адаптеры CRM Events и Activity, штатный HTTP handler и
   loopback HTTP-клиент. Измеряется время до полного чтения тела ответа и размер
   реальных generated protobuf-ответов. Копии conversion удалены из старого
   benchmark. Локальные Panel/EventCard тоже измеряются сериями с сериализацией.
4. **L-01: работа при поступлении событий.** Пять установок обрабатываются
   штатным Service.Run с двумя workers. Общий mock Gateway ограничен 20
   запросами/секунду, задержка ответа 20ms. Выполняются текущий сбор, backfill
   и refresh; фиксируются реальные вызовы, бюджет ожидания, lag и время до
   готовности деталей, а также серия метрик очередей.
5. **Дополнительный дефект, найденный нагрузкой.** SQL ClaimEnrichment при
   200 ожидающих объектах возвращал 200 вместо 50: PostgreSQL повторно вычислял
   locking subquery при UPDATE. Это превращало batch в `unavailable/invalid`
   ещё до внешнего запроса. Выборка вынесена в `WITH picked AS MATERIALIZED`.
   Отдельная регрессия доказывает лимит 50; worker-профиль проверяет готовность
   всех backfill-объектов, а не только исчезновение из due-очереди.

ADR и runbook обновлены. Неопубликованная owner-миграция 000007 содержит индекс
completed/failed jobs; индекс удаляемых операций убран, поскольку они сохраняются.
Других миграций и изменений Core retry-протокола нет.

## Нагрузочные результаты

Критерии записаны [до прогона](criteria.md). Стенд: Docker aarch64, 2 CPU,
4 094 447 616 байт RAM; PostgreSQL 17, лимит 512 MiB. Полный профиль выполнен
отдельно от общего gate, без `-race`; это локальная синтетика, не production SLO.
[Полный лог](load.txt), [owner/SQL результаты](rel-02/summary.json).

**L-03:** 3 прогрева + 25 samples каждого сценария, реальные сокеты и mTLS.

| Сценарий | HTTP | Размер body | P95 / P99 |
| --- | --- | --- | --- |
| Non-compact, limit=48 | 200 | 3 098 539 B | 69.47 / 118.49 ms |
| Следующий limit=49 | 429 resource_exhausted | 40 B | 29.02 / 31.65 ms |
| Non-compact, limit=100 | 429 resource_exhausted | 40 B | 46.06 / 52.88 ms |
| Compact, limit=100 | 200 | 46 452 B | 1.87 / 2.36 ms |
| Одна большая карточка | 200 | 128 609 B | 2.19 / 2.38 ms |

Успешный near-bound owner protobuf: 3 078 680 B; Activity protobuf: 3 085 728 B.
Ни успешного ответа сверх лимита, ни тихого усечения нет.
[transport.json](rel-02/transport.json).

**L-01:** шесть волн по пять новых событий, 1000 backfill-событий, пять refresh
объектов. Завершены все пять backfills; готовы все 30 новых деталей и все
backfill-объекты. Выполнены 50 Events и 38 Tasks запросов, из них пять содержали
refresh (13.16% Tasks запросов); всего получено 1035 task-объектов. Детали:
P95/P99 2.885s; исходное событие P95 2.226s / P99 2.332s. Максимальный lag 4.262s;
ожидание общего бюджета P95 76.40ms / P99 78.40ms. Весь профиль — 6.83s.
[workers.json с серией метрик](rel-02/workers.json).

Пороги не повышались. Redis, новые индексы чтения и агрегаты не добавлены.

## Проверки и промежуточные результаты

- Регрессии GC проходят с очисткой и без неё; исходный replay-тест теперь
  требует сохранения operation/inbox после GC jobs.
- [Первый целевой прогон](targeted-first.txt): транспортный тест неверно
  ожидал gRPC status вместо нормализованного `serviceapi.Error`. Исправлена
  проверка теста; [целевой транспортный повтор с -race](transport-targeted.txt) PASS.
- [Промежуточная сборка benchmark](load-compile-first.txt): после удаления копий
  conversion был удалён импорт, ещё нужный для MaxMessageSize. Импорт восстановлен.
- [Первый полный профиль](load-first.txt) нашёл ошибку batch 200/50.
  [Регрессия до исправления](enrichment-batch-before.txt) FAIL;
  [после исправления и рабочая нагрузка](workers-after.txt) PASS.
- [Окончательный полный профиль](load.txt): все три верхнеуровневых теста PASS,
  33.55s, ни один порог не превышен.
- `make ACTIVITY_TEST_PROJECT=amocrm-stage6-fix-test activity-ci`: **PASS,
  210 верхнеуровневых Go PASS с -race, 3 служебных helper SKIP; UI 25 PASS**.
  [Команда и вывод](activity-ci.txt), [Go](activity-go.txt), [UI](activity-ui.txt).
  Process fault/mode switch тесты выполнены; необязательный предыдущий бинарь
  (`COMPONENT_PROCESS_PREVIOUS_API_BINARY`) не задавался, actual old-binary
  сравнение не заявляется. [Временный стенд удалён](cleanup.txt).
- `make test`: **PASS** — сборки backend-бинарей, проверка gofmt, `go vet ./...`
  и `go test -race -count=1 ./...` в Docker. [Лог](make-test.txt). Этот gate
  не подменяет owner/process интеграционные проверки выше: БД в нём не задана.

Проверенные owner-исходники и миграции: [SHA-256 manifest](source-manifest.json).

## Воспроизведение

Общий gate выполняется в отдельном Compose-проекте и удаляет его после проверки:

```sh
make ACTIVITY_TEST_PROJECT=amocrm-stage6-fix-test activity-ci
make test
```

Для полного измерения сначала подготовить выделенный PostgreSQL/образ:

```sh
docker-compose -p amocrm-stage6-fix-test --profile tests \
  -f docker-compose.activity.yml -f docker-compose.activity-tests.yml \
  up --detach --wait postgres
docker-compose -p amocrm-stage6-fix-test --profile tests \
  -f docker-compose.activity.yml -f docker-compose.activity-tests.yml \
  exec -T postgres sh < deploy/activity/init-tests.sh
docker-compose -p amocrm-stage6-fix-test --profile tests \
  -f docker-compose.activity.yml -f docker-compose.activity-tests.yml \
  build component-tests
docker-compose -p amocrm-stage6-fix-test --profile tests \
  -f docker-compose.activity.yml -f docker-compose.activity-tests.yml \
  run --rm --no-deps -e STAGE6_REL02_MEASURE=true \
  -e STAGE6_REL02_ARTIFACT_DIR=/src/tmp/stage6-remeasure \
  --entrypoint go component-tests test -count=1 -timeout=5m -v \
  ./internal/services/crmevents \
  -run '^TestStage6(ReadMeasure|TransportMeasure|WorkersUnderSharedBudget)$'
docker-compose -p amocrm-stage6-fix-test --profile tests \
  -f docker-compose.activity.yml -f docker-compose.activity-tests.yml \
  down --volumes --remove-orphans
```

Для сервера по-прежнему нужны согласованные owner-миграция 000007 и бинарь
CRM Events. Квитанции, уже удалённые ошибочной версией GC, автоматически не
восстанавливаются. В этой задаче ошибочный GC не запускался на runtime/production.
Полностью закрывать этап 6 нельзя до решения остатка REL-03/CORE-05.
