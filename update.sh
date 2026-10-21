#!/bin/bash
#
# update.sh — обновление бекенда amocrm-pro из GitHub.
# Каноническая версия хранится в репозитории. Серверный wrapper должен
# вызывать этот файл (см. docs/runbooks/core-update.md).
#
# Сохраняет compose overlays существующего worker, включая Task Control и
# Distribution. Учитывает Activity v0 / CRM Events:
#   - если выбран docker-compose.v0.yml, пересобирает также activity, crm-events,
#     certificates и tools (integrations/activity-control), а после старта
#     проверяет здоровье API, Worker, CRM Events, Activity и графа;
#   - если v0 overlay не выбран — работает с базовым стеком как раньше
#     (api/worker/migrate).
#
# Использование:
#   update.sh [branch] | update.sh --check
#   PROJECT_DIR=/path/to/amocrm-pro update.sh [branch]
#
set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="${PROJECT_DIR:-$SCRIPT_DIR}"
BRANCH="${1:-}"
CHECK_ONLY=0
if [ "$BRANCH" = --check ]; then CHECK_ONLY=1; BRANCH=""; fi

log_info()  { echo -e "${GREEN}[INFO]${NC}  $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# --- Современный Docker Compose (plugin или standalone) ---
COMPOSE_CMD=()
if docker compose version >/dev/null 2>&1; then
    COMPOSE_CMD=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
    COMPOSE_CMD=(docker-compose)
else
    log_error "Docker Compose не найден: нет ни 'docker compose', ни 'docker-compose'"
    exit 1
fi
log_info "Используется Docker Compose: ${COMPOSE_CMD[*]}"

# --- Переход в директорию проекта ---
cd "$PROJECT_DIR" || {
    log_error "Не удалось перейти в директорию $PROJECT_DIR"
    exit 1
}
PROJECT_DIR="$(pwd -P)"
log_info "Рабочая директория: $(pwd)"

if ! git rev-parse --git-dir >/dev/null 2>&1; then
    log_error "Это не git репозиторий"
    exit 1
fi
command -v python3 >/dev/null || { log_error "Требуется python3 для проверки конфигурации"; exit 1; }
BACKUP_FILE="$(git rev-parse --git-path update-last-state)"

# Снять список ДО git pull: удалённый/пропавший overlay должен прервать
# обновление, а не молча отключить уже работающий listener. Explicit -f
# используется далее для КАЖДОЙ команды, включая down и health checks.
DEPLOYED_FILES="${UPDATE_COMPOSE_FILES:-}"
if [ -z "$DEPLOYED_FILES" ]; then
    WORKER_ID=$("${COMPOSE_CMD[@]}" -f docker-compose.yml ps --all --quiet worker)
    if [ -n "$WORKER_ID" ]; then
        if [[ "$WORKER_ID" = *$'\n'* ]]; then
            log_error "Несколько worker: задайте UPDATE_COMPOSE_FILES явно"; exit 1
        fi
        DEPLOYED_FILES=$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project.config_files" }}' "$WORKER_ID")
        if [ -z "$DEPLOYED_FILES" ] || [ "$DEPLOYED_FILES" = '<no value>' ]; then
            log_error "У worker нет списка compose files: задайте UPDATE_COMPOSE_FILES явно"; exit 1
        fi
    fi
fi

select_compose_files() {
    COMPOSE_FILES=()
    V0_OVERLAY=0
    TASK_CONTROL_OVERLAY=0
    DISTRIBUTION_OVERLAY=0
    local files file
    if [ -n "$DEPLOYED_FILES" ]; then
        IFS=',' read -r -a files <<< "$DEPLOYED_FILES"
    else
        files=(docker-compose.yml)
        if [ -f docker-compose.v0.yml ]; then files+=(docker-compose.v0.yml); fi
    fi
    for file in "${files[@]}"; do
        if [ ! -f "$file" ]; then log_error "Отсутствует compose overlay: $file"; return 1; fi
        COMPOSE_FILES+=(-f "$file")
        case "$file" in
            *docker-compose.v0.yml) V0_OVERLAY=1 ;;
            *docker-compose.task-control.yml) TASK_CONTROL_OVERLAY=1 ;;
            *core-distribution.local.yaml|*docker-compose.distribution.yml) DISTRIBUTION_OVERLAY=1 ;;
        esac
    done
    log_info "Compose files: ${COMPOSE_FILES[*]}"
    "${COMPOSE_CMD[@]}" "${COMPOSE_FILES[@]}" config --format json |
        python3 "$PROJECT_DIR/scripts/check-update-compose.py" "$TASK_CONTROL_OVERLAY" "$DISTRIBUTION_OVERLAY"
}

if [ "$CHECK_ONLY" -eq 1 ]; then
    select_compose_files
    log_info "Конфигурация проверена; Git и контейнеры не изменялись"
    exit 0
fi

# --- Проверка незакоммиченных изменений ---
if ! git diff-index --quiet HEAD -- 2>/dev/null; then
    log_error "Есть изменения tracked-файлов; сохраните их перед обновлением"
    exit 1
fi

CURRENT_BRANCH=$(git rev-parse --abbrev-ref HEAD)
OLD_COMMIT=$(git rev-parse HEAD)
log_info "Текущая ветка: $CURRENT_BRANCH"

# --- Переключение ветки, если указана ---
if [ -n "$BRANCH" ] && [ "$BRANCH" != "$CURRENT_BRANCH" ]; then
    log_info "Переключение на ветку: $BRANCH"
    git fetch origin
    git checkout "$BRANCH" || {
        log_error "Не удалось переключиться на ветку $BRANCH"
        exit 1
    }
fi

# --- Бэкап состояния до обновления (для отката) ---
printf 'branch=%s\ncommit=%s\nproject=%s\n' "$CURRENT_BRANCH" "$OLD_COMMIT" "$PROJECT_DIR" > "$BACKUP_FILE"
log_info "Состояние до обновления сохранено в $BACKUP_FILE"

# --- git pull ---
log_info "Получение обновлений из GitHub..."
if ! git pull --ff-only origin "$(git rev-parse --abbrev-ref HEAD)"; then
    log_error "Не удалось выполнить git pull"
    exit 1
fi
NEW_COMMIT=$(git rev-parse HEAD)

if [ "$OLD_COMMIT" = "$NEW_COMMIT" ]; then
    log_info "Нет новых изменений"
    log_info "Текущий коммит: $NEW_COMMIT"
else
    log_info "Обновлено: $OLD_COMMIT -> $NEW_COMMIT"
    log_info "Изменения:"
    git log --oneline "$OLD_COMMIT..$NEW_COMMIT"
fi

# --- Ревизия сборки: полный SHA в бинарь и OCI label ---
# docker-compose.yml подставляет ${BUILD_REVISION} в build args и в
# org.opencontainers.image.revision. Экспорт гарантирует, что --version
# работающих бинарей совпадёт с проверенным commit, а не будет 'unknown'.
export BUILD_REVISION="$NEW_COMMIT"
log_info "BUILD_REVISION=$BUILD_REVISION"

# Проверить итоговое слияние, включая secrets и loopback-порты, до остановки.
select_compose_files

# --- Сборка образов ---
BUILD_SERVICES=(api worker migrate)
if [ "$V0_OVERLAY" -eq 1 ]; then
    BUILD_SERVICES+=(certificates activity crm-events)
fi
log_info "Сборка Docker образов: ${BUILD_SERVICES[*]}"
if [ "$V0_OVERLAY" -eq 1 ]; then
    # tools (integrations/activity-control) собираются для штатных операторских команд
    COMPOSE_PROFILES=tools "${COMPOSE_CMD[@]}" "${COMPOSE_FILES[@]}" build "${BUILD_SERVICES[@]}" integrations activity-control || {
        log_error "Не удалось собрать образы (включая tools)"
        exit 1
    }
else
    "${COMPOSE_CMD[@]}" "${COMPOSE_FILES[@]}" build "${BUILD_SERVICES[@]}" || {
        log_error "Не удалось собрать образы"
        exit 1
    }
fi

# --- Остановка только после успешной проверки и сборки ---
log_info "Остановка контейнеров..."
"${COMPOSE_CMD[@]}" "${COMPOSE_FILES[@]}" down --remove-orphans || {
    log_error "Не удалось остановить контейнеры"
    exit 1
}

# --- Запуск стека (миграции и генерация сертификатов — one-shot сервисы) ---
log_info "Запуск обновленного стека..."
"${COMPOSE_CMD[@]}" "${COMPOSE_FILES[@]}" up --detach || {
    log_error "Не удалось запустить стек"
    exit 1
}

log_info "Ожидание запуска сервисов..."
sleep 5

log_info "Статус сервисов:"
"${COMPOSE_CMD[@]}" "${COMPOSE_FILES[@]}" ps

# --- Проверка здоровья: ожидание http-ответа внутри контейнера сервиса ---
wait_healthy() { # wait_healthy SERVICE URL
    local service="$1" url="$2" ok=0
    for _ in $(seq 1 60); do
        if "${COMPOSE_CMD[@]}" "${COMPOSE_FILES[@]}" exec -T "$service" wget -q -O /dev/null "$url" 2>/dev/null; then
            ok=1
            break
        fi
        sleep 1
    done
    if [ "$ok" -ne 1 ]; then
        log_error "$service не ответил на $url в течение 60 секунд"
        "${COMPOSE_CMD[@]}" "${COMPOSE_FILES[@]}" logs --tail=50 "$service" || true
        return 1
    fi
    log_info "✓ $service готов ($url)"
    return 0
}

fail=0
log_info "Проверка здоровья API..."
wait_healthy api http://127.0.0.1:8082/live || fail=1

log_info "Проверка здоровья Worker..."
wait_healthy worker http://127.0.0.1:8081/live || fail=1

if [ "$DISTRIBUTION_OVERLAY" -eq 1 ]; then
    wait_healthy worker http://127.0.0.1:8084/components/distribution/ready || fail=1
fi
if [ "$TASK_CONTROL_OVERLAY" -eq 1 ]; then
    # Без JWT ожидается 401. Проверяет listener/маршрут, не полноценный auth-flow.
    status=$("${COMPOSE_CMD[@]}" "${COMPOSE_FILES[@]}" exec -T worker wget -S -O /dev/null --post-data='' http://127.0.0.1:8087/api/v1/widget/task-control/auth 2>&1 | awk '/HTTP\// {print $2}' | tail -n1) || true
    if [ "$status" != 401 ]; then log_error "Task Control auth: ожидался 401"; fail=1; fi
fi

if [ "$V0_OVERLAY" -eq 1 ]; then
    log_info "Проверка здоровья CRM Events..."
    wait_healthy crm-events http://127.0.0.1:8092/ready || fail=1

    log_info "Проверка здоровья Activity..."
    wait_healthy activity http://127.0.0.1:8091/ready || fail=1

    log_info "Проверка готовности графа (api -> gateway/activity/crm-events)..."
    wait_healthy api http://127.0.0.1:8082/components/activity/ready || fail=1
fi

# --- Проверка ревизии именно работающих бинарей (--version == BUILD_REVISION) ---
# Сверяем не образ, а процесс в контейнере: иначе checkout мог бы совпадать,
# а работать старый бинарь. Прямая сборка без BUILD_REVISION даёт 'unknown'.
verify_revision() { # verify_revision SERVICE BINARY
    local service="$1" binary="$2" actual
    actual=$("${COMPOSE_CMD[@]}" "${COMPOSE_FILES[@]}" exec -T "$service" "$binary" --version 2>/dev/null | tr -d '\r' | head -n1)
    if [ "$actual" != "$BUILD_REVISION" ]; then
        log_error "Ревизия $service ($binary): ожидалось '$BUILD_REVISION', получено '$actual'"
        return 1
    fi
    log_info "✓ $service $binary --version = $actual"
    return 0
}

log_info "Проверка ревизии работающих бинарей..."
verify_revision api amocrm-api || fail=1
verify_revision worker amocrm-worker || fail=1
if [ "$V0_OVERLAY" -eq 1 ]; then
    verify_revision activity activity || fail=1
    verify_revision crm-events crm-events || fail=1
fi

if [ "$fail" -ne 0 ]; then
    log_error "Один или несколько сервисов не прошли проверку здоровья или ревизии"
    exit 1
fi

log_info "✓ Обновление успешно завершено!"
log_info "Текущий коммит: $NEW_COMMIT"
log_info ""
log_info "Полезные команды:"
log_info "  ${COMPOSE_CMD[*]} ${COMPOSE_FILES[*]} logs -f api worker activity crm-events — просмотр логов"
log_info "  ${COMPOSE_CMD[*]} ${COMPOSE_FILES[*]} ps                  — статус сервисов"
log_info "  ${COMPOSE_CMD[*]} ${COMPOSE_FILES[*]} down                — остановка стека"
