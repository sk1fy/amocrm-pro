# Исправления аудита этапа 7 — 10 сентября 2026

База: `dac12af` плюс незакоммиченные этапы 6/7. Исправлены четыре
воспроизведённых дефекта; исходные результаты исполнителя сохранены отдельно.
Runtime/production не изменялись, внешние вызовы в проверках — синтетические.

## Изменения

| Дефект | Исправление | Проверка |
| --- | --- | --- |
| Reconcile/uninstall удаляли чужие webhook destinations | Реестр установки с зашифрованными URL; intent сохраняется до регистрации. Удаляются только подтверждённые текущие/прежние destinations. Legacy определяется по точному каноническому пути с текущим секретом установки | Чужой коннектор, сосед на том же hostname, смена ключа/hostname, потеря ответа регистрации, повтор uninstall |
| Старый rule-configuration job ломал maintenance из-за RESTRICT FK | Зависимые результаты очищаются ограниченными пакетами до jobs. Свежие результаты, idempotency receipts и outbound effects защищают job | Молодая конфигурация, истёкшая конфигурация с живой квитанцией, затем удаление всей истёкшей цепочки при batch=1 |
| Expired OAuth lease разрешал повтор одноразового refresh | Expired claim сохраняет неизвестный исход. Финализация проверяет version+lease identity. Поздний владелец может сохранить результат; stale caller и новый lease не могут переписать его | Один remote refresh вместо двух; late success; restart/timeout; новая авторизация; отдельная проверка lease fencing |
| Uninstall повторял истёкший access token и не доходил до DELETE | Общий OAuth provider с operator capability на один UUID со статусом uninstalled | Реальный OAuthClient/amoCRM.Client с mock HTTP обновляет token, удаляет owned webhook, повтор сходится; обычный product TokenProvider и другой UUID запрещены |

Новая миграция Core: **000015_webhook_ownership**. Она добавляет
`installation_webhook_destinations` и индекс cleanup для
`lead_status_workflow_rule_configurations`. Существующие 000012/000014 и
миграции CRM Events сохраняются.

URL содержит webhook secret, поэтому в реестре хранятся hash, ciphertext и
key version, а AAD связывает ciphertext с installation ID и hash URL.
Успешно удалённые старые destinations удаляются из реестра. Неизвестные старые
URL с утраченным ключом не считаются собственностью установки и автоматически
не удаляются. Это защищает другие интеграции при account-level API списке.

GC конфигураций использует общий семидневный горизонт; возраст проверяется и
по конфигурации, и по terminal job. Для живой квитанции результат остаётся
доступным. Добавлен конечный metrics label `rule_configuration`.

## OAuth recovery

Внешний HTTP остаётся вне SQL-транзакции. Timeout/transport/cancel в HTTP и 5xx
не доказывают, что token не был использован, поэтому claim сохраняется и второй
Refresh запрещён. Отмену до вызова Gateway и ошибку decrypt можно освободить;
429 разрешает повтор отклонённого запроса. 401/validation закрывают свой claim
и помечают auth failure атомарно, с ограждением от reauthorization.

`ErrRefreshOutcomeUnknown` не переводит установку в ложный reauth_required.
Прежний владелец может завершить persist. Если его ответ/pending потерян,
нужна новая авторизация. Убирать lease вручную для повторной отправки token
нельзя. Operator CLI возвращает для этого случая отдельную понятную ошибку.

Старый `StoredAccessTokenProvider` с no-op refresh удалён. Новый
`NewUninstallTokenProvider` использует ту же ротацию и сохраняет uninstalled:
обычный product token load продолжает отклоняться.

## Проверки

- До исправлений: [три регрессии](before-regressions.txt),
  [uninstall с истёкшим token](before-uninstall.txt) — FAIL на требуемое
  корректное поведение. После исправлений эти случаи перенесены в штатные тесты.
- [Целевые пакеты с -race](targeted.txt) — PASS: OAuth, maintenance,
  webhook и integrations CLI. Дополнительная проверка durable registration
  intent и CLI: [PASS](final-targeted.txt).
- `make TEST_COMPOSE_PROJECT=amocrm-stage7-fix-test integration-test` — **PASS**,
  включая down/up, конкурентный migrate и штатный Core suite с -race.
  220 верхнеуровневых PASS, 2 ожидаемых opt-in performance SKIP.
  [Полный лог](integration-test.txt).
- `make ACTIVITY_TEST_PROJECT=amocrm-stage7-fix-activity-test activity-ci` —
  **PASS: 215 Go PASS с -race, 3 служебных helper SKIP, UI 25 PASS**.
  [Команда](activity-ci.txt), [Go](activity-go.txt), [UI](activity-ui.txt).
  Process fault/mode switch выполнены. Необязательный предыдущий бинарь не
  задавался; actual old-binary сравнение не заявляется.
- `make test` — **PASS**: сборки backend-бинарей, gofmt, `go vet ./...`,
  `go test -race -count=1 ./...` в Docker. [Лог](make-test.txt).
  DB-проверки этого gate не подменяют integration suite выше.

Проверенные Go/SQL файлы: [SHA-256 manifest](source-manifest.json).
Тестовые Compose-проекты удалены штатными traps; [Activity cleanup](activity-cleanup.txt).

Новые/обновлённые регрессии находятся в:

- `internal/oauth/rotation_regression_integration_test.go`;
- `internal/oauth/token_provider_integration_test.go`;
- `internal/maintenance/rule_configuration_cleanup_integration_test.go`;
- `internal/webhook/ownership_regression_integration_test.go`;
- существующих reconcile-тестах (теперь явно задают owned destinations).

## Перед развёртыванием

1. Применить Core 000015 до нового бинаря API/worker/integrations CLI.
2. Остановить/дренировать старые OAuth refresh executors: предыдущая версия
   могла перехватить истёкший lease. Существующий неизвестный claim разрешается
   успешным persist прежнего владельца или reauthorization.
3. Сохранять старые encryption keys, пока на них ссылаются credentials,
   webhook keys или новый реестр destinations. Inventory дополнен в
   [runbook ротации](../../../runbooks/secrets-rotation.md).

Удалённые прежней ошибочной реализацией чужие webhooks автоматически не
восстанавливаются. Down 000015 удаляет реестр владения, а не восстанавливает
удалённые remote subscriptions. Проверка целевого сервера/KMS по CORE-06
по-прежнему открыта. Для текущих изменений production доступ не использовался.

## Воспроизведение

```sh
make TEST_COMPOSE_PROJECT=amocrm-stage7-fix-test integration-test
make ACTIVITY_TEST_PROJECT=amocrm-stage7-fix-activity-test activity-ci
make test
```

Каждый DB gate использует отдельный тестовый Compose-проект с автоматическим
cleanup. Тесты с reset общей БД не запускались одновременно в одном проекте.
