# ADR-0034: сохраняемая операция назначения и доказательства внешнего результата

Статус: Proposed. Дата: 02.10.2026. Реализованный runtime scope РС-04 описан в
[спецификации 08](../specs/lead-distribution-v1/08-assignment-implementation.md).

Core атомарно сохраняет command/receipt/job, глобальный account+lead guard и
append-only result/outbox. Scheduler lease не является TTL внешнего guard.
Перед одним PATCH сохраняется dispatch intent; uncertain takeover не повторяет
запись. Success требует persisted valid response и observation, начатого после
response_finished_at с source updated_at не старше ACK. Наблюдение само по себе
не является evidence авторства или завершения возможного позднего запроса.

HTTP private API использует signed HTTPS из ADR-0033. Admission возвращает 202
receipt; control cancel/reconcile возвращают 200 Operation, сохраняют actor/reason
и серверное время. Это реализованное уточнение proposed РС-02, где controls были
202 и cancel input включал requestedAt. Namespace binding+action+UUID key устойчив
к смене operation URL, operation ID включён в immutable control hash.

Операторский resolve с непроверяемыми self-reported claims не добавляется.
Неопределённость удерживается, даже при matching GET или HTTP error, пока нет
валидного сохранённого ACK и достаточного observation evidence. Пилотные jobs и
replay identities не стираются общим семидневным cleaner. Использованная миграция
не откатывается обычным down.

Business authority остаётся в TeamOS. Current decision-validation seam fails
closed до реализации rule/episode/availability/claim registry в РС-06. Это не
разрешение заменять его callback, всегда возвращающим true. Push доставка outbox
и live CRM пилот относятся к последующим этапам. GET→PATCH не обеспечивает atomic
CAS и не защищает от всех ручных правок в окне между чтением и записью.
