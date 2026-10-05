# ADR-0036: завершение истёкшей команды без принятого назначения

Статус: Accepted locally. Дата: 04.10.2026.

## Контекст

После потери ответа admission TeamOS хранит frozen command и claims сделки
и группы. Истечение validUntil и GET 404 не доказывают отсутствие позднего
admission. Ранее этот сценарий навсегда занимал группу даже без CRM-запроса.
ADR-0034 продолжает определять доказательства для возможного внешнего эффекта.

## Решение

Private `POST /internal/v1/distribution/assignments/expire` принимает исходный
envelope и Idempotency-Key. HMAC и scoped service grant обязательны. Core берёт
те же locks binding/key и operation ID, что admission, проверяет immutable hash
и всю историческую binding. Найденную операцию возвращает без изменения.

Если операции нет, срок обязан истечь по часам БД. В одной транзакции сохраняются
terminal rejected/no_attempt/no_request_sent, неизменяемый receipt, result,
outbox и аудит. Для существующего NOT NULL job FK создаётся сразу cancelled
job; он никогда не виден исполнителю как queued. Guard и попытка PATCH отсутствуют.
Pause, revoked binding/capability не запрещают это завершение без эффекта.
Изменение frozen payload конфликтует; продлить срок старой команды нельзя.

Поздний admission возвращает terminal receipt. TeamOS валидирует всю identity,
версию и evidence результата и снимает только связанные claims, не продвигая
round-robin. Существующий unknown не завершается таймером или совпавшим GET.

## Отклонённые варианты

- Локальное снятие claims по 404/таймеру: не сериализует задержанный admission.
- Повторный PATCH или новый ID: теряет защиту от двойного назначения.
- Принудительное объявление unknown успешным: нет доказательства авторства.

## Последствия

Новая миграция не требуется. Сначала выпускается Core, затем TeamOS backend.
Старый Core возвращает 404; TeamOS сохраняет неопределённость и claims до
совместимого ответа. Terminal records, receipts и outbox сохраняются действующей
политикой retention. Живая OAuth/SDK/CRM-приёмка остаётся отдельным условием пилота.
