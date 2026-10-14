# ADR-0033: защищённое подключение распределения к TeamOS

Статус: Proposed. Дата: 02.10.2026.

## Решение, реализованное для этапа РС-03

Core сохраняет OAuth и общий amoCRM budget в worker. Выделенный private HTTP
listener worker обслуживает только подключение, справочники и проверки доступа
распределения. Он использует существующий Client worker; второй OAuth provider
или limiter в API не создаётся. Режим включается явно и независимо от Activity.
Capability `lead-distribution` выдаётся оператором через существующий
`integrations set-service`; существующие установки не получают её автоматически.

Вместо предложенного в РС-02 mTLS/RPC для связи с TeamOS реализован HTTP поверх
HTTPS с подписанием каждого запроса HMAC-SHA256, отдельным key ID и явно выданным
company/installation grant. Подпись включает key ID, method, URI с query,
company, installation, timestamp, nonce и SHA256 точных body bytes. Core хранит
nonce в PostgreSQL, поэтому повтор запрещён между репликами. Timestamp ±60 секунд,
retention nonce 5 минут, очистка ограничена 100 строками за запрос. Повтор транспорта
получает новый nonce и сохраняет бизнес intent ID. Конфигурация допускает до восьми
ключей для периода ротации. Удаление старого ключа и отключение grant закрывают его
доступ. Ни один серверный ключ не передаётся браузеру.

Привязка требует двух независимых полномочий: владелец/администратор TeamOS
создаёт pending intent; Core проверяет scoped service grant и текущего
администратора amoCRM по одноразовому проверенному widget JWT. JTI и binding
фиксируются одной транзакцией; потерянный ответ восстанавливается GET той же
binding. Intent действует не дольше 15 минут. Отмена ставит tombstone под тем же
company advisory lock и запрещает поздний confirm. Старые revoked bindings и
сопоставления сохраняются; уникальность требуется только для active binding.

Права на просмотр сделки вычисляются сервером: общие права A/G/M/D, затем override
прав на текущий статус, затем расширение через user/group подписчика сделки.
Role права читаются отдельно. Недоступный источник или неизвестная модель прав
не дают доступ. Виджет дополнительно обращается обратно в TeamOS за текущей
привязкой, активным employee и section access; employee ID сверяется с Core mirror.
TeamOS пользователь не выбирает произвольный CRM actor: подписывающий backend
проверяет локальный principal и сохранённое соответствие. Mirror mappings имеют
монотонную версию и hash полного снимка, чтобы задержанный запрос не переписал
новое соответствие.

## Ограничения

TLS завершает доверенный reverse proxy; listener нельзя публиковать напрямую.
Флаг `DISTRIBUTION_TLS_PROXY=true` обозначает проверенную deployment configuration,
а не создаёт TLS. Deployment evidence и реальный widget/OAuth тест на аккаунте
остаются обязательными перед пилотом. Это ещё не RPC mTLS и не реализация очереди
или назначения ответственного. Будущие внешние эффекты обязаны повторно проверять
capability и текущую binding перед mutation; модуль соединения не содержит PATCH.

## Источники политики

- [amoCRM пользователи, роли и порядок расчёта прав](https://www.amocrm.ru/developers/content/crm_platform/users-api).
- [amoCRM подписчики сущности](https://new.amocrm.ru/developers/content/crm_platform/subscriptions-api).

## Пакетное чтение прав (исправление QA12, 14.10.2026)

Private `POST /internal/v1/distribution/bindings/{bindingId}/permissions/batch`
принимает actor из проверенного TeamOS mapping и до 100 уникальных lead ID.
Нельзя передавать actor из браузера. Подпись и scope такие же, как у single-lead
permissions; capability, active binding и mapping проверяются до и после чтения.
Любой неизвестный источник закрывает всю страницу. Actor, role и lead snapshots
повторно используются только внутри этого запроса и этой установки. Assignment
worker продолжает отдельные свежие pre/post проверки, без такого reuse.

Core читает actor и пакет lead snapshots параллельно. Пакет exact-ID использует
[документированный filter[id]](https://www.amocrm.ru/developers/content/crm_platform/filters-api).
На аккаунтах без фильтрации только явные HTTP 400/402/422 допускают возврат к
точным single-lead GET с максимум четырьмя workers и тем же общим deadline.
401/403/429, timeout, неполный ответ и чужие строки не вызывают fallback.
Одна сделка читается обычным GET без требования фильтрации.

TeamOS группирует страницу по binding/revision/account, дедуплицирует lead ID и
проверяет локальные actor/access/mapping/revision fences после network call.
Статистика и наблюдения сохраняют общий бюджет 3 секунды; неопределённость не
становится нулевой статистикой. Detail использует метаданные того же CRM snapshot,
на котором рассчитаны права, вместо дополнительного read+permission roundtrip.
Никаких TTL-кэшей положительных прав между запросами нет.

Развёртывать Core первым, затем TeamOS. Новый TeamOS поддерживает старый Core:
только HTTP 404 отсутствующего batch endpoint запускает bounded single-lead
fallback в том же бюджете; permission и transport errors не обходятся. Если CRM
не поддерживает exact-ID фильтрацию, больших страниц при высокой latency всё ещё
может не хватить на 3 секунды; источник остаётся явно unavailable и fail closed.
