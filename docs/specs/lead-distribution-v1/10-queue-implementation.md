# РС-06: очередь, графики и выбор сотрудника

Дата: 02.10.2026. Бизнес-решение выполняет TeamOS company; Core сохраняет и
исполняет отдельную CRM-операцию. Ни браузер, ни виджет для обработки очереди не
нужны. Живая проверка amoCRM и приёмка развёртывания в этот этап не входят.

## Реализованные границы

В TeamOS миграция `000025_distribution_queue` добавляет настройки IANA timezone,
правила точки входа, очередь, claims группы/сделки, сохраняемый курсор,
версии входных данных и неизменяемую историю. Runtime API объявлен в
`team-os-backend/contracts/openapi/teamos.yaml`; gateway/protobuf/sqlc
генерируются из канонических контрактов. Настройки/правила меняют owner/admin,
чтение требует текущего доступа к разделу распределения и ограничено компанией.
Существующая локальная симуляция сохраняет отдельную семантику и не выдаётся за
реальную CRM-операцию.

Первое включение правила сохраняет `first_activation_at`: существовавшие до него
наблюдения не импортируются автоматически. Обе известные даты source occurred
и ingress received должны быть не раньше этого порога; NULL не заменяется датой
последующего GET/создания projection, в том числе при смешанном rollout workers.
Неизвестное время источника сохраняется как `source_time_unknown` и требует
настройки/проверки. Пауза/изменение/возобновление не
переставляют этот порог. Новые подходящие эпизоды во время паузы сохраняются;
старые события другого binding не становятся новыми кандидатами. Неоднозначный
вход из РС-05 не превращается в выдуманный эпизод. Обработка использует точные
account/pipeline/status/binding/revision, а не произвольный текст `source` группы.

## Доступность

Чистый домен `company/internal/domain/schedule/availability.go` использует недельный
и циклический график, гражданские даты компании и интервалы `[start,end)`.
Ночная смена допускает хвост предыдущей даты; явное нерабочее исключение текущей
даты обрывает его в полночь, обычный выходной шаблона хвост не обрывает. Отпуск,
больничный, off и trip не дают рабочей доступности; work переопределяет смену.
Равные начало/конец не означают круглосуточную смену и требуют исправления.

Часовой пояс задаётся явно. Отсутствующий/невалидный график или timezone не
означают доступность. Неоднозначные и отсутствующие локальные границы перехода
DST не выбираются молча: соответствующий интервал отклоняется. Поиск ближайшей
смены ограничен 35 следующими локальными календарными днями; пустой горизонт не
считается бесконечной сменой. Цикл вычисляется по календарным датам, а не
длительности локальных суток.

Участник также должен быть активен в TeamOS, не удалён внешней синхронизацией,
включён в группе, иметь verified CRM mapping с подтверждённой Core revision и
активный CRM ID в актуальном полном справочнике Core. API доступности возвращает
причину каждого исключения и ближайшую известную смену. Истечение справочника
или ошибка чтения запрещают автоматический выбор.

## Очередь и решение

Admission и wakes ограничены пакетами. PostgreSQL хранит `next_attempt_at`, lease,
причину и порядок `created_at,id`. Более поздняя сделка той же группы не обходит
старую готовую или уже занятую lease запись через `SKIP LOCKED`; будущая
не готовая запись без in-flight claim не блокирует остальные готовые эпизоды.
Другая группа обрабатывается
независимо. Lease перепроверяется после SQL locks; устаревший executor не меняет
состояние/историю нового владельца. Все внешние GET/POST находятся вне длительной
SQL транзакции.

Изменения графиков, исключений, сотрудников, группы, mapping, правила, timezone
и состояния подключения сохраняют wake revision в БД. Перезапуск не теряет ночь
ожидания или изменение данных. Пауза сохраняет очередь; выход/удаление/смена
binding отменяют устаревшего кандидата. Отправленная операция сначала уточняется:
отмена или истёкший lease не доказывают, что поздний внешний PATCH не придёт.

При выборе сохраняются exact scope, episode/rule/group/decision/operation UUID,
rule/availability/claim revisions, снимок источника и доступности, порядок
участников, `validUntil`, transport/control UUID. Полный assignment envelope
фиксируется **до** POST Core; при потере ACK повторяется та же команда с тем же
ключом, а GET восстанавливает уже принятую операцию. Короткое разрешение не
заменяется произвольными клиентскими revisions.

Внутри группы одновременно существует одна неподтверждённая операция. Claim
account+lead не обходится выходом/повторным входом или переходом в другую группу.
`unknown` удерживает оба claims до доказанного результата Core; timer, owner GET
или смена этапа сами их не освобождают.

## Round-robin и подтверждение

Выбор идёт после сохраняемой границы обхода по доступным участникам. При изменении
состава из прежнего циклического порядка берётся первый сохранившийся участник
после границы; если старых участников больше нет — первый нового порядка.
Редактирование состава само не расходует ход.

`keepCurrent=true` по умолчанию: доступный текущий владелец в группе сохраняется,
PATCH и движение курсора отсутствуют. При `keepCurrent=false`, если round-robin
выбрал текущего владельца, Core возвращает `no_change/already_target`: PATCH
отсутствует, но один подтверждённый ход расходуется. `no_change/kept` ход не
расходует. TeamOS понимает оба исхода как реальные результаты Core, а не ждёт
только `state=succeeded`.

Курсор и финальная история меняются одной транзакцией с применением trusted
operation result, ровно один раз. Push результата и GET/reconcile используют
одну проверенную operation identity/version. Старый результат другой попытки
остаётся историей и не подтверждает новую decision. До доказанной отправки
устаревшее решение можно пересчитать в том же эпизоде после освобождения Core
и Team claims; старая команда/причина остаются в immutable history.

Cancel/reconcile использует стабильную причину и ключ для точной версии результата:
UUID выводится из сохранённого control UUID и action/resultVersion. Повтор потерянного
ACK сохраняет payload, следующая версия не конфликтует со старой квитанцией.
Reconcile не отправляет PATCH. Сохранённый CRM ACK плюс последующий свежий GET
позволяют завершить `confirming`; без ACK matching owner оставляет `unknown`.

## Реальный grant перед эффектом

TeamOS signed `validate-decision` сверяет сохранённую decision, exact scope/actor,
все revisions, текущий episode/head, binding/mapping, claims, правило/паузу,
доступность и получателя. Неизвестная/устаревшая запись, отсутствующая capability
или настройка получают отказ. Разрешение связано с operationId/decisionId и
конкретным Core worker fence, не кешируется и ограничено минимумом из пяти секунд,
конца текущей смены и command expiry.

Core повторно проверяет активность CRM получателя и свежий CRM snapshot. Ручное
изменение owner/stage перед PATCH завершает старую команду без перезаписи. Dispatch
сохраняет intent до единственного PATCH и проверяет grant/lease после ожидания
locks и непосредственно перед отправкой.

В РС-06 усилено и завершение без PATCH: `FinishNoChange` проверяет короткий grant,
command expiry, cancel, current capability/binding, global guard и job lease
внутри транзакции после ожидания operation/guard locks. Истёкший grant не выпускает
cursor-confirming `no_change` result. Восстановление **уже отправленного** эффекта
с прежним durable ACK не требует нового бизнес-разрешения и остаётся РС-04.

Короткий grant — снимок полномочий, не распределённая SQL блокировка. Изменение
после успешной проверки может совпасть с уже начатым PATCH; CRM CAS/exactly-once
и атомарность графика с ручной CRM правкой не обещаются.

## Проверки и воспроизведение

Core обязательные проверки: `make test` (fmt/vet/full race/build) — PASS;
полный current PostgreSQL профиль из `make integration-current-test` повторён с
`GOFLAGS=-p=1` — PASS. Первый параллельный полный запуск встретил 5-секундный
advisory-lock timeout общего testkit в существующем leadstatus тесте; бизнес-
assertion не падал. Серийный запуск сохраняет все 18 пакетов и отдельную БД,
не меняет legacy код/тесты. Focused distribution PostgreSQL/race — PASS, включая
operation/guard lock wait, grant expiry, cancel и revoke перед `no_change`.


TeamOS проверки: `make gen`, all-module `make test`, `make check-contract`
(199 frontend calls) — PASS; Company lint — 0 issues. Полная PostgreSQL
application suite — 70.298 s и signed transport — 8.616 s до последнего
strict source cutoff delta. После strict known-source cutoff и immutable
control requests текущий focused PG/race — PASS (application 10.390 s,
signed transport 5.083 s); отдельный delta PG профиль — PASS (5.521 s).
Он проверяет ночь → перезапуск → начало смены/назначение, reload exact control
body/key/version и запрет его изменения, permanent pre-send failure и следующую
готовую сделку, FIFO/старый lease, владение точкой, old/NULL cutoff, admission/wake
205 entries с сохранением dirty revision между пакетами и две конкурирующие
SQL connections без deadlock чтения графика/его writer. Ночной сценарий —
отдельная проверка TeamOS, а не одна из девяти парных Core↔TeamOS проверок ниже.
Full baseline не объявляется повторённым после последнего delta: current focused
и delta профили проверяют эти изменения. Логи: `/tmp/rs06-team-final-pg.log`,
`/tmp/rs06-team-final-current-race-pg.log`, `/tmp/rs06-team-final-delta-pg.log`.

Парная проверка находится в Core
`internal/distribution/team_bridge_integration_test.go` и TeamOS
`services/company/internal/transport/distributionhttp/rs06_bridge_integration_test.go`.
Она запускает **два настоящих** private handlers/HTTP clients, HMAC/nonces, отдельные
PostgreSQL БД, Core jobs/worker и TeamOS registry/queue. Только amoCRM управляется
фикстурой; synthetic allow-token/подмена Team validator не используются.

Команда из Core: `make distribution-team-bridge-test`. Нужны Docker, sibling
TeamOS checkout (либо `TEAMOS_BACKEND_DIR`), актуальные matching migrator/test images
из `make integration-test`. Опционально `TEAMOS_GO_MOD_CACHE` указывает каталог
или Docker volume с исходниками модулей. Оба test processes используют Docker
host network одного daemon; TLS доверяет только выдаваемым тестовым сертификатам.
Синтетический секрет/сертификаты и coordination файлы/логи остаются в ignored
`tmp/distribution-bridge.*`; production token/env-файлы не читаются. Скрипт очищает
только свои контейнеры и одноразовую Core БД; Team Testcontainers очищает свою БД.
Без opt-in env мостовые тесты пропускаются, и такой skip не считается приёмкой.

Подтверждённый парный baseline: назначение и перезапуск; keep без расхода;
same-owner assign с одним ходом без PATCH; конкурентные события одной группы;
истечение реального Team grant во время Core guard wait и безопасное новое
разрешение; durable ACK/ошибка GET → reconcile без второго PATCH; unknown с
сохранением claims группы/сделки после перезапуска. Финальный negative профиль
также проверяет отключение CRM получателя после decision и ручную правку owner
перед отправкой. `make distribution-team-bridge-test` — PASS: все девять
сценариев TeamOS (22.84 s, package 23.883 s) и Core harness
(44.79 s, package 45.813 s).
Логи/coordination этого прогона: ignored `tmp/distribution-bridge.obqBIY`.
Истечение grant подтверждено точной ошибкой Core `decision expired or unavailable`,
нулём дополнительных PATCH и сохранённым queued result/guard перед новой попыткой.
После последних test/docs изменений текущий Docker fmt/vet/race/contract профиль
`./api ./internal/distribution` также PASS (2.386 s / 1.728 s).

## Трассировка задач

| Подзадача | Реализация и проверка |
| --- | --- |
| [РС-06.1.1](https://app.clickup.com/t/869faptky) | Domain availability: недельные/циклические графики, ночь, IANA/DST, границы дат и ограниченный next shift |
| [РС-06.1.2](https://app.clickup.com/t/869faptm2) | Исключения и current employee/mapping/CRM lifecycle, API причин, durable wake |
| [РС-06.2.1](https://app.clickup.com/t/869faptmv) | PostgreSQL queue/lease/next_attempt_at, restart, FIFO и внешние вызовы вне SQL locks |
| [РС-06.2.2](https://app.clickup.com/t/869faptn8) | Pause/resume/cancel/recalculate и append-only history; неизвестная отправка остаётся удержанной |
| [РС-06.3.1](https://app.clickup.com/t/869faptnq) | Current availability, keep и confirmed round-robin cursor; same-owner outcome и повтор результата |
| [РС-06.3.2](https://app.clickup.com/t/869faptny) | Per-group/account+lead claims, immutable decision, real signed grant, Core preconditions и парная PG/race проверка |

## Ограничения выпуска

- Живой amoCRM OAuth/reauth сценарий РС-03.1.2 остаётся отдельной незавершённой
  проверкой. Тестовые ACK/GET в мосте не свидетельствуют о выполненном live pilot.
- Production TLS/private routing, capability grants, корректные mappings/timezone,
  scope выбранного пилота и cutover старого распределителя требуют развёртывания
  и приёмки. Capability остаётся default-off без автоматических grants.
- TeamOS frontend, новый виджет и интерфейс Core admin относятся к следующим
  этапам; данный этап добавляет серверный механизм и канонический API.
- Неполные исторические CRM события не становятся доказанными входами; recovery
  из РС-05 сохраняет честные gaps. Автоматический выпуск неизвестного guard по
  таймеру/GET не добавлен.
