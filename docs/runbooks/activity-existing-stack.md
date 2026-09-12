# Activity в существующем Core-стеке

[docker-compose.v0.yml](../../docker-compose.v0.yml) — overlay для
[docker-compose.yml](../../docker-compose.yml). Сохранён из конфигурации
целевого тестового стенда Devcorps. Параметры сервисов соответствуют
предоставленному серверному файлу; изменён только поясняющий заголовок.

В отличие от docker-compose.activity.yml, overlay использует существующий
Core PostgreSQL volume и Core-БД (на стенде — amocrm), интеграции и OAuth.
Activity и CRM Events подключаются к отдельным логическим БД того же
PostgreSQL. API обращается к продуктам по gRPC/mTLS; worker/Gateway остаётся
единственным владельцем бюджета запросов amoCRM.

## Подготовка

- Сохранить исходный Compose project name. На Devcorps это amocrm-pro.
  Смена имени создаст другие именованные volumes вместо подключения прежних.
- Сохранить резервные копии БД и секретов по
  [runbook восстановления](activity-backup-restore.md).
- В PostgreSQL заранее должны быть БД amocrm_activity и amocrm_events,
  роли activity_owner/activity_runtime и events_owner/events_runtime,
  правильные CONNECT/USAGE/DML и default privileges.
  Runtime не должен иметь DDL или доступа к чужим owner-БД.
  Overlay не создаёт роли/БД. Не запускать development init-db.sh
  поверх существующего кластера: он предназначен для fresh cluster.
- Существующие DATABASE_URL, ENCRYPTION_KEYS и остальные настройки Core
  сохранить в серверном .env. Не заменять их примерами из base Compose.
- В .env нужны ACTIVITY_OWNER_PASSWORD, ACTIVITY_RUNTIME_PASSWORD,
  EVENTS_OWNER_PASSWORD, EVENTS_RUNTIME_PASSWORD.
  Они интерполируются в PostgreSQL URI: специальные символы пароля должны
  быть percent-encoded в URI, соответствуя фактическому паролю роли.
- Для share-панелей задать ACTIVITY_APP_ORIGINS,
  ACTIVITY_APP_PUBLIC_ORIGIN и ACTIVITY_MANAGEMENT_TOKEN.
  ORIGINS — точные HTTPS origins по формату конфигурации Core;
  PUBLIC_ORIGIN — адрес viewer-сайта без пути/hash.
- .env хранить с правами 0600 вне Git. Полный вывод compose config содержит
  подставленные секреты: не публиковать его и не сохранять в общедоступные файлы.

## Проверка и запуск

Из корня checkout, с подготовленным серверным .env:

~~~sh
docker compose -p amocrm-pro -f docker-compose.yml -f docker-compose.v0.yml config --quiet
docker compose -p amocrm-pro -f docker-compose.yml -f docker-compose.v0.yml up -d --build api worker activity crm-events
docker compose -p amocrm-pro -f docker-compose.yml -f docker-compose.v0.yml ps
curl --fail http://127.0.0.1:8082/ready
~~~

Перед сборкой задать BUILD_REVISION по фактически выпускаемому commit;
для checkout с изменениями использовать отметку dirty. Иначе значение будет unknown.
Параметры портов base Compose могут быть переопределены через .env;
пример readiness выше использует стандартный порт.

API сохраняет зависимость от Core migration; Activity/Events ожидают
соответствующие owner migrations. Certificates создаёт identities при
первом запуске и затем использует существующие volumes.
API /live не заменяет /ready всего графа.

activity-control здесь настроен как Core CLI для pilot/list/inspect/retry.
Для panel-* HTTP-команд дополнительно нужны API_BASE_URL=http://api:8080
и ACTIVITY_MANAGEMENT_TOKEN, переданные в контейнер; автоматически этому
сервису они не назначаются. Токен не размещать в аргументах или shell history.

## Ограничения и откат

Это конфигурация **тестового стенда**, не production hardening.
Она не исправляет обнаруженную на Devcorps роль amocrm-superuser для Core
и PUBLIC CONNECT к Core-БД. См.
[аудит CORE-06](../verification/stages1-9-2026-09-12/README.md).
Service-certs identities — тестовый механизм; production-ротация описана
в [secrets-rotation](secrets-rotation.md).

Переход на base Compose выключает Activity-интеграцию. Это не откат схемы
или удаление истории. Сначала отключить Activity pilot нужных installations
штатным CLI, остановить новые команды и проверить незавершённую работу
по [Activity runbook](activity-v0.md).
Не переключать конфигурацию посреди незавершённых операций.

После подготовки использовать тот же project name и прежние Core-секреты:

~~~sh
docker compose -p amocrm-pro -f docker-compose.yml up -d --remove-orphans
~~~

Именованные volumes и БД этой командой не удаляются. Не использовать
down --volumes или prune как способ отката. Прежние бинарные версии и
совместимость миграций проверяются отдельно; этот шаг сам по себе
не возвращает предыдущую версию приложения.

Если на сервере уже лежит untracked docker-compose.v0.yml, перед первым
git pull сохранить его копию вне checkout и сравнить с tracked-версией.
Git не перезапишет конфликтующий untracked-файл автоматически.
Секреты .env, identity volumes и данные при добавлении файла в Git
не переносятся и не заменяются.
