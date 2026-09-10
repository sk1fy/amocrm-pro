# REL-03: хранение технической истории после аудита

Дата: 2026-09-10. Исправлены два дефекта: поздний replay после удаления inbox
мог откатывать новые настройки; paused backfill удалялся до восстановления
авторизации. Контракт: [ADR-0016](../../adr/0016-technical-history-retention.md).

Очистка completed/failed jobs ограничена `HistoryBatch` и горизонтом не менее
семи суток. Paused-задания сохраняются вместе с диапазоном, курсором и связями.
Owner не удаляет `event_inbox` и `event_operations`, поскольку Core пока не
ограничивает календарный срок redelivery при простое и операторском retry.
Поэтому replay после GC jobs возвращает ту же операцию и сохраняет Conflict
при несовпадающем payload.

Действующий retention событий, сирот enrichment и coverage позади границы,
а также накопительные счётчики `event_sources` сохранены.

**Открытый остаток:** конечный срок хранения inbox/operations требует
согласованного протокола CORE-05. REL-03 нельзя считать полностью закрытым
до этого решения. Безопасная очистка jobs и защита восстановления реализованы.

Регрессии: `stage6_retention_regression_test.go` (контроль без GC и проверка с GC)
и `stage6_technical_history_integration_test.go` (поздний replay, bounded cleanup,
живой lease, independent retention, metrics и coverage/enrichment).
Критерии повторного прогона: [fixes/criteria.md](fixes/criteria.md).
