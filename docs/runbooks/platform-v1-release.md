# Выпустить и восстановить Platform V1

Этот runbook описывает подготовку артефактов и действия оператора. Он не
подтверждает production-развёртывание. P10 пропущен по решению владельца:
ручную проверку на реальном аккаунте amoCRM владелец выполняет отдельно.
P11 не снимает gates источников лицензии, активаций или amoSTART.

## Подготовить три образа

Соберите из чистого полного commit SHA три Docker target: `platform`,
`platform-migrate`, `platform-lifecycle`. Всем передайте одинаковый
`--build-arg BUILD_REVISION="$REVISION"`. Проверка не собирает и не скачивает
образы. Для переноса используйте опубликованные оператором
`repository@sha256:digest`; для локальной проверки допустим полный image ID
`sha256:…`. Теги не являются release identity.

```sh
python3 scripts/platform-release-verify.py \
  --platform-image "$PLATFORM_IMAGE" \
  --migrate-image "$PLATFORM_MIGRATOR_IMAGE" \
  --lifecycle-image "$PLATFORM_LIFECYCLE_IMAGE" \
  --revision "$REVISION" --out /secure/release-manifest.json \
  --config-id platform-v1-config-01 --identity-id platform-v1-tls-01
```

Команда сначала фиксирует ID всех трёх образов, затем выполняет `--version`
по этим ID без сети. Она проверяет OCI revision, полный SHA, непривилегированного
пользователя, Linux и одинаковую архитектуру amd64 либо arm64. SQL из
`/migrations` фактического мигратора должен побайтно совпасть с
`migrations-platform` указанного commit. Требуются локальные Docker daemon
и git object этого commit. Манифест содержит только image IDs, ревизию,
архитектуру, контрольную сумму SQL и несекретные идентификаторы конфигурации
и identity. Не передавайте DSN, сертификаты, ключи либо их хеши вместо ID.
Проверка не доказывает происхождение образа или действительность сертификата;
оператор подтверждает источник образов и доверие PKI перед использованием.

Проверки исходников: `make platform-release-check` и
`make platform-observability-test`. Фактические release images проверяются
отдельной командой выше после сборки; unit-тесты её не заменяют.

## Подготовить окружение

Скопируйте `deploy/platform/release.env.example` вне checkout, ограничьте
доступ (`chmod 600`) и заполните собственные значения. Не сохраняйте вывод
`docker compose config`, environment или inspect runtime containers в evidence.
`configuration_id` обозначает утверждённую версию настроек, `identity_id` —
версию комплекта PKI; храните их соответствие в защищённом реестре оператора.

PostgreSQL предоставляет оператор. Compose выпуска не создаёт БД, роли,
пароли или PostgreSQL service. Примените ограничения ролей из
[основного runbook](platform-v1.md): owner доступен только мигратору и
одноразовому bootstrap, процесс Platform получает только access/jobs DSN.
Для удалённой БД используйте `sslmode=verify-full`; CA и пути libpq должны
быть доступны процессу в read-only TLS mount. Разделите БД Platform и Core,
запретите доступ ролей Platform к соседним БД. Рассчитайте соединения:
число реплик × (access pool + jobs pool), плюс мигратор и операции.

`PLATFORM_TLS_DIR` содержит `tls.crt`, `tls.key`, `ca.crt`, читаемые UID
10001. Серверный сертификат имеет DNS SAN закрытого адреса; Core и Manager
BFF используют отдельные SPIFFE identities из основного runbook.
`PLATFORM_MANAGER_CURSOR_KEY` предоставляет оператор в существующем формате
конфигурации. Сохраните ключ при рестарте и откате, чтобы не прервать курсоры.
Для публичного proof задайте существующие `PLATFORM_ACCESS_PROOF_PUBLIC_KEYS`
и `PLATFORM_ACCESS_PILOT_ORIGIN` согласно контракту P04.

Gateway отключён при пустом `PLATFORM_ACCOUNT_GATEWAY_ADDRESS`. При включении
он должен быть доступен по сети и проверять Platform SPIFFE. Можно использовать
тот же Platform identity directory, если он выдан для исходящего mTLS;
иначе задайте `PLATFORM_GATEWAY_IDENTITY_DIR`. Mount read-only; пустая переменная
при отключённом Gateway не создаёт пустой source path. Compose сохраняет
обычную исходящую сеть для внешних PostgreSQL и Gateway.

Management `:8081` доступен только в Docker-сети проекта; порт не публикуется.
Публичный и закрытый listeners по умолчанию привязаны к loopback хоста.
Меняйте `PLATFORM_PUBLIC_BIND`/`PLATFORM_PRIVATE_BIND` только вместе с
сетевыми ACL и маршрутизацией доверенных клиентов. Prometheus подключается
к сети проекта и scrape `platform:8081`; не публикуйте management ради scrape.

## Применить миграции и запустить

Дальше `$RELEASE_ENV` — абсолютный путь к защищённому env-файлу.
В примерах используется Docker Compose v2; имя проекта должно быть отдельным.
При standalone Compose замените `docker compose` на `docker-compose`.
Все три значения image в env должны соответствовать проверенному манифесту.
Перед миграцией сохраните согласованный backup и проверьте восстановление
на отдельном PostgreSQL. Не запускайте одновременно два release job.

```sh
docker compose --env-file "$RELEASE_ENV" -p platform-v1 \
  -f deploy/platform/docker-compose.release.yml run --rm --no-deps platform-migrate up
docker compose --env-file "$RELEASE_ENV" -p platform-v1 \
  -f deploy/platform/docker-compose.release.yml run --rm --no-deps platform-migrate up
```

Второй запуск должен успешно сверить ledger и контрольные суммы без новых
изменений. Не исправляйте ledger вручную. При checksum mismatch остановите
выпуск и сопоставьте манифест миграций с артефактом и backup.

Для первоначального bootstrap примените один и тот же утверждённый lifecycle
manifest к Core его штатной утилитой и к Platform:

```sh
docker compose --env-file "$RELEASE_ENV" -p platform-v1 \
  -f deploy/platform/docker-compose.release.yml --profile operations \
  run --rm --no-deps platform-lifecycle bootstrap-platform /manifests/pilot.json
docker compose --env-file "$RELEASE_ENV" -p platform-v1 \
  -f deploy/platform/docker-compose.release.yml up -d --wait platform
```

Проверьте management readiness из Docker-сети, public access с действующим
proof, manager чтения/команды через Manager BFF mTLS и соответствие `--version`
release manifest. Health сам по себе не доказывает эти операции. Проверьте
сохранённый manual term через публичный access, feed/history UUID, точные
повторы command IDs и доставку lifecycle после рестарта. Ручная реальная
amoCRM-проверка остаётся отдельным P10 действием владельца.

## Наблюдать и реагировать

`deploy/platform/prometheus.yml` и `alerts.yml` предназначены для Prometheus
в release Docker-сети. Для нескольких реплик перечислите каждый scrape target.
Shared-DB gauges (`queue_depth`, `oldest_ready_seconds`, `expired_leases`)
объединяются через max, а не sum. `queue_depth` имеет только queued/running;
dead отслеживается по process counter outcomes `dead|recovered_dead`.
Суммируются increase process counters, включая database errors. Ошибка
snapshot отключает зависимые backlog/lease alerts: прошлое значение gauge
не считается свежими данными. Отдельно контролируются snapshot_ok и up==0.

Пороги — стартовые эксплуатационные defaults: scrape/snapshot отсутствуют
2 минуты, ready job старше 300 секунд ещё 5 минут, expired leases сохраняются
2 минуты, любой dead transition за 5 минут, рост database errors 2 минуты.
Это не бизнес-gates и не SLA; оператор настраивает их по нагрузке.
При snapshot/database error проверьте доступность БД и grants, не считайте
пустую очередь доказательством восстановления. При dead сохраните исходный
ID и исследуйте безопасный код ошибки; повтор побочного эффекта допустим
только после установления результата. Не включайте payload/DSN в журнал инцидента.

## Откатить приложение

Допустимая база отката — полный P09 API commit `093cdb5` (используйте полный
SHA из git и отдельный проверенный manifest). P08 не содержит feed/history и
не является V1 rollback target. Схему не откатывайте: оставьте применённые
миграции и все бизнес-факты. Остановите текущие реплики, смените только
`PLATFORM_IMAGE` на проверенный совместимый P09 artifact, затем запустите
Platform с прежними DSN, ключами и TLS. Проверьте readiness, feed/history,
manual term/access и повторы команд. Возврат текущего артефакта выполняется
тем же способом. Не используйте `down -v` и миграции `down` для rollback.

## Восстановить PostgreSQL

1. Остановите входящий lifecycle и менеджерские записи, остановите Platform
   workers/реплики. Зафиксируйте точку остановки и сохраните текущую БД для
   расследования. Не восстанавливайте dump поверх работающего приложения.
2. Восстановите согласованный dump и роли/grants в отдельную пустую БД
   PostgreSQL 17. Сверьте схему и ledger, затем выполните проверенный мигратор
   дважды. Не запускайте Core bootstrap заново с новым manifest без решения
   владельца области.
3. Подключите одну реплику к восстановленной БД, сохранив identity и cursor
   key. Проверьте manager feed/history, manual term/public access, receipt
   точных повторов, очередь и backlog. Истёкшие lease должна восстановить
   штатная очередь; не меняйте её строки вручную.
4. Сопоставьте lifecycle source с восстановленной точкой. Replay выполняйте
   через штатную Core lifecycle утилиту с исходными installation UUID и
   диапазоном sequence. После проверки идемпотентности возобновите доставку
   и записи, затем остальные реплики. Зафиксируйте возможный интервал потери
   данных согласно backup RPO.

Изолированный `scripts/platform-recovery-test.sh` проверяет сценарии restore,
повтора миграций и rollback на синтетической БД с текущим и
P09 артефактами. Его успешный прогон не заменяет регулярное восстановление
операторского backup в отдельном окружении.

```sh
PLATFORM_RECOVERY_CURRENT_IMAGE="$PLATFORM_IMAGE" \
PLATFORM_RECOVERY_MIGRATE_IMAGE="$PLATFORM_MIGRATOR_IMAGE" \
PLATFORM_RECOVERY_ROLLBACK_IMAGE="$P09_PLATFORM_IMAGE_ID" \
PLATFORM_RECOVERY_TEST_IMAGE="$TEST_BASE_IMAGE_ID" \
  sh scripts/platform-recovery-test.sh
```

Все четыре входа recovery должны быть полными `sha256:` image IDs. Test-base
собирается из текущего исходного commit. Скрипт извлекает tzdata из каждого
образа Platform и передаёт его бинарю через `ZONEINFO` в соответствующей фазе.
PostgreSQL по умолчанию использует
`postgres:17-alpine`, разрешённый в image ID до запуска. Скрипт не принимает
внешние DSN, создаёт собственные source и restore базы и удаляет их при выходе.
