# Распределение сделок через amocrm-pro

## Действующие требования и приёмка

- [Бизнес-правила](01-business-rules.md).
- [Пилот и переход со старого распределителя](03-pilot-and-acceptance.md).
- [Локальная приёмка и внешние gates](11-local-acceptance.md).

Реализованные HTTP-контракты Core:
[distribution API](../../../api/distribution-openapi.yaml),
[widget API](../../../api/distribution-widget-openapi.yaml) и
[admin API](../../../api/admin-openapi.yaml).
Контракты TeamOS принадлежат `team-os-backend/contracts/`.
Код, runtime OpenAPI и миграции определяют реализованное поведение.

## Эксплуатация

- [Подключение](../../runbooks/lead-distribution-connection.md).
- [Виджет](../../runbooks/lead-distribution-widget.md).
- [Диагностика админки](../../runbooks/lead-distribution-admin.md).
- [Наблюдение и подготовка пилота](../../runbooks/lead-distribution-pilot.md).

Живые OAuth/SDK, установленный виджет, целевое окружение, legacy cutover
и production rollout требуют отдельной приёмки по матрице и runbook пилота.
