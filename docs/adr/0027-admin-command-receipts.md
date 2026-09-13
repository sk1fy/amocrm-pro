# ADR-0027: durable admin commands и диагностика через worker

- **Status:** Accepted.
- **Date:** 2026-09-13.

Дополняет ADR-0017, ADR-0019 и ADR-0025. Контракт админки отделён от
публичного widget/OAuth API и доверяет Bearer-аутентификации внутреннего
listener; права сотрудника проверяет Admin API перед отправкой команды.

## Контекст

Повтор запроса, потеря ответа и перезапуск Admin API не должны повторять
изменение Core. Секрет интеграции нельзя сохранять в очереди или журнале
операции. Проверка подключения и удаление webhook требуют amoCRM HTTP и
должны использовать существующий исходящий бюджет worker/Gateway.

## Решение

`POST /admin/v1/commands` принимает объект, команду и типизированный payload.
`GET /admin/v1/commands/{id}` только читает квитанцию. UUID из
`Idempotency-Key` становится ID квитанции; произвольный ключ преобразуется
в UUIDv5 с namespace OID. Поэтому Admin API знает ID до отправки и после
обрыва восстанавливает исход чтением, не повторяя запрос с секретом.

Миграция `000016_admin_commands` добавляет квитанции с хешами ключа и
канонического запроса. Ключ глобален между сотрудниками и объектами.
Повтор идентичного запроса возвращает исходную квитанцию; изменение
payload/команды/объекта даёт conflict. Actor не входит в хеш: инициатором
остаётся первый сотрудник. Открытый payload и секрет не сохраняются.
Квитанции сохраняются независимо от семисуточного retention jobs.

Ключ и объект сериализуются advisory locks. На объекте может оставаться
только одна незавершённая квитанция. Прочие команды получают conflict.
Для синхронных команд изменение, native audit и квитанция коммитятся одной
транзакцией. Ошибка прикладной функции откатывает savepoint и сохраняет
безопасную failed-квитанцию с аудитом отказа. Предварительная валидация и
конфликт ключа/занятого объекта возвращают HTTP error envelope.

Прикладной SQL не копируется в admincommand:

- CLI `integrations.Store.Apply` и admin `ApplyTx` используют одну функцию;
- `SetPilotTx`/`RetryDeliveryTx` используются и существующими CLI wrappers;
- ручной reconcile ставит существующий job `webhook.reconcile`;
- `jobs.RetrySafeTx` сохраняет исходный job ID, payload и actor.

Native audit и аудит квитанций используют `actor_type='admin'`, `actor_id`
из `X-Admin-Actor`. Секреты и входные тела в audit не попадают.

### Внешние вызовы

`check` ставит `admin.connection_check`; `uninstall` сначала фиксирует
локальный `uninstalled`, затем в той же транзакции ставит
`admin.webhook_unregister`. Оба возвращают pending-квитанцию и `job_id`.
В jobs сохраняется только ID квитанции, не входной payload.

Worker выполняет `GET /api/v4/account` через общий `amocrm.Client`.
Диагностика завершается состоянием succeeded, а её результат содержит
`classification`/`verification`: `verified_ok`, `auth_error`,
`network_error`, `rate_limited`, `internal_error`; `observed_at` и
`retry_after` в секундах. Ошибка авторизации — подтверждённый результат
проверки, а не сбой механизма операций.

Uninstall использует существующий `webhook.Unregister` и scoped OAuth
provider. `WithTokenProvider` сохраняет общий limiter/HTTP/metrics клиента
Gateway и не создаёт вторую квоту. Ошибка снятия webhook возвращает partial
с безопасным `webhook_error`; новый явно подтверждённый uninstall может
повторить конвергентное снятие подписки. Потерянный OAuth refresh требует
восстановления результата refresh либо повторной авторизации.

Внешнему job даётся одна автоматическая попытка. Worker завершает job,
квитанцию и audit одной транзакцией через completion observer после
обычного fence по worker/attempt/lease. Потеря процесса/lease без
подтверждённого результата даёт `unknown_outcome`, без автоматического
повторного HTTP. GET квитанции показывает running из processing-job,
не производя запись и не выполняя команду.

### Безопасный retry

Разрешены только failed/dead jobs двух типов:

| Тип | Основание |
| --- | --- |
| `webhook.reconcile` | Сверка и приведение подписок к желаемому состоянию |
| `widget.ping` | Нет мутации amoCRM, audit дедуплицирован по исходному job ID |

Установка и интеграция должны быть active. Actor/payload/job ID и номера
попыток сохраняются; `max_attempts=attempts+5` добавляет ограниченный бюджет.
`retry_allowed` и `retry_reason` в read API используют то же правило.
Lead-status, parse/process_event и прочие типы остаются только для чтения:
произвольный ручной replay не утверждён для их state fences и retention.

Retry Activity сохраняет исходный command ID и receiver idempotency,
семисуточный горизонт и проверяет `payload.installation_id` до изменения.

## Отклонённые варианты

- Повтор HTTP после обрыва: опасен и требует повторного хранения секрета.
- Выполнение amoCRM HTTP внутри API: создаёт второй исходящий бюджет.
- SQL-транзакция на время HTTP: нарушает refresh/lock границы ADR-0017.
- Любой job можно повторить: безопасная семантика существует не для всех.

## Последствия

Сначала применяют миграцию, затем обновляют worker с новыми handlers и
только затем API с command routes. Это исключает приём новых job старым
worker. Миграция обратима; её down удаляет журнал квитанций и не применяется
к общим данным без отдельного подтверждения.

`PUBLIC_BASE_URL` в API задаёт origin OAuth start URL. Если он отсутствует,
используется origin зарегистрированного HTTPS redirect URI; для relay
redirect deployment обязан задавать явный `PUBLIC_BASE_URL`.

Публичные маршруты и существующие CLI-команды сохраняют поведение. Admin
commands требуют отдельного внутреннего listener. Jobs read API по-прежнему
не раскрывает произвольные `payload`/`result`; результат диагностики доступен
через квитанцию.
