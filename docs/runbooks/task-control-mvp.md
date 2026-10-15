# Контроль задач: выпуск сквозного MVP 1.1.0

Изменения относятся только к Контролю задач. Правила хранятся в company backend TeamOS;
Core проверяет JWT amoCRM и установку, но не получает правил. Проверка и создание задач
остаются в браузере amoCRM. Это инструкция для оператора; команды production в ходе разработки
не запускались. Сначала проверьте на одном тестовом аккаунте.

## Что обновить

Четыре согласованных PR: amocrm-pro, team-os-backend, team-os, rakurs-widgets-new.
Порядок: Core → backend TeamOS → TeamOS UI → пакет виджета. Зафиксируйте предыдущие
commit/image digest и сохраните backup обеих БД. Не удаляйте старый runtime_config/каталог amoCRM.
Адреса Core и backend в архиве должны совпадать с реально опубликованными HTTPS origins.

## 1. Core

Обновите checkout на проверенный commit PR, соберите образы API, worker, migrate,
integrations и task-control-grant штатным процессом. Сохраните существующие параметры
DB/шифрования/OAuth/очередей. Примените миграции по 000024 включительно штатным migrate.

Новый listener worker включается отдельным overlay `docker-compose.task-control.yml`.
Он слушает 8087 только внутри Docker network. Переменные:

- `TASK_CONTROL_SERVICE_KEYS` — JSON `{ "teamos-task-control": "случайный секрет минимум 32 символа" }`.
- `TASK_CONTROL_ASSERTION_KEY_ID` — например `task-control-v1`.
- `TASK_CONTROL_ASSERTION_PRIVATE_KEY` — base64 Ed25519 private key 64 байта (не PEM).
- Overlay задаёт `TASK_CONTROL_HTTP_ADDRESS=:8087`, `TASK_CONTROL_TLS_PROXY=true`.

Создайте отдельную Ed25519 пару и status HMAC secret через secret manager. Публичный ключ
32 байта передайте TeamOS; private key и client secret amoCRM остаются только в Core.
Не используйте ключи из tests/fixtures. Не меняйте существующие ключи других сервисов.

Штатный CLI integrations теперь принимает `--services task-control` и `--service task-control`.
Для уже зарегистрированной интеграции:

```sh
docker compose run --rm integrations set-service \
  --actor YOUR_OPERATOR --code YOUR_WIDGET_CODE --service task-control --enabled true
```

Если автономный виджет ещё не зарегистрирован в Core, следуйте
[инструкции provisioning](integrations.md): зарегистрируйте **его действительный OAuth client UUID**,
redirect URI и secret через stdin; задайте только `--services task-control`.
Обновление архива не заменяет OAuth-авторизацию: должна появиться active installation
этой интеграции для тестового аккаунта. Не используйте installation Распределения.

После установки выдайте scoped grant конкретной company TeamOS и installation Core:

```sh
docker compose -f docker-compose.yml -f docker-compose.task-control.yml run --rm task-control-grant \
  --key-id teamos-task-control --company-id TEAMOS_COMPANY_UUID \
  --installation-id CORE_INSTALLATION_UUID --actor YOUR_OPERATOR --enabled true
```

Reverse proxy направляет на `worker:8087`:

- публично только `POST/OPTIONS /api/v1/widget/task-control/auth`;
- `/internal/v1/task-control/accounts/*` — только с сети backend TeamOS, по TLS;
  запрос дополнительно подписан отдельным status secret.

Пример location для существующего nginx TLS vhost (имя upstream уточните в вашем deploy):

```nginx
location = /api/v1/widget/task-control/auth {
    proxy_pass http://worker:8087;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto https;
}
# В отдельном приватном vhost, доступном только backend TeamOS:
location /internal/v1/task-control/accounts/ {
    proxy_pass http://worker:8087;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto https;
}
```

Не публикуйте весь порт 8087 наружу. Существующие `/oauth/amocrm/callback` и маршруты
других виджетов остаются на своих upstream. Секреты, X-Auth-Token и тела proof/session
не включайте в access/debug логи.

## 2. Backend TeamOS

Обновите company и gateway вместе, примените company миграции 000032 и 000033.
Сохраните действующую verified `company_integrations` связь amoCRM-аккаунта;
новое подключение не создаёт компанию и не выдаёт пользователям роли.

В company задайте:

- `COMPANY_TASK_CONTROL_CORE_URL` — приватный HTTPS origin Core, без пути;
- `COMPANY_TASK_CONTROL_KEY_ID=teamos-task-control`;
- `COMPANY_TASK_CONTROL_SECRET` — соответствующий status HMAC secret;
- `COMPANY_TASK_CONTROL_ASSERTION_PUBLIC_KEYS` — JSON keyId → base64 public key.

Gateway и company используют существующий service credential для RPC.
Публичный backend HTTPS публикует `/api/v1/widget/task-control/sessions` и `/bootstrap`.
CORS должен разрешать реальный amoCRM origin. Проверяйте preflight в браузере;
CORS не заменяет авторизацию. Management API остаётся под существующей сессией TeamOS.

## 3. TeamOS и пакет

Соберите frontend штатно (`npm ci`, `npm run build`) с существующим адресом gateway.
В «Интеграции» появится «Контроль задач», привязанный к amoCRM-аккаунту компании.

В rakurs-widgets-new/rkrs_lead_notasks выполните:

```sh
TASK_CONTROL_CORE_ORIGIN=https://PUBLIC_CORE_HOST \
TASK_CONTROL_BACKEND_ORIGIN=https://PUBLIC_TEAMOS_BACKEND_HOST \
sh scripts/build.sh
```

Архив `../rkrs_lead_notasks.zip` содержит runtime version 1.1.0 и публичные origins,
но не секреты. Установите его в ту же приватную amoCRM-интеграцию. До явного включения
TeamOS прежние локальные правила работают по прежнему runtime_config.

1. Администратор amoCRM: «Код подключения» в настройках виджета.
2. Администратор TeamOS: вставить код в «Интеграции → Контроль задач» в течение 60 секунд.
3. Создать правила, сохранить черновик и опубликовать. В MVP укажите CRM IDs вручную.
4. Получить «Код смены режима» в amoCRM, вставить в TeamOS и включить правила TeamOS.
5. В amoCRM включить «Правила из TeamOS», нажать основную кнопку «Сохранить», перезагрузить вкладки.

## Приёмка и возврат

- Установленный виджет активен; подтверждённо удалённый неактивен; outage показывает unknown.
- Менеджер из правила: сделка без незавершённой задачи ответственного показывает контроль;
  после создания задачи ограничение снимается. Менеджер вне правила не затронут.
- Сохранение черновика не меняет исполнение. Публикация меняет его в течение 60 секунд
  на открытой вкладке. Проверить пользователей, воронки/статусы и покупателей отдельно.
- Неверный аккаунт в proof, истёкший/повторный proof и сотрудник без прав получают отказ.
- Вторая вкладка редактора получает конфликт ревизии, не перезаписывает изменения молча.
- При недоступности TeamOS вкладка хранит последнее корректное правило в памяти;
  после перезагрузки без сети managed не подменяется старым standalone.

Для возврата сначала с новым кодом режима переключите TeamOS в standalone, затем выключите
«Правила из TeamOS» в amoCRM, сохраните и перезагрузите вкладки. Старый каталог/runtime_config
не менялись. При необходимости возвращайте прежние app images после отключения managed;
аддитивные миграции/данные оставьте, не делайте down с удалением правил. Переход на старый
архив возвращает старую автономную логику и должен быть явным решением оператора.

### Удаление виджета во время пилота

Статус установки берётся из реестра Core. В существующей инфраструктуре нет
автоматического обработчика удаления интеграции из интерфейса amoCRM. После удаления
подтвердите это штатной командой (только для фактически удалённого виджета):

```sh
docker compose run --rm integrations uninstall \
  --actor YOUR_OPERATOR --code YOUR_WIDGET_CODE --installation-id CORE_INSTALLATION_UUID
```

TeamOS увидит notInstalled после истечения status cache (до 5 минут). Отсутствие heartbeat
само по себе не меняет статус. Автоматическое обнаружение удаления требует следующего
прохода lifecycle/мониторинга; эту проверку не считать завершённой только по UI MVP.

## Границы MVP

Нет автоматического импорта локальных правил/CRM справочников, полного UI истории/восстановления,
ACK применения по вкладкам и расширенного мониторинга admin panel. Публикация означает сохранение
серверной версии, не подтверждение её применения всеми браузерами. Эти задачи остаются в плане.
Локальные проверки описаны в PR; приёмка с настоящим amoCRM после выкладки — отдельный шаг.
