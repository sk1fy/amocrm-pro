# РС-09: диагностика и восстановление распределения

Работа через внутренний Core admin API и раздел распределения в карточке
подключения amocrm-pro-admin. Реальные OAuth/SDK/CRM ещё не проверены: текущая
приёмка использует изолированные PostgreSQL и синтетические CRM fixtures.

## Сценарий оператора

1. Найти установку, открыть «Распределение». Проверить время наблюдения,
   capability, отдельный pause, авторизацию, подписку, binding/mapping.
   `module_enabled` — configured capability в активном scope; не доказательство
   работающего TeamOS endpoint. `service_authorized` отражает только записанный
   Core grant. TLS/HMAC/runtime connectivity требуют отдельной проверки.
   `team_queue_state=unknown`: бизнес-очередь и правила смотреть в TeamOS.
2. Сравнить backlog событий/результатов и oldest pending с SLO пилота. Пустой
   успешный источник означает ноль, недоступный источник — ошибку/unknown.
   Ошибка обновления не должна превращать предыдущие значения в новые данные.
3. Искать UUID event/message/request/decision/operation/correlation/consumer.
   Открыть цепочку consumer → event → operation → result. Произвольные CRM
   payload и секреты недоступны. Список — newest first, keyset, максимум 100.
4. При OAuth auth_error использовать «Проверить подключение» (`check`). Receipt
   может завершиться с classification auth_error/network_error: это результат
   проверки, а не исправление подключения. Если нужна переавторизация — пройти
   обычный OAuth процесс. Не отправлять секреты через комментарии.
5. При неполной подписке выполнить `reconcile`: receipt возвращает queued job;
   затем перечитать webhook_status/events/checked_at и job. Успех постановки
   задачи не равен подтверждённой подписке amoCRM.
6. При blocked delivery сначала исправить cause (TLS, grant, schema, endpoint).
   Повторить точный `message_id` с текущими attempts. Frozen payload/UUID
   сохраняются; предыдущие attempts записаны в receipt. Не повторять
   acknowledged/delivering и не менять envelope вручную.
7. При outcome_unknown выполнить «Проверить результат» (`distribution-reconcile`)
   с operation UUID и текущей result_version. Ответ 202 — durable receipt.
   Дождаться command result и перечитать operation. Matching CRM owner без ACK
   остаётся outcome_unknown; guard не освобождается. Не повторять PATCH, не
   удалять guard/ledger SQL. Истёкшая версия → перечитать, сравнить, новая
   осознанная команда с новым ключом.
8. «Приостановить» останавливает новые Core назначения данной установки.
   Нормализация, доставка и проверка существующих эффектов продолжаются.
   Уже допущенный сетевой запрос может завершиться. Правила TeamOS отдельно
   поставить на паузу через TeamOS, если требуется остановить бизнес-решения.
   Для resume подтвердить текущий paused=true, перечитать после receipt.

## Метрики и пределы

`amocrm_distribution_delivery_messages{kind,state}` — durable delivery backlog.
`amocrm_distribution_delivery_oldest_seconds{kind}` — возраст oldest unacknowledged.
`amocrm_distribution_unfinished_operations{state}` — незавершённые Core operations.
`amocrm_distribution_operation_errors{class}` — безопасные ограниченные классы ошибок.
`amocrm_distribution_recovery_gaps{state}` — явные historical transition gaps.
`amocrm_distribution_consumer_receipts{consumer,state}` — normalization disposition.
`amocrm_distribution_delivery_collection_available` — доступность источника метрик;
при 0 предыдущие gauges не считаются свежими. Labels содержат только ограниченные
виды/состояния, без account/user/installation/operation IDs. Порог задержки
определяется SLO пилота. Это не TeamOS queue length.

## Воспроизводимая локальная проверка

`make fmt-check vet test openapi-check integration-test`. Изолированный тестовый
compose-project, не dev/pilot volumes. Новые tests: scoped search каждого UUID,
keyset, безопасный schema, actor-bound replay, атомарный pause/audit, сохранение
frozen delivery, pause перед dispatch, unknown+guard без второго PATCH.
Admin UI проверяется через обязательный `make check` своего репозитория.

## Проверка в других окружениях

- Тестовый сервер + CRM fixture: приватный admin listener, HTTPS proxy, отдельные
  owner DBs, apply migrations, fault injection задержки/timeout/invalid ACK,
  restart worker между receipt и outcome. Сравнить UI, audit и metrics; реальные
  OAuth/CRM этим не подтверждаются.
- Тестовый сервер + отдельный amoCRM аккаунт: тестовый домен/integration,
  синтетические сделки и 2–3 сотрудника. Проверить OAuth/reauthorization, complete
  webhook subscription, настоящий event → Team decision → PATCH ACK → GET → result,
  admin role restrictions и обработку неизвестного исхода. Production данные
  и production scope не подключать для демонстрации.
- Локальные сервисы + тестовый amoCRM через HTTPS tunnel: публичны только
  callback/webhook/widget assets; admin/internal endpoints остаются приватными.
  Зафиксировать ограничения доступности ноутбука и повторить restart/network tests.

Live проверка, пилот и cutover относятся к следующей приёмке; локальная
демонстрация не разрешает перенос рабочего распределителя.
