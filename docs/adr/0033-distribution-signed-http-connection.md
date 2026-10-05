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
