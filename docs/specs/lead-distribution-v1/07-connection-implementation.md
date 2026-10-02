# РС-03: подключение, идентичности и справочники

Дата: 02.10.2026. Реализация соединения распределения с новым Core.

## Что работает локально

Core зарегистрировал opt-in capability `lead-distribution`. Существующие OAuth,
шифрованные credentials, reauthorization и installation disable/uninstall
переиспользуются; новой копии OAuth или amoCRM limiter нет. Новый private listener
worker использует тот же amoCRM Client и outbound budget, что прежние продукты.
Дополнительные product routes не изменяют Lead Status или Activity.

Scoped signed HTTPS bridge связывает existing company UUID с installation,
integration и account только после двух проверок: полномочий администратора
TeamOS и проверенного одноразового amoCRM widget JWT текущего CRM администратора.
JTI и binding записываются атомарно. Повтор после потерянного ACK восстанавливает
ту же связь. Pending intent ограничен 15 минутами; revoke ставит durable tombstone
и останавливает поздний confirm. Повторная связь сохраняет прежнюю revoked историю.

CRM users, pipelines и stages получаются целиком с ограниченной пагинацией.
Отсутствующие/null collections и неполные ответы не становятся пустым справочником.
Удалённые ID обнаруживаются отсутствием в новом снимке; дата получения и предел
свежести передаются явно. PII и полные права пользователя в справочник не входят.
Существующие employee UUID сохраняются; full mappings mirror защищён
mappingRevision и payload hash от повторов и задержанных запросов.

Проверка `CanViewLead` использует current active user, role, general rights,
status override и user/group подписчиков текущей сделки. Она допускает обычного
сотрудника с правом просмотра и закрывает доступ при неизвестной policy. Виджет
проверяет TeamOS section access и active employee отдельным подписанным обратным
запросом; employee ID сверяется с mirror. Серверные ключи и OAuth tokens браузерам
не передаются. Public widget paths используют существующие JWT/CORS принципы.

Readiness установки сообщает безопасные OAuth/capability/subscription состояния
и recoveryAction. `assignmentReady=false`: назначение и бизнес-очередь принадлежат
следующим этапам, а работающее соединение не означает готовность всей функции.

## Изменение предложенного транспорта РС-02

В РС-02 предполагался mTLS/RPC bridge. Для РС-03 реализованы отдельные HTTPS routes
с HMAC по exact request bytes, method/URI/key/scope/timestamp и durable nonce.
Это явное изменение реализации, описанное в
[ADR-0033](../../adr/0033-distribution-signed-http-connection.md).
TLS обязателен и завершает доверенный ingress; private worker port нельзя
публиковать напрямую. Пример маршрутизации и точные методы находятся в
[runbook](../../runbooks/lead-distribution-connection.md).

## Проверки и ограничения приёмки

Два полных запуска `make integration-test` прошли: реальная disposable PostgreSQL,
миграции up/down, конкурентный migrator, существующие regression suites и
distribution binding lifecycle/capability/mapping/nonce tests. HTTP recovery/permission проверки выполнялись против реальной PostgreSQL,
а не вместо транзакционной реализации.

Проверяемые сценарии: изменённые signature body/method/query/key/scope, duplicate
headers, replay nonce между репликами, отключённая capability, reauth, cross-company
и cross-account binding, JTI и lost ACK, mapping actor isolation, version conflicts,
revocation, late confirm tombstone; права A/G/M/D, status deny/expand, role и
подписчик; реальная композиция widget JWT/CORS/JTI и подписанного TLS callback,
обычный mapped сотрудник, wrong origin, replay, capability off и local policy unavailable; неполные/дублированные/слишком большие источники и пагинация.

**Живого тестового окружения и CRM аккаунта нет, что подтвердил пользователь.**
Подключение, повторную авторизацию и диагностику на реальном тестовом аккаунте
не проверяли. РС-03.1.2 остаётся на hold до предоставления окружения; этот документ
не закрывает этап и не заменяет live acceptance. Deployment TLS/firewall evidence
и installed-private-widget JWT/CORS также не доказаны локальными unit fixtures.
ADR-0033 остаётся Proposed.

## Трассировка

| Подзадача | Реализованный результат | Оставшаяся проверка |
| --- | --- | --- |
| РС-03.1.1 | capability/catalog, explicit provisioning, lifecycle gates без assignment effect endpoint | эффект assignment реализуется в РС-04 с повторной проверкой capability |
| РС-03.1.2 | reuse OAuth, current scope, safe readiness/recovery states | **реальный аккаунт: connect/reauth/diagnostics; on hold** |
| РС-03.2.1 | signed scoped grant + verified CRM admin binding, pending recovery/revoke/history | deployment и live dual approval в пилоте |
| РС-03.2.2 | UUID-preserving mapping mirror, TeamOS principal and widget actor/resource policy | live CRM rights variation в пилоте |
| РС-03.3.1 | complete bounded users/pipelines/stages snapshots, freshness/source errors | target-account объёмы/права источника в пилоте |
| РС-03.3.2 | scoped signed identities, key rotation/grants/nonces, widget JWT/CORS ingress | production TLS/origins/service deployment evidence |

Core runtime: `internal/distribution`, `internal/integration/amocrm/distribution.go`,
миграция `000018`, worker composition и canonical public `api/openapi.yaml`.
TeamOS runtime хранится в собственном company service; Core не читает его БД.

### Итог текущих автоматизированных проверок

- `make test`: passed — gofmt, go vet и `go test -race -count=1 ./...`.
- `make integration-test`: passed — migrations up/down/concurrent и regression suites,
  включая HTTP recovery/mapping/ACL/revoke и durable scope/lifecycle tests.
- `make distribution-integration-test`: passed — текущие исходники с поздними
  audit и mapping ACK изменениями, реальные Postgres транзакции под race detector.
  Финальный targeted повтор также passed: real widget JWT/CORS/JTI composition,
  scoped signed HTTPS TeamOS callback и ordinary employee resource policy.
- `make distribution-test`: passed — auth/ACL/CRM adapter/canonical API и worker/CLI
  compilation; targeted повтор после уточнения JWT settings и каталога также passed.
- Compose opt-in overlay `config --quiet`: passed с синтетическими значениями.
- `git diff --check`: passed.
- `make distribution-grant-build`: passed — контейнер `amocrm-distribution-grant:local`;
  бинарник запускается и показывает usage без обращения к БД.

Сырые логи находятся вне Git. Количество проходящих сценариев не используется
как доказательство live CRM, production TLS или готовности assignment engine.
