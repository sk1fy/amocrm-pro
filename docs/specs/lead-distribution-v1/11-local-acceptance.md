# РС-10: локальная приёмка и оставшиеся проверки пилота

Дата: 2026-10-03. Область: локальные изменения этапа 10 в ветках
`codex/lead-distribution-stage-10`. Это доказательства локальной реализации,
а не завершённый пилот amoCRM. Основная матрица —
[03-pilot-and-acceptance.md](03-pilot-and-acceptance.md).

## Среды и происхождение данных

- Paired profile: настоящие Core private HTTP/API/store/worker и Team company
  service/private HTTP, HTTPS/HMAC и две отдельные disposable PostgreSQL.
  Входные события и CRM — синтетические fixtures. Виджетный API проходит
  настоящий SDK verification/CORS/JTI pipeline с синтетически подписанным JWT;
  токен не получен от amoCRM, ZIP не установлен в живую amoCRM.
- Company PG profile: настоящий SQL, owner application, блокировки и миграции;
  Core является контролируемым adapter fixture. Чистые schedule tests проверяют
  даты, ночные интервалы, overrides и DST без сетевых эффектов.
- TeamOS и widget browser E2E: реальные собранные интерфейсы со своими SDK/API
  fixtures. Их нельзя считать одновременной работой двух браузеров на paired DB.
  Paired проверяет совпадение native Team read и widget API на одном настоящем
  сохранённом наблюдении. Это проверка API, а не браузерного совместного сценария.

## Матрица 1–16

Во всех строках live-колонка остаётся обязательной перед допуском G1–G6.
Локальное покрытие не переименовывает fixture-названия в реальные CRM результаты.

| № | Локальное доказательство и уровень | Что остаётся на тестовом сервере/аккаунте |
|---|---|---|
| 1 | Paired `actual_assignment_and_durable_restart`: Core worker, ACK/GET, Team result и один RR turn | Настоящий CRM owner и оба браузерных UI |
| 2 | Paired signed `lead.created` и source PG `TestSourceIngressSnapshotIsolationAndFrozenOutbox`; Company entry admission | Настоящий webhook создания сразу на этапе |
| 3 | Paired `keep_has_no_patch_and_no_turn`, `same_owner_assign_confirms_exactly_one_turn`; shared `TestChooseDistributionPlanSharedKeepOrderAndWaiting` | Оба значения настройки в установленном виджете и TeamOS |
| 4 | Company PG `TestDistributionNightWaitSurvivesRestartAndConfirms`; pure schedule/availability tests; dry-run общий chooser | Реальные графики, ночной переход и отсутствие графика в обоих UI |
| 5 | Company PG восстанавливает ночное ожидание; paired `rs10_actual_two_owner_backup_restore_reconnect_preserves_unknown` делает настоящий dump/restore обоих владельцев и reconnect | Остановка/старт процессов целевого сервера и наблюдение смены без браузера |
| 6 | Core PG `TestAssignmentAtomicConcurrentReplayAndSemanticConflict`; paired concurrent events и observation exact duplicate; stable command IDs | Повторы настоящих webhook/доставок, потеря ACK в целевой сети |
| 7 | Company delivery PG проверяет projection generation/reentry/старые события; Core `TestObservationOrderingAndDocumentedAbsence` | Выход/повторный вход через amoCRM и оба UI |
| 8 | Paired `unknown_outcome_holds_group_and_lead_after_restart`, `durable_ack_reconciles_without_second_patch`; настоящий двухвладельческий restore сохраняет unknown/guard/frozen ID/RR | Управляемый сетевой сбой после настоящего PATCH, без слепого повтора |
| 9 | Company observed-entry cancellation и Core observation absence/deletion tests; поздний эффект остаётся отдельной operation | Удаление/выход на настоящем аккаунте во время ожидания и in-flight |
| 10 | Company durable availability wake/queue PG и schedule tests; общий dry-run/live chooser | Правки графика/отпуска/состава из UI при работающем сервере |
| 11 | Paired concurrent group turns; Company `TestDistributionQueueDurableOrderLeaseWakeHistory`; `TestDistributionModeChangeSerializesAdmissionAndPendingObservationPoint` | Пилотная конкуренция/нагрузка и конфликты двух реальных настроек |
| 12 | Paired `manual_crm_owner_change_before_patch_is_not_overwritten`; Core success/conflict/unknown PG | Ручная смена в amoCRM до и во время вызова; отсутствие CAS явно сохраняется |
| 13 | Core/Company durable delivery, immutable outbox, leases и recovery PG; backup/reconnect profile | Сбой каждого процесса/сети, потеря результата и последующее восстановление |
| 14 | Widget lifecycle/abort/reopen и TeamOS polling browser fixtures; серверные workers работают независимо от UI | Закрытие и повторное открытие установленного ZIP/TeamOS на настоящем аккаунте |
| 15 | Paired recipient deactivation и grant expiry under lock; Core dispatch fence; Company pause/mode/epoch/expired-404 gate PG | Настоящее отключение capability/подключения и остановка пилота |
| 16 | Core ACL/scope/auth PG; Company observation скрывает всю запрещённую строку, отклоняет чужую компанию и возвращает ошибку при неизвестном permission source; UI role fixtures | Живые пользователи обоих интерфейсов и текущие права amoCRM |

Исходники: [paired Core fixture](../../../internal/distribution/team_bridge_integration_test.go),
[coordinator](../../../scripts/distribution-team-bridge-test.sh),
[Core assignment tests](../../../internal/distribution/assignment_worker_integration_test.go).
Company тесты находятся в соседнем `team-os-backend/services/company/internal/application/`:
`distribution_observation_integration_test.go`, `distribution_queue_integration_test.go`,
`distribution_night_integration_test.go`, `distribution_delivery_integration_test.go`.
Профиль paired Company — `internal/transport/distributionhttp/rs06_bridge_integration_test.go`.

## Отдельные доказательства режима наблюдения

`rs10_observation_scope_has_zero_effects_and_fresh_enable_boundary` проверяет:
один неизменяемый план после дубликата; ноль новых Core operations/assignment jobs
и PATCH, отсутствие живой очереди/claims выбранного observe-rule; одинаковый ID
плана через native Team read и widget API; после настоящего mode CAS старое
наблюдение не исполняется, а новый вход после границы создаёт одно назначение.
Состояние настоящего ответственного читается отдельно через `currentLead`.

Company PG дополнительно проверяет default-off, отсутствие создания/изменения
рабочего RR, приватность, failed permission source, fresh epoch/floor,
неизменяемость истории, запрет flip с незавершённой очередью, две очередности
rule/admission locks и защиту point edit при pending observation job.
Отрицательный Core lookup не удаляет frozen intent или claims: истёкший запрос
остаётся `requires_configuration / expired_never_admitted`.

## Ограничения допуска

G1, живая часть G3/G5 и G6 ещё не подтверждены. Нет настоящего OAuth/SDK-token,
установки ZIP, серверных адресов, данных старого worker и реального переключения.
Offline legacy-writer fixture проверяет порядок stop/drain/enable и запрет
возврата при unknown, но не управляет `rakurs-ssd` и не доказывает его остановку.

Секундная точность CRM source timestamps консервативно исключает события той же
секунды, если они не доказывают вход после микросекундной live-границы. Свежий
GET не заменяет source entry evidence. Нельзя перематывать floor ради приёмки
или отправлять старые наблюдения как новые события.

Команды проверки и безопасная эксплуатация:
[runbook пилота](../../runbooks/lead-distribution-pilot.md).
