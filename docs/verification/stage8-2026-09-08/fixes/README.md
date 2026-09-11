# Исправления повторного ревью этапа 8

11.09.2026. Рабочее дерево поверх `d7cb8c7`, без commit/push/deploy.
Предыдущие отчёты этапа сохранены как история. Исправлены четыре замечания.

| Замечание | Исправление | Доказательство |
| --- | --- | --- |
| Четыре алерта возвращали пустой вектор | Очереди агрегируются `sum without(state)`, сохраняя labels цели; ошибки сопоставляются `ignoring(code)` | `alerts.test.yml` загружает сами rules; исходная версия проваливает firing-проверки |
| Остановленный Gateway не увеличивал server counter | Отдельный `GatewayScrapeUnavailable`: `up == 0` либо отсутствие worker scrape target, `for: 5m` | Сценарии stopped, missing и recovered Gateway |
| `accepted` не восстанавливался из старого Events dump | Поддерживаемый restore — полный согласованный набор при остановленных writers; обязательный read-only `verify-restored-commands.sh` отклоняет несовпадения до запуска | Synthetic restore трёх owners с отдельными command ID для Activity и Events; negative cases ниже |
| При переносе Events не переключался Activity | Runbook и ADR требуют обновить `CRM_EVENTS_ADDRESS` и перезапустить Core API **и Activity**, в том числе при rollback | Сверено с `componentruntime/runtime.go`, где Activity создаёт собственный Events client при старте; physical cutover остаётся открытым |

## Новые проверки

`deploy/observability/alerts.test.yml`: 18 PromQL assertions над `ALERTS`.
Проверяются реальные `job/instance/component`, выдержка `for`, firing,
восстановление, отсутствие Gateway и невозможность заимствовать очередь
соседней цели. До исправлений — шесть FAIL (четыре rules и два сценария
нового Gateway alert); после — PASS. Пороги существующих алертов не увеличены.

`verify-backup-owners.sh --isolated` теперь проверяет:

- согласованный restore с обоими типами Core targets;
- отказ при отсутствующем Events inbox;
- отказ при отсутствующей Activity receipt;
- отказ при несовпадении actor;
- отказ при недоступной/неверной owner DB;
- допустимость `pending_delivery` без inbox и истории старше семи дней.

Preflight сопоставляет target, integration, installation, actor и command ID;
для Events требует также прежний operation ID. Работает через отдельные DSN
оператора и read-only SQL, не добавляет чужих DSN продуктовым процессам.
Секреты, payload и tenant IDs в его лог не попадают.

Обе проверки включены в `make activity-ci`, который вызывает GitHub Activity
job. Новые миграции, runtime API и изменения Go бизнес-логики не понадобились.

## Граница восстановления

Это исправление не добавляет автоматический replay `accepted`. При провале
preflight требуется другой полный согласованный набор; без него восстановление
не принято. Сверка identity необходима, но не доказывает согласованность
произвольных независимых live dump. Истёкший redelivery horizon не продлевается.
Такая граница явно записана в runbook вместе с остановкой всех writers,
проверкой набора и запуском только после PASS.

Живой SDK, production keyring/роли, multi-host cutover и выпуск не выполнялись.

## Команды

```sh
make activity-observability-test
make activity-backup-verify
make ACTIVITY_TEST_PROJECT=amocrm-stage8-fixes-test activity-ci
```

## Итоговый gate

`make ACTIVITY_TEST_PROJECT=amocrm-stage8-fixes-test activity-ci` завершился
с exit **0** на окончательном коде: 230 верхнеуровневых Go PASS / 0 FAIL,
3 служебных helper SKIP, `-race`; UI 25 PASS / 0 SKIP. В этот же gate вошли
13 валидных Prometheus rules, 18 assertions и обновлённый owner restore с
negative cases. `RTO_LOCAL_SECONDS=1` относится только к синтетической репетиции.
Проверка с предыдущим API-бинарём не выполнялась: optional
`COMPONENT_PROCESS_PREVIOUS_API_BINARY` не задан.

Артефакты: [полный gate](activity-ci.txt), [Go](activity-go.txt),
[UI](activity-ui.txt), [красные rules до исправления](alerts-before.txt),
[целевой PASS rules](alerts-after.txt), [SHA-256 проверенных файлов](source-sha256.json).
`sh -n`, `git diff --check` и ссылки изменённых runbooks также проверены.

Compose-проекты `amocrm-stage8-fixes-test` и `amocrm-stage8-backup-test` после
проверок удалены вместе с тестовыми данными. Runtime `amocrm-activity` не менялся.
