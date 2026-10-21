# Обновление Core с Task Control и Distribution

Канонический updater — [`update.sh`](../../update.sh) в корне репозитория.
Он перенесён из server-local скрипта pi после инцидента 21.10.2026:
старый updater применял только base + v0, и после `down --remove-orphans`
worker терял listeners 8084/8087 и host-порты 18084/18087.

## Первый переход со старого updater

Одного `git pull` недостаточно, чтобы заменить скрипт вне репозитория.
После слияния PR сначала получите новый updater **без запуска старого**:

```sh
cd /root/amocrm-pro
git pull --ff-only origin main
./update.sh --check
./update.sh main
```

Серверный `/root/.pi/agent/skills/update-amocrm-backend/update.sh` после
сохранения резервной копии замените wrapper, вызывающим репозиторный файл:

```bash
#!/bin/bash
set -euo pipefail
exec "${PROJECT_DIR:-/root/amocrm-pro}/update.sh" "$@"
```

Это отдельный шаг оператора на сервере. Локальный PR сам его не выполняет.
Секреты, `.env`, descriptor, binding и generation updater не изменяет.

## Выбор конфигурации

До получения новых исходников updater читает label
`com.docker.compose.project.config_files` существующего worker
(в том числе остановленного). Один и тот же список в исходном порядке
используется для config, build, down, up и проверок. Если файл пропал,
обновление завершается **до остановки контейнеров**. Ошибка config или build
также оставляет текущие контейнеры работающими.

Для текущего staging порядок такой:

1. `docker-compose.yml`
2. `docker-compose.v0.yml`
3. `docker-compose.task-control.yml`
4. `deploy/distribution-staging/core-distribution.local.yaml`
5. `deploy/task-control-staging/docker-compose.loopback.yml`

Последний слой обязателен после Distribution: `ports: !override` в
Distribution удалит ранее добавленный порт Task Control, если поменять порядок.
Preflight проверяет итоговый JSON Compose: listeners, обязательные настройки,
TLS proxy и наличие портов только на `127.0.0.1`. Полный config с секретами
в stdout или временный файл не записывается. Нужны Bash, Python 3 и современный
Docker Compose с `config --format json` и `!override`.

Если worker ещё не создан, по умолчанию выбираются base + v0 при наличии.
Для первой установки виджетов или восстановления стека явно задайте список
(те же пути, разделённые запятыми, без пробелов вокруг запятых):

```sh
export UPDATE_COMPOSE_FILES='docker-compose.yml,docker-compose.v0.yml,docker-compose.task-control.yml,deploy/distribution-staging/core-distribution.local.yaml,deploy/task-control-staging/docker-compose.loopback.yml'
./update.sh --check
./update.sh main
```

Сохраняйте имя Compose-проекта и его `.env` (`COMPOSE_PROJECT_NAME`, если задан),
иначе будет выбран другой стек. Tracked-изменения прерывают обновление;
Git обновляется только fast-forward. Сборка проходит до остановки стека,
но обновление всё ещё включает `down`/`up` и требует окна простоя.

После запуска проверяются версии бинарников и прежние health endpoints,
Distribution ready и ожидаемый 401 Task Control auth без JWT. Эти проверки
не заменяют браузерный auth-flow и R09/R10. При неуспехе updater возвращает
ненулевой код, автоматического отката нет; сохранённый SHA находится в
Git metadata `update-last-state`. Для отката используйте тот же полный
список overlays, не команду перехода к одному base compose.
