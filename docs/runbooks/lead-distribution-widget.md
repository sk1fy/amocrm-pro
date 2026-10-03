# Виджет распределения: локальная реализация РС-08

Виджет `rkrs_lead_distribution` использует общий модуль TeamOS. Браузер обращается
к Core через штатный amoCRM SDK `$authorizedAjax` (заголовок `X-Auth-Token`),
не хранит JWT/OAuth/межсервисные ключи и не назначает ответственного самостоятельно.
Контракт: [`api/distribution-widget-openapi.yaml`](../../api/distribution-widget-openapi.yaml).

## Подключение и права

`GET /api/v1/widget/distribution/bootstrap` возвращает `state`, `accountId`,
`userId`, `canManage`, `teamOSUrl` и активную `binding`, если она существует.
При отсутствии связи — минимальное `not_connected`, без company/employee данных.
Связь создаётся существующим подтверждаемым сценарием TeamOS, а не произвольным
account_id из браузера. Недоступность OAuth/справочников/сервисных grants
означает отказ доступа; bootstrap не утверждает, что соединение исправно.

Опциональная `DISTRIBUTION_TEAMOS_PUBLIC_URL` — HTTPS origin пользовательского
TeamOS; сервер возвращает ссылку `/distribution`. Это отдельный адрес от
private `DISTRIBUTION_TEAMOS_URL`; без настройки ссылка `null`, интерфейс
показывает необходимость конфигурации. Browser URL не выбирает company/scope.

Для чтения требуется проверенная связь, acknowledged employee mapping, активная
компания/сотрудник и текущий доступ к разделу. Настройки изменяет одновременно
активный CRM administrator и текущий TeamOS owner/admin. Карточка/история/действия
дополнительно проверяют обычный live `CanViewLead` до и после private вызова.
Core повторно проверяет текущую связь и employee mapping перед выдачей результата.

## Runtime

`POST /api/v1/widget/distribution/runtime` принимает только
`{kind,id?,leadId?,limit?,offset?,write?,requestId?,payload?}`. Scope/user/company
формирует Core из проверенного principal. Core подписывает private
`POST /internal/v1/distribution/widget-runtime`; для этого endpoint необходимо
явное capability **widget-runtime** в TeamOS (не заменяется widget-access).
HMAC, durable nonce, exact binding revision и обычные сервисные ограничения
сохраняются. Межсервисные ключи в ZIP/frontend не размещаются.

Чтение: `groups`, `settings`, `rules`, `references`, `availability`, `lead`,
`history`. References добавляет только verified active employees `{id,name}`
точной связи. `lead` возвращает разрешённые строки очереди и использует общий
детальный DTO этапа 07: live owner/name/URL, доступные действия и безопасные
nullable поля. История не раскрывает приватные command/audit payload.

Запись (`write:true`) допускает `rules` (новое правило для существующей группы),
`rule` (revision-protected edit), `group` (общее изменение конфигурации) и `action`.
Создание группы и часовой пояс остаются в TeamOS. Rules/create не принимает
browser bindingId/revision; Core/Team заполняют их сами. Rule/update требует
`expectedRevision`, `active`, `keepCurrentResponsible` и опциональные
`pipelineId/statusId`. Group/configuration:
`expectedRevision,name,memberIds,disabledMemberIds,active,algorithm:round_robin`.
Действия: `action,requestId,expectedUpdatedAt`; requestId должен совпадать с
конвертом. Группы другого подключения недоступны для изменения.

## Неопределённый исход

Миграция TeamOS 27 хранит receipt по `(company,requestId)` и хэш семантического
запроса, включая scope, actor и payload; expiry нового JWT не меняет этот хэш.
Pending фиксируется **до** бизнес-операции, ответ — после. Это не единая
транзакция ledger+effect и не обещание exactly-once во внешней CRM.

Подтверждённый повтор возвращает сохранённый результат, другой payload/actor
для того же ключа — 409. Если процесс/сеть потеряны после admission,
повтор возвращает **202 `{state:outcome_unknown,retryAllowed:false}`** и никогда
не запускает ту же мутацию второй раз. Post-dispatch потеря прав в Core также
возвращает 503 outcome_unknown: отказ не доказывает отсутствие эффекта.

Виджет сохраняет draft и requestId, явно проверяет тот же receipt, перечитывает
общую конфигурацию и сравнивает с ожидаемой. Новый запрос допускается после
осмысленной сверки пользователем с новой текущей revision; автоматическая
подмена ключа запрещена. Параметры, отвергнутые до admission, эффекта не создают.
Контекст backend mutation ограничен JWT expiresAt и максимумом 15 минут; авторизация повторно
проверяется после ожидания authoritative availability lock. Отдельные базы
не дают атомарного cross-service изменения прав, обычные bounded checks остаются.

## Проверка и выпуск

Локальные Core PostgreSQL/race тесты проверяют SDK header/CORS/JTI, server scope,
обычные CRM права, signed runtime и post-dispatch uncertainty. TeamOS PostgreSQL
проверяет общую revision, идемпотентность с новым JWT, конфликт payload/revision,
текущую роль/связь, expiry на блокировке и durable pending без повторной мутации.

Настоящая установка ZIP, OAuth, смена CRM аккаунта и E2E с реальной тестовой
сделкой **не проверены локально**. Это отложенная РС-08.3.2; закрытие браузера
не управляет Core/TeamOS worker. После подготовки тестового сервера/аккаунта
нужны разрешённый HTTPS asset/API host, интеграция, подтверждённая связь,
explicit service grants и тестовые пользователи/воронка/сделки.

Для реальной установки trusted TLS ingress публикует только widget bootstrap,
runtime/permissions и OPTIONS с account-bound CORS. Private `/internal/v1/`
маршруты listener остаются недоступными извне; overlay не добавляет host ports.

Неоднозначная active binding не выбирается «первой»: bootstrap возвращает
минимальный `ambiguous`, без company/employee данных и прав изменения.

Парный локальный профиль `make distribution-team-bridge-test` проверяет 10
сценариев с настоящими Core/TeamOS HTTP-handler и отдельными PostgreSQL: новый
сценарий bootstrap → shared rules → запись → receipt с новым JWT → одна revision,
поддельный companyId и текущая роль; плюс 9 сценариев назначения РС-06. JWT
подписан синтетическим fixture secret, amoCRM заменён контролируемой фикстурой.
Это проверка production middleware/контракта, **не установка виджета в amoCRM**.
