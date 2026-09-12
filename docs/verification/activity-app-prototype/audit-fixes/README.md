# Проверка замечаний к 8cdd758 — 2026-09-11

Все пять дефектов из повторного аудита подтверждены исходниками и исправлены:

1. Общий viewer budget проверяется до ResolveShare без выделения per-key state.
   Bucket выделяется только после разрешения включённой ссылки. Регрессия:
   4097 неверных ключей не занимают buckets, следующая действительная ссылка
   возвращает 200. Общий лимит и ограничение числа активных buckets сохранены.
2. PostgreSQL advisory transaction lock сериализует count + insert по
   installation/integration между процессами. Регрессия на PostgreSQL 17:
   при 49 панелях из 12 конкурентных create успешен один, итог 50.
3. Fingerprint CRM Events включает PanelID только для viewer principal.
   Чужой panel cursor отклоняется; Activity уже преобразует несовместимый
   cursor в 404. Для widget поле отсутствует в JSON fingerprint, поэтому
   прежний формат fingerprint сохранён. Существующие viewer cursors потребуют
   начать чтение с первой страницы после обновления.
4. CleanupErrors отслеживает ошибки; CleanupSilent использует время последнего
   успешного commit (изначально 0) и учитывает отсутствие метрики. Успех
   агрегируется между worker replicas, использующими общий cleanup lock.
   CleanupBatchPressure обнаруживает повторное исчерпание batch budget даже
   при продолжающемся удалении. Это индикатор давления, а не измерение backlog.
   Runbook и Prometheus behaviour tests обновлены.
5. Transfer rehearsal использует приватный mktemp-каталог, umask 077 и ранний
   cleanup trap. Python regression test проверяет 0700/0600 и отсутствие файлов
   после ошибки config и отказа TRANSFER_LIVE; включён в activity-transfer-verify.

## Выполненные проверки

- go test ./api/... ./cmd/... ./internal/... — PASS.
- go vet ./api/... ./cmd/... ./internal/... — PASS.
- make activity-observability-test — PASS (promtool config и реальные alert rules).
- python3 deploy/activity/test-verify-transfer.py — PASS.
- PostgreSQL 17 в отдельном временном контейнере с tmpfs, go test -race:
  TestPostgresConcurrentPanelQuota,
  TestPostgresPanelsIsolationRotateDisableAndViewerDTO,
  TestCreatePanelChangedEnabledConflicts — PASS.
- git diff --check — PASS.

## Ограничения проверки

- go test ./... не завершился успешно из-за ранее существовавшего игнорируемого
  tmp/stage9-review: в одном каталоге смешаны Go-пакеты activitybridge и activity.
  Эти файлы не изменялись; все пакеты приложения проверены отдельно.
- make activity-ci остановился перед component integration tests: PostgreSQL
  сообщил No space left on device в Docker. Полный transfer restore также
  остановился при запуске PostgreSQL. Для адресной проверки квоты использован
  PostgreSQL с данными в tmpfs; общий gate этим не заменяется.
- Предыдущий API binary, production deploy, live cutover и внешняя продуктовая
  приёмка не проверялись. Этапы 8/9 остаются открытыми; согласованный выпуск
  Core, policy/Gateway, Activity и CRM Events по-прежнему необходим.
