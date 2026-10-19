# Контроль задач: release candidate 1.2.3, схемы v1/v2

Актуализировано 16.10.2026, КТ-01.R10. Порядок поставки: **Core → company/gateway
TeamOS → пакет виджета**. Новый frontend TeamOS и экраны админки не обязательны.
Правила, история и resolver принадлежат company; Core проверяет установку,
идентичность и CRM-справочники. Проверка/создание задач выполняется в amoCRM.

Сейчас разрешены только локальные проверки. R09 не прошла новую живую amoCRM и
браузерную приёмку, поэтому этот документ **не разрешает пилот или production rollout**.
Release manifest должен отдельно отражать незакрытые gates. Не считать старый
happy path 1.1.3 подтверждением 1.2.3. [Живая R06](task-control-r06-acceptance.md)
называет фактические прежние SHA, backup и восстановление; утверждение «ничего
не развёрнуто» к прежнему стенду уже неприменимо. Read-only аудит 19.10.2026 обнаружил на тестовом сервере прежние Core `759c4e3`
(schema 25) и backend `39cf57a` (schema 33); candidate 1.2.3 там отсутствует.
Bootstrap 200 относится к прежнему R06; новая выкладка в этом этапе не выполнялась.

## Согласованная поставка

Локальный manifest создаётся `scripts/task-control-release.py` репозитория backend:
точные SHA трёх checkout, Core/company schema 30/40, версия runtime 1.2.3,
HTTPS origins, OAuth UUID (либо явно неизвестен), ZIP SHA-256, toolchain,
локальные image IDs/архитектура и опубликованные RepoDigests, если они существуют.
Не заменять registry digest локальным Docker ID и не использовать плавающий latest.

Скрипт требует коммитов и чистых tracked файлов; дважды собирает каждый ZIP и
проверяет совпадение байтов. `SOURCE_DATE_EPOCH` берётся из коммита виджета;
Python/zlib записываются в manifest. Разные toolchain не объявляются побитово
совместимыми без сравнения. `--build-images` собирает api/worker/migrate/integrations,
task-control-grant/runtime-descriptor Core и company/gateway backend, с OCI revision.
По умолчанию origins — example.invalid, OAuth UUID не задан, releaseApproved=false.
Такой candidate нельзя устанавливать для живого managed-прогона.

```sh
# Из backend. Новый каталог; secrets сюда не передаются.
python3 scripts/task-control-release.py --output /tmp/task-control-rc --build-images
# Для реального кандидата отдельно указать проверенные публичные HTTPS origins
# и --oauth-client-uuid действующей приватной интеграции Контроля задач.
```

OAuth UUID вводится в существующей настройке виджета и сверяется с Core integration;
он не является секретом и не подменяет client secret/state. Origins зашиваются
в ZIP; UUID записывается в manifest для сверки, а не применяется к аккаунту скриптом.
Исторический тестовый origin: https://2-26-66-187.sslip.io; его доступность и право
использования в новом пилоте нужно проверить заново. Distribution installation
не заменяет Task Control installation. До выкладки зафиксировать предыдущие SHA,
registry digests/архитектуру, ZIP checksum, installation/generation, binding/epoch,
schema, publication/version/runtime revision и состояние обоих редакторов.

## Совместимость и UI

| Пакет | Протокол | Ограничение |
|---|---|---|
| 1.1.0/1.1.3 | Исторический MVP v1 | Не целевой rollback и не подтверждение текущей приёмки |
| 1.1.4 | v1 | Поддерживаемый legacy; descriptor не разрешает dynamicRulesV2 |
| 1.2.0 | v1/v2 | Negotiation, динамические intent, mode/export/ACK |
| 1.2.1 | v1/v2 | Дополнительно import/source bridge |
| 1.2.2 | v1/v2 | Дополнительно received/applied/heartbeat/error |
| **1.2.3** | **v1/v2** | Текущий кандидат, actor/async/task-write защиты R08 |

Core доверяет только подтверждённой оператором версии текущего generation.
Браузерные capabilities и heartbeat не являются таким подтверждением.
Старый MVP UI использует совместимые v1 API, пока binding schema=1. После явной
активации v2 старые записи/редактирование v1 получают requires_v2; не включать v2
у пользователя, которому ещё нужен старый редактор. Чтение/публикацию v2 можно
проверять API-клиентом без нового UI. Ошибку 409 нельзя обходить перезаписью БД.

LegacyExplicit хранит прежние CRM IDs и явные 142/143; dynamic allOpen исключает
закрытые этапы и включает будущие открытые. Selected не расширяется автоматически.
Пустая actor-проекция — выключение контроля, не fallback на standalone.
Динамический intent/историю нельзя заменить урезанной проекцией одного actor.

## Backup перед обновлением

Остановить изменения конфигурации/публикации и задания resolver на согласованное
окно; зафиксировать момент backup, активные mode intents и текущие указатели.
Сделать pg_dump -Fc **обеих** БД: Core и company, с согласованными идентификаторами
и временем. Проверить pg_restore --list, checksum и восстановление в отдельные БД.
Сохранить защищённо env/overlays, текущие images, настройки reverse proxy, ключи
шифрования с версиями, JWT/assertion/HMAC keys и OAuth secret. Dump без совпадающих
ключей шифрования не обеспечивает восстановления OAuth. Не выводить секреты в
manifest, shell history, access/debug logs. Шифровать реальные backup и ограничить доступ.

При отдельном согласовании сохранить полный legacy-каталог/runtime_config виджета,
оригиналы импорта, draft backup, publication/intent и полный экспорт standalone.
Не ограничиваться применённым actor snapshot. Проверить отсутствие confirmed mode
intent, требующего reconciliation; не отменять его таймером или SQL.

Локальные drills: Core `scripts/task-control-backup-drill.py --output /tmp/r10-core-drill`
проверяет upgrade 28→30, dump/restore всех таблиц, generation, миграционный replay
и отказ down30 при evidence 1.2.3. Company opt-in drill описан в backend
`docs/integrations/task-control-r10-release.md`; он проверяет mode/export, сохранность
23 task-control таблиц, restart application и явный restore публикации. Это реальные
PostgreSQL операции с синтетическими данными, не восстановление production.

## 1. Core

Применить миграции **по 30** штатным migrate, затем обновить api и worker на
согласованные images. Сохранять encryption/OAuth/очереди и прежние overlays.
Не выполнять down при binary rollback. Сначала проверить health/listeners.

`docker-compose.task-control.yml` включает отдельный worker listener 8087 и
одноразовые tools. Он не публикует listener наружу. Staging loopback overlay
привязывает только 127.0.0.1:18087. Нужны существующие параметры DB/шифрования и:

- TASK_CONTROL_SERVICE_KEYS: отдельные scoped HMAC key IDs/секреты (≥32 символов).
- TASK_CONTROL_ASSERTION_KEY_ID и TASK_CONTROL_ASSERTION_PRIVATE_KEY: Ed25519,
  private key base64 64 байта; в backend передаётся только public key 32 байта.
- TASK_CONTROL_HTTP_ADDRESS/TLS_PROXY из overlay, корректный доверенный TLS proxy.

Не использовать тестовые ключи. Зарегистрировать именно приватный Task Control
OAuth client по [provisioning](integrations.md), сохранить redirect URI/state.
Для существующей интеграции штатным integrations set-service включить task-control.
Выдать grant конкретной company/installation; references требуют отдельного --references true.

```sh
docker compose -f docker-compose.yml -f docker-compose.task-control.yml run --rm task-control-grant \
  --key-id KEY_ID --company-id COMPANY_UUID --installation-id INSTALLATION_UUID \
  --actor OPERATOR_ID --enabled true --references true
```

После фактической установки и проверки пакета 1.2.3 подтвердить **текущее** поколение:

```sh
docker compose -f docker-compose.yml -f docker-compose.task-control.yml run --rm task-control-runtime-descriptor \
  --installation-id INSTALLATION_UUID --generation CURRENT_GENERATION \
  --runtime-version 1.2.3 --actor OPERATOR_ID
```

Есть отдельный image/target runtime-descriptor с --version и OCI revision; использовать
pin из manifest. Reinstall меняет generation, прежнее подтверждение не наследуется.
Отзыв evidence: те же installation/generation/actor и --clear без runtime-version.
Отзыв — административная операция, не способ незаметно переключить схему правил.

## 2. Backend и reverse proxy

Применить company миграции **по 40**, затем обновить company/gateway согласованной
парой. Сохранить verified company/account mapping и права. Company настраивается через
COMPANY_TASK_CONTROL_CORE_URL/KEY_ID/SECRET и ASSERTION_PUBLIC_KEYS. Последний —
COMPANY_TASK_CONTROL_ASSERTION_PUBLIC_KEYS, map key ID → base64 public key.
Существующий COMPANY_GATEWAY_SERVICE_TOKEN/GATEWAY_COMPANY_SERVICE_TOKEN должен
совпадать. Секреты не попадают в браузер. Проверить реальные amoCRM CORS/preflight;
CORS не заменяет авторизацию. Read/manage permissions проверяются на каждом запросе.

В staging `public.locations.conf` содержит точные auth/disconnect Core и
connection/sessions/bootstrap/source-result/runtime-v2/ACK/telemetry TeamOS routes.
Изображение одного origin не означает передачу правил через Core. Body source ≤1 MiB,
telemetry ≤8 KiB. `private.locations.conf` остаётся в закрытом TLS vhost с прежними
allow/deny; внутренние installation/references routes подписаны HMAC. Admin listener
не публикуется через runtime proxy. Не открывать весь /api/ или 8087/8443 наружу.

Проверка include: `python3 scripts/task-control-proxy-smoke.py` в Core запускает
изолированный nginx, проверяет синтаксис и 16 разрешённых/запрещённых путей со stub
upstreams. Это routing smoke; реальные CORS/авторизация/сертификаты проверяются отдельно.
На сервере выполнить nginx -t перед reload, сохранить маршруты других виджетов.
У signed disconnect access_log off; отключить запись query/body/Authorization также
на вышестоящих proxy. Настроить именно URL отключения, не OAuth redirect URI.

## 3. Runtime и миграция standalone

Собрать ZIP только со сверенными origins, проверить checksum и OAuth UUID, установить
в ту же интеграцию штатным способом. Отдельного обновления frontend не требуется.
До явного действия администратора standalone не зависит от TeamOS.

1. Link требует свежего CRM-admin proof и актуального TeamOS manage. Сохранить requestId.
2. Импортировать старые правила через source request → preview → новый verification
   snapshot → apply. Изменившийся источник даёт source_changed. Сохранённые UID,
   flags, closed stages и inactive rules не должны исчезнуть. Apply меняет только draft.
3. Для динамики: в standalone активировать v2; сохранить intent, validate/preview,
   publish и явно перейти в managed. Старый редактор после этого не использовать.
4. В виджете сохранить выбранный режим штатной кнопкой, перезагрузить вкладки.
   Проверить publication/runtime revision и received/applied через application API.
5. Не считать ACK одной вкладки применением всем. Проверить scope/actor и свежесть
   справочников, затем отдельные разрешённые сценарии leads/customers.

## Rollback и восстановление

Предпочтителен rollback правил: штатный publications/rollback создаёт новую версию
исторического intent на **текущих** источниках; draft и история сохраняются. Старый
requestId повторяется только с тем же телом. Удалённый отдел/недоступные ссылки могут
запретить restore; это не основание редактировать publication pointers через SQL.

Для standalone использовать mode-preview **полного** экспорта и явный mode intent,
подтвердить dynamicLoss/empty export при необходимости. Зафиксировать checksum и
revisions. V2 mode-intents возвращает server receipt со state=completed и полным
export: это ещё не сохранение настроек amoCRM. Передать receipt в
widget.taskControlMigration.prepareStandalone с явными подтверждениями, проверить
новый отдельный каталог и сохранить подготовленный runtime_config основной кнопкой
виджета. Receipt/export живут одну минуту; просроченный результат требует нового
согласованного preview/операции. V1 prepared/confirmed требует штатного reconciliation.
Нельзя просто снять checkbox и продолжить со старым каталогом. Проверить сохранение
runtime_config/каталога и работоспособность standalone, не удалить серверный intent.

Binary rollback допустим лишь к ранее проверенным images, совместимым с оставленной
схемой 30/40 и текущим протоколом. Откат к старому runtime v1 при активном binding v2
не допускается: сначала выполнить явный standalone экспорт/смену режима и отдельно
принять потерю динамики. Старые 1.2.* протокольно совместимы, но не содержат всех
защит R08; их нельзя объявлять эквивалентом 1.2.3. Не релабелить descriptor версии,
не уменьшать epoch/generation/revisions, не очищать историю. Down30 отказывает при
1.2.3 evidence; down40 отказывает при retained runtime events. Обход guards запрещён.

При восстановлении БД: закрыть runtime/management ingress и остановить записи/worker,
восстановить **в отдельные** БД, сверить schema checksums и контрольные суммы бизнес-
таблиц/истории. До открытия удалить восстановленные runtime sessions/issued metadata,
потребовать свежую installation/reference проверку; replay records не удалять ради
повторного proof. Дождаться истечения всех старых proof/session/подписанных transport
окон либо выполнить согласованную ротацию ключей. OAuth refresh tokens из старого
backup могли быть заменены снаружи — проверить актуальность/reauth, не объявлять active
по одному dump. Согласовать пары Core/company identity и intent reconciliation.
Нельзя обещать сохранение изменений после точки backup без проверенного PITR/WAL.

## Ротация, лимиты, наблюдение и остановка

Ed25519: сначала добавить новый public key в backend, затем переключить Core signer;
старый verifier оставить до max exp + допустимого skew всех прежних assertions.
HMAC: добавить новый key ID/secret в Core и scoped grant с нужной references capability,
переключить backend, проверить запросы, затем вывести старый key/grant после допустимого
окна/retry. Нельзя использовать один ключ для разных продуктов. Шифровальные ключи
сохраняются по версиям до перешифровки и проверки restore; OAuth secret rotation
планируется отдельно, с учётом подписанного disconnect. Ключи не вращать автоматически
скриптом подготовки candidate.

Рабочие пределы: assertion 60 s, session/lease ≤300 s, 64 сессии на actor/binding,
poll видимой вкладки 60 s, backoff ≤300 s, payload ≤1 MiB/1000 правил. References TTL
≤5 min, refresh с возраста 120 s; healthy org/mapping→resolver ≤60 s, CRM→resolver
≤210 s, доставка видимой вкладке ещё ≤60 s. При outage эти сроки не обещаются;
stale lease истекает fail-open без standalone fallback. Import/source TTL 10 min.
Telemetry: 20 событий/instance/min, 1000/binding/min, 256 instances/5 min; события
30 дней, batch GC 1000/min. Heartbeat не продлевает lease и не доказывает uninstall.

Наблюдать через существующие HTTP/worker логи и application API: 401/403/409/429/503,
source/installation freshness, runtime revision, received/applied/heartbeat age,
reason/errorCode и correlation ID, resolver lag, накопление confirmed intents/GC.
Core `/admin/v1/installations/{id}/task-control/diagnostics` требует ADMIN_API_TOKEN и
X-Admin-Actor на приватном admin listener; [пример](../examples/task-control-diagnostics.json).
Бизнес-состояние применения читается из TeamOS. Не создавать dashboard-значения,
которых реальные endpoints/метрики не дают.

Остановить расширение пилота при tenant/actor утечке, повреждённом checksum, двойной
helper-записи, неразрешённом обходе прав, систематическом stale/outage, неуспешном
restore backup или неопределённом confirmed mode. Сначала остановить изменение scope,
зафиксировать evidence/correlation, сохранить данные; recovery/standalone выполняются
явно. Не снимать marker неизвестного POST и не повторять task POST автоматически.
Browser/mobile/API/другое устройство не покрыты полной серверной гарантией.

Контракты: [Core internal](../../api/task-control-openapi.yaml),
[Core admin](../../api/admin-openapi.yaml); backend contracts/openapi/teamos.yaml и
contracts/fixtures/task-control-*.json, docs/integrations/task-control-r09-acceptance.md,
task-control-r10-release.md. Перед пилотом обязательны CI **финальных** SHA, R09 live,
поддерживаемые браузеры и конкретные registry digests/ZIP с реальными origins.


## Передача серверному агенту после merge (аудит 19.10.2026)

Обновить Core, backend и widgets со согласованного main; записать полные SHA и CI.
Создать **новый** candidate из этих checkout с фактическими HTTPS origins и OAuth
UUID именно `task-control-widget`. `amocrm-pro-service` в тестовом аккаунте —
Distribution, его installation и OAuth UUID не использовать для Контроля задач.
Пакет на диске с manifest 1.1.2 и старые rollback-r06 images не доказывают наличие
1.2.3 и совместимого rollback. Не назначать runtime descriptor до фактической
установки и проверки конкретного ZIP на текущем generation.

Core Compose tools передают BUILD_REVISION и показывают тот же OCI SHA, что API
и worker. При обычном build передать `BUILD_REVISION=$(git rev-parse HEAD)`;
при candidate-сборке его задаёт backend release tool. Закрепить candidate overrides
последними, применять `up --no-build`, сохраняя действующие env/ключи и proxy.
Миграции Core 25→30 и company 33→40 выполнять штатным up после backup/restore
проверки; полный порядок обновления и условия отката приведены выше.

В backend добавлен `scripts/task-control-deployment-audit.py`: read-only сравнение
четырёх работающих образов и ZIP с candidate.json. Инструкция и ограничения —
backend `docs/integrations/task-control-r10-release.md`. Это artifact check, не
schema/live/rollout acceptance; он не выставляет releaseApproved.

Отдельно проверить webhook error штатным reconcile и статусом задания. При
expired state начать новую scoped OAuth-авторизацию: не отключать проверку state
и не переиспользовать код. Recovery описан в [lifecycle](task-control-lifecycle.md).
Снять свежие installation/generation, binding/epoch, publication/runtime revisions
и received/applied ACK; истёкший snapshot и heartbeat не подтверждают свежесть.
После этого выполнить R09 matrix и R10 full-stack rollout/restore/binary rollback
с сохранением истории/draft и зафиксировать реальные результаты. До их выполнения
R09/R10 не считать завершёнными. Новые PR не меняют эту границу приёмки.


## Пустой список webhook events (исправление после аудита 20.10.2026)

Task Control может работать без webhook events: runtime читает задачи через amoCRM API.
Если после добавления обязательных событий включённых сервисов список пуст, reconcile
не должен выполнять POST `/api/v4/webhooks` с `settings: []` и не должен подписывать
интеграцию на произвольные события. Он снимает только принадлежащие этой installation
подписки и сохраняет `webhook_status=unregistered`, очищая прежнюю ошибку только после
успешной синхронизации. Чужие webhook destinations сохраняются. Ошибка list/delete
остаётся ошибкой и обрабатывается штатными retry-правилами.

После обновления **worker** выполнить штатный административный `webhook.reconcile`
для нужной installation и проверить результат job, `webhook_checked_at`, отсутствие
`webhook_last_error` и фактический список hooks. Для пустого списка ожидается
`unregistered`, не `active`. Не исправлять статус SQL и не добавлять события ради
зелёного индикатора. Если включён Distribution, его обязательные пять событий
сохраняются; пустой registry list не отключает подписку Distribution.

Тело старого HTTP400 не сохранялось, поэтому точный ответ amoCRM ретроспективно
не доказан. Доказан неверный запрос с пустыми settings; исправление и штатная
повторная синхронизация должны подтвердить устранение проблемы на стенде.
Официальный контракт: https://www.amocrm.ru/developers/content/crm_platform/webhooks-api.


Runtime1.2.4 требует Core schema31: новая миграция расширяет CHECK descriptor,
status/read и операторская CLI разрешают1.2.4 с dynamicRulesV2. Down31 отказывает,
пока существует evidence1.2.4: нельзя молча переименовать установленную сборку.
Подтверждать descriptor после установки ZIP, сохраняя installation/generation.
Предыдущие результаты приёмки на schema30 относятся к1.2.3.
