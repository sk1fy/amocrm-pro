# Исправления по аудиту этапов 1–5

Дата: 8 сентября 2026 года. Рабочее дерево поверх
`8c36827ef041561d2b2d734e5c686f36869812f8`. Production/runtime не менялись.

## Изменения

- Временные ошибки больше не заканчиваются необратимым error. После пяти
  быстрых попыток объект остаётся retry/temporary с повтором раз в 10 минут.
  Счётчик насыщается на MaxAttempts. ReauthRequired ожидает восстановления
  авторизации с интервалом один час. Успех сбрасывает счётчик.
- Миграция `000006_enrichment_retry_recovery` возвращает прежние
  error/temporary в очередь; error/invalid не затрагивает. Down не воссоздаёт
  застрявшие состояния.
- Notes/Tasks/Entities изолируют известные ID с превышением лимитов контента.
  Gateway возвращает корректные объекты и отдельный `invalid_ids`; owner
  сохраняет unavailable/invalid только для этих ID. Контент не обрезается
  и не выдаётся как ready; 49 корректных объектов не теряются из-за одного
  oversized. Неожиданный ID и структурно неполный ответ остаются ошибкой
  всего вызова. Дополнительных внешних запросов для деления батча нет.
- Claim admission защищён transaction advisory lock на installation_id и
  повторной проверкой активного object lease. Collector source lease не
  используется. Сохранены срок lease и fencing token при финализации.
- Старые временные репро-тесты этапов 3/4 переименованы в `.go.txt` и больше
  не участвуют в `go vet ./...`. Команды воспроизведения обновлены.
- checks.json этапа 5 указывает последний прогон 0.5.1/21 UI/32 сайта;
  исходный snapshot сохранён как initial-checks.json. Пресеты 23:59:50
  сохранены согласно указанию владельца.

Wire: additive `invalid_ids=2` в NotePage/TaskPage/EntityCatalog. Generated
protobuf и conversion пересобраны штатно: protoc 31.1, protoc-gen-go 1.36.8,
protoc-gen-go-grpc 1.5.1, generate-adapters.py. Публичный HTTP/OpenAPI не
меняется: ID ошибок внутреннего Gateway представлены в существующем
sidecar state/reason_code.

## Проверки

До исправления наличие трёх проблем подтверждено тестами:
[лог наблюдений](before-fixes.txt). PASS в этом старом логе означает
подтверждение дефекта, а не отсутствие ошибок.

После исправлений [целевые тесты](targeted.txt) прошли с `-race -p 1`:
serviceapi, CRM Events с PostgreSQL, Gateway, amoCRM и protobuf round-trip.
Новые сценарии проверяют медленное восстановление, отсутствие раннего повтора,
атомарность claim, восстановление истёкшего lease и отказ старому worker,
частичный батч 49/1, миграцию застрявших объектов, период reauth и invalid_ids
для всех трёх методов через wire.

`go vet ./...` с примонтированным рабочим деревом и `gofmt -l .` — PASS.

Итоговый gate:

```sh
make COMPOSE=docker-compose ACTIVITY_TEST_PROJECT=amocrm-audit-fixes-test activity-ci
```

Exit 0: **182 верхнеуровневых Go PASS с `-race`, 3 штатных helper SKIP;
UI 21 PASS / 0 SKIP**. Проверены owner PostgreSQL, mTLS, реальные процессы,
аварийные перезапуски, embedded/gRPC и общий бюджет. Необязательная проверка
с предыдущим API-бинарём пропущена: COMPONENT_PROCESS_PREVIOUS_API_BINARY не
задан. Тестовый проект и volumes удалены штатным cleanup.

Артефакты: [Go](activity-go.txt), [UI](activity-ui.txt), [Make/Compose](activity-ci.txt),
[SHA-256 исходников](source-manifest.json), [машинная сводка](checks.json).
`git diff --check` — PASS.

## Выпуск и ограничения

Применить owner-миграцию 000006 и обновить Gateway + CRM Events. Старый owner
не превратит invalid объект в пустой ready: он увидит отсутствие объекта как
not_found. Точное invalid-состояние требует обновления обеих сторон. Старый
Gateway может по-прежнему отклонять весь батч; после обновления owner это
не создаёт терминального временного error.

Дополнительные индексы и квоты refresh без измерений не вводились. Различение
deleted/missing_in_source уточнено в ADR-0014: эти коды зарезервированы,
историческое действие удаления сохраняется в событии, а один 404 не доказывает
удаление объекта. Живой amoCRM, production rollout и нагрузочный REL-02 не
выполнялись. Пересборка ZIP/сайта для этих backend-исправлений не требуется.
