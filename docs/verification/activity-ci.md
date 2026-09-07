# Activity: обязательные проверки CI

GitHub workflow `.github/workflows/ci.yml` сохраняет прежние contract, test и
Core PostgreSQL integration jobs и добавляет отдельный `activity` job. Все
production image builds зависят от его успеха; матрица включает `activity`,
`crm-events`, `service-certs` и `activity-control` вместе с прежними образами.
Проверяется синтаксис основного Compose, Activity grpc, embedded и test overlay.

Локальный эквивалент (Docker Compose с поддержкой `!override` и `up --wait`):

```sh
make COMPOSE="docker compose" activity-ci
```

`activity-ci` удаляет только ресурсы выделенного тестового проекта до и после
запуска, в том числе при ошибке. По умолчанию это `amocrm-pro-activity-test`.
Явный `-p` имеет приоритет над `name: amocrm-activity` и переменной
`COMPOSE_PROJECT_NAME`. Имя `ACTIVITY_TEST_PROJECT` обязано заканчиваться на
`-test`: проверка выполняется до запуска и удаления ресурсов, поэтому имена
основного проекта `amocrm-pro` и пилота `amocrm-activity` запрещены. Выбирайте
только отдельное имя, предназначенное для удаления. Команды `activity-up` и
`activity-embedded` также используют явный `-p amocrm-activity`, чтобы
унаследованный `COMPOSE_PROJECT_NAME` не направил их в основной проект.
`make activity-test` запускает те же проверки,
но сохраняет тестовый PostgreSQL и кеш для повторного запуска.

Порядок выполнения:

1. Запустить отдельный PostgreSQL и дождаться healthcheck. Идемпотентный
   `deploy/activity/init-tests.sh` создаёт `core_components_test`,
   `activity_components_test` и `events_components_test`.
2. Собрать test/migration образы, применить Core migrations. Профиль `tests`
   включён явно, поэтому dependency graph работает и с чистого окружения.
3. Запустить race-enabled Go suites. `-p 1` исключает одновременную очистку
   общих тестовых схем разными пакетами. Activity и CRM Events применяют свои
   миграции в собственных БД. RPC-тесты используют настоящие PostgreSQL и mTLS;
   процессные тесты создают временные БД, собственные runtime identities и
   реальные OS-процессы. Они проверяют embedded/grpc, потерянные ответы,
   перезапуск, fencing и недоступность Gateway.
4. Проверить присутствие PASS для обязательных owner/RPC/process тестов.
   Пропуски разрешены только для трёх helper entrypoints, которые запускаются
   родительскими тестами как дочерние процессы. Неуказанные DB-переменные,
   пропущенная обязательная проверка или ошибка теста завершают job с ошибкой.
5. Перед Go suite удалить старый operation fixture. PostgreSQL-тест создаёт
   новый JSON успешной операции; Node 22 использует этот JSON для тестов
   реальной панели. Отсутствующий fixture и любой UI skip завершают job с ошибкой.

Логи сохраняются в `tmp/activity-v0-evidence/`: `activity-go.log`,
`activity-ui.log`, `crm-operation.json`, `compose-cleanup.log`. GitHub также
сохраняет `activity-compose.log` и публикует каталог как artifact
`activity-verification`, включая неуспешные прогоны.

Это проверки текущих исходников с синтетическим amoCRM upstream. Они не
подменяют проверку установленного виджета в реальном аккаунте и не доказывают
произвольную межрелизную совместимость. Опциональный прогон прежнего бинаря
Core API и queue performance benchmarks в этот job не включены. Статус job
GitHub можно утверждать только после фактического запуска workflow.

Для адресного запуска Go-теста после подготовки тестового стека переопределяйте
entrypoint явно: `component-tests` по умолчанию выполняет shell runner с полным
набором проверок, а не принимает аргументы `go test` напрямую.

```sh
docker compose -p amocrm-pro-activity-test --profile tests \
  -f docker-compose.activity.yml -f docker-compose.activity-tests.yml \
  run --rm --no-deps --entrypoint go component-tests \
  test -race -count=1 -v -run '^TestComponentOSProcessFaults$' ./internal/componentruntime
```

Такой адресный запуск не заменяет `activity-ci`: он обходит проверки обязательных
PASS и UI fixture, предназначенные для полного прогона.

## Особенности Linux runner

Процессные тесты собирают временные бинарники с `-buildvcs=false`: checkout
смонтирован в контейнер, и владелец `.git` может отличаться от UID процесса.
Без этого Go может завершиться `error obtaining VCS status: exit status 128`
ещё до запуска проверяемого сервиса. VCS stamping тестовым бинарникам не нужен;
глобальное разрешение Git `safe.directory=*` не используется.

`crm-operation.json` содержит только синтетический ответ тестового получателя
и создаётся с правами `0644`, включая повторную запись поверх старого файла.
Это позволяет host runner загрузить artifact после запуска тестов от root в
контейнере. Private keys и настоящие данные авторизации в этот файл не попадают.
