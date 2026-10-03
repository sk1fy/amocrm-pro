# РС-10: локальный стенд, режим наблюдения и подготовка пилота

Текущий результат — локальная реализация и fixtures. Живой запуск требует
[матрицы приёмки](../specs/lead-distribution-v1/11-local-acceptance.md) и G1–G6
из [спецификации пилота](../specs/lead-distribution-v1/03-pilot-and-acceptance.md).

## Совместимые версии и порядок подготовки

1. Сохранить известные совместимые артефакты Core, company/gateway, TeamOS,
   pinned widget bundle/ZIP и admin UI. API для совместимости создаёт правило
   с `executionMode=live`, если режим не указан; новая форма TeamOS предлагает
   `observe`. В обоих случаях новое правило неактивно (`active=false`).
2. Штатно применить Core миграции до 21 и company до 28. В company 28 сохраняет
   старую первую live-границу и ordinary pause; новые observe jobs/results
   отдельны от живой очереди. Used observation identities запрещают destructive
   down. Для отката приложения использовать совместимую версию, не migrate down.
3. Подготовить отдельные owner DBs, приватные адреса сервисов и HTTPS proxy.
   `DISTRIBUTION_TEAMOS_URL`, `DISTRIBUTION_HTTP_ADDRESS`,
   `DISTRIBUTION_TLS_PROXY`, `DISTRIBUTION_TEAMOS_KEY_ID`,
   `DISTRIBUTION_SERVICE_KEYS` и симметричная конфигурация Team bridge задаются
   серверным env/защищённым secret-file механизмом окружения. Значения секретов
   в инструкции, ZIP, browser storage и комментариях не сохраняются.
4. Проверить readiness/совместимость контрактов и явно provision нужные scoped
   capabilities. OAuth, webhook coverage и текущие mappings должны быть
   подтверждены на тестовом аккаунте. Core admin `service_authorized` отражает
   разрешение Core, а не успешное TLS/Team соединение.

## Наблюдение

Создать неактивное правило, выбрать `observe`, проверить point/timezone/members
и текущие mapping/grants, затем включить active для наблюдения. Дополнительно
Core installation pause позволяет закрыть новое assignment admission/dispatch,
оставив чтение, normalization, delivery и уточнение уже существующих эффектов.

План считается общей live/dry-run функцией по свежему CRM snapshot, графикам,
исключениям и текущему RR snapshot. Observe не резервирует следующий ход,
не двигает рабочий RR и не создаёт живую queue/claim/Core command. Два плана могут
иметь одного кандидата: это предварительный выбор без резервирования.

В TeamOS/виджете отдельно показываются актуальный ответственный и сохранённый
план на момент `crmObservedAt/checkedAt`. Нет operation ID и действий назначения
из строки наблюдения. Недоступный source — ошибка/unknown, не ноль. Доступ к
сделке проверяется свежим серверным `CanViewLead`; запрещённая строка целиком
скрывается. `hasMore` — продолжение физической страницы, не число доступных сделок.

Перевод `observe→live` выполняется CAS текущей rule revision. Mode flip запрещён
при любой unsettled живой queue, включая ещё не отправленный intent. Оператор
безопасно отменяет допустимые waiting rows отдельно; unknown не отменяется
удалением guard/ledger. Каждый flip создаёт новую epoch; вход в live фиксирует
новый monotonic `liveStartedAt`. История/first_activation_at не переписываются.
Observation rows, уже существующие сделки и старые события не дренируются после
включения. Требуются настоящие entry evidence и source times после границы.

Обычная pause/resume внутри `live` сохраняет принятую живую очередь и прежнюю
границу. Это другое действие, чем переключение observe/live.

## Переключение и остановка

До live включения оператор подтверждает выбранный account/pipeline/stage/group,
единственного владельца записи и список прежних in-flight/unknown операций.
Сначала остановить intake/исполнение старого worker и выяснить его эффекты;
не считать закрытую вкладку или выключенный UI-триггер остановкой сервера.
Существующие сделки показать только для чтения. Их обработка — отдельное
уполномоченное действие с конкретным набором; данный этап его автоматически
не выполняет.

Затем CAS режима/границы нового правила, разрешение Core admission и контрольные
новые сделки. Не расширять охват до приёмки графиков, ошибок, восстановления,
прав и согласованного интервала наблюдения. Реальная остановка `rakurs-ssd`
этим локальным стендом не выполняется и не подтверждается.

При остановке: pause новых Core admissions/dispatch и нужных Team rules,
сохранить intake/events/deliveries/history, выяснять уже отправленные effects.
Pause не возвращает ответственного автоматически. Старый writer разрешать
только после прекращения новых запросов нового writer и выяснения всех
релевантных unknown/guards. Длительный отказ требует сверки пропущенных событий,
а не нового случайного request ID и replay всех старых команд.

## Отрицательный lookup и истёкший frozen intent

При Core 404 локальный worker повторно проверяет lease, current mode/epoch,
active и срок решения. Истёкший intent остаётся
`requires_configuration / expired_never_admitted`, с теми же operation/request
IDs и claims. 404 не доказывает, что старый запрос не придёт/не commit позже.
Не удалять frozen intent, не создавать replacement ID автоматически.

Оператор проверяет точные существующие IDs и журналы обеих сторон, устраняет
доступность источника и выясняет поздние admission/effects через владельца.
Автоматическая retirement такого intent требует отдельного atomic negative
admission/tombstone протокола; в текущем этапе она не реализована. Ожидание
без такого доказательства — осознанное ограничение, а не успешное восстановление.

## Согласованная резервная копия и восстановление

Production backup должен охватывать обе owner DB в согласованной точке.
Перед копией остановить новых writers/worker ticks/сверки и дождаться окончания
DB/HTTP work; отдельно сохранить перечень unknown и immutable request IDs.
При восстановлении не отправлять новые эффекты до сверки двух восстановленных
состояний с фактом CRM. Сохранить guards, attempts/ACK, queue/mirror versions,
RR, scopes/mappings/epochs, receipts/outboxes и дедупликацию.

Локальный `make distribution-team-bridge-test` действительно использует
`pg_dump -Fc` и `pg_restore --clean --if-exists` обеих disposable DB, затем
Core `Pool.Reset` и новые Team service/HTTP handlers. Это восстановление в те же
два изолированных контейнера, не перенос на новый сервер. Внешний CRM fixture
не откатывается: после restore matching owner без ACK всё ещё unknown, guard
и frozen ID/RR сохраняются, второй PATCH не выполняется.

Offline coordinator `scripts/distribution-backup-fixture.py` не вызывается из
веб-запросов. Он pin проверяет точный Compose project Core и dedicated run label
Team testcontainer, не принимает произвольный DSN и сохраняет synthetic dumps
0600 в приватном ignored bridge directory. Workers здесь управляются вручную;
во время dump/restore нет ticks/HTTP writes. Shared/dev volumes не используются.

## Локальные проверки и другие окружения

- Core: `make fmt-check vet openapi-check`, focused Docker race tests и
  `make distribution-team-bridge-test` с соответствующими migration/test images.
- Team: `make gen`, помодульные `GOWORK=off` unit/race, company/gateway lint,
  `make check-contract FRONTEND_DIR=../team-os-rs07`, профиль company PG.
- TeamOS/widget/admin UI — обязательные проверки своих репозиториев.

Следующие варианты: сервер с CRM fixture для TLS/process/fault/restore проверок;
сервер с отдельным amoCRM тестовым аккаунтом для OAuth/SDK/webhook/PATCH и обоих
UI; локальные сервисы с тестовым аккаунтом через HTTPS tunnel для callbacks и
assets, с приватными admin/internal endpoints. Реальные секреты передаются
через механизм окружения сервера. Ни один из вариантов не подменяет G6.
