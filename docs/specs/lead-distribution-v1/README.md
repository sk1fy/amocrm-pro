# Распределение сделок через amocrm-pro

Требования первой версии подготовлены 01.10.2026. Локальная реализация
РС-03–РС-10 и независимое ревью описаны отдельными документами ниже.
Проектная спецификация и локальные проверки не подтверждают production.

## Требования и проектные контракты

- [Бизнес-правила](01-business-rules.md).
- [Архитектура и доступ](02-architecture-and-access.md).
- [Пилот и приёмка](03-pilot-and-acceptance.md).
- [Модель данных и lifecycle](04-data-model-and-lifecycle.md).
- [API и delivery](05-api-and-delivery-contracts.md).
- [Сценарии интерфейса](06-interface-scenarios.md).
- [Проектные контракты](contracts/) и [макеты](mockups/index.html).

## Реализация и проверки

- [Подключение Core ↔ TeamOS](07-connection-implementation.md).
- [Назначение](08-assignment-implementation.md).
- [Delivery](09-delivery-implementation.md).
- [Очередь](10-queue-implementation.md).
- [Локальная приёмка и внешние gates](11-local-acceptance.md).
- [Независимое ревью](12-independent-review.md).

Фактические публичные контракты и миграции находятся в соответствующих
репозиториях. При расхождении с проектными файлами в `contracts/`
использовать runtime OpenAPI/proto, миграции и код владельца сервиса.

## Эксплуатация

- [Подключение](../../runbooks/lead-distribution-connection.md).
- [Виджет](../../runbooks/lead-distribution-widget.md).
- [Диагностика админки](../../runbooks/lead-distribution-admin.md).
- [Наблюдение и подготовка пилота](../../runbooks/lead-distribution-pilot.md).
- [Перенос staging 06.10.2026](../../verification/staging-sync-2026-10-06.md).

Живые OAuth/SDK/установленный виджет, целевое окружение, legacy cutover
и производственный rollout требуют отдельной приёмки. Границы и условия
указаны в локальной матрице и runbook пилота.
