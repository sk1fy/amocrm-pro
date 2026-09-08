# Добавление отдельно размещаемого продуктового модуля

Этот шаблон применяется к будущим модулям и расширениям Activity/CRM Events.
Архитектурные правила — [ADR-0012](../adr/0012-separable-product-module-contract.md).
Здесь нет генератора нового сервиса: выполнять только части, нужные конкретной
функции. Если функция уже принадлежит существующему владельцу, расширить его.

## 1. Паспорт изменения до реализации

Заполнить в ADR или описании задачи:

| Поле | Что зафиксировать |
| --- | --- |
| Продукт и операция | Пользовательский результат; почему существующий метод не подходит |
| Владелец | Service code, capability, consumer и ответственный за данные |
| Данные | Собственные таблицы, retention, tenant scope, источник и историческая семантика |
| Зависимости | Точные порты/методы и вызывающие identities; никаких «доступ ко всему Gateway» |
| Контракт | DTO, grants, ошибки, пределы, pagination; синхронное чтение или durable command |
| Размещение | Embedded/standalone, entrypoint, адреса dependency, собственный runtime DSN |
| Бюджет | Pool, worker slots, RPC concurrency, внешние вызовы, retry, объём хранения |
| Совместимость | Wire/durable изменения, миграции, очередность rollout и rollback |
| Приёмка | Тесты, реальные сценарии тестера, требуемые инфраструктурные доказательства |

До добавления таблицы/очереди/endpoint найти существующую операцию в
[serviceapi](../../internal/serviceapi/contracts.go),
[каталоге](../../internal/services/catalog.go) и соответствующем владельце.
Например, подробности уже полученного CRM-события можно раскрыть из текущего
Panel; это не требует нового сервиса, collector или запроса при каждом клике.

## 2. Создать или расширить владельца и его порты

Для нового модуля ориентироваться на такую структуру; `<module>` — заполнитель,
эти каталоги не нужно создавать до появления конкретной продуктовой задачи:

```text
internal/services/<module>/
  service.go                    # Repository + dependency ports, один Service
  <operation>.go                # прикладные операции и правила
  postgres*.go                  # только собственные SQL-адаптеры
  *_test.go                     # прикладные проверки, SQL/import граница
  *_integration_test.go         # собственная БД и восстановление
internal/serviceapi/             # typed transport-neutral контракт
api/proto/services.proto        # additive wire-контракт
internal/servicerpc/             # адаптеры и преобразования
cmd/<module>/main.go             # тонкий standalone entrypoint
migrations/<module>/            # up/down только владельца
```

Сравнить конструкторы
[Activity](../../internal/services/activity/service.go) и
[CRM Events](../../internal/services/crmevents/service.go). Передавать интерфейс
своего Repository и ограниченные зависимости; не создавать из прикладного кода
pool, сетевые клиенты или runtime-конфигурацию. Не импортировать соседний
`internal/services/<other>` даже для повторного использования его Repository.
Общий DTO помещается в `serviceapi`; чужие данные читаются через порт владельца.

Для расширения существующего DTO согласованно изменить
[protobuf](../../api/proto/services.proto),
[conversion generator](../../api/proto/generate-adapters.py), если новый тип
требует поддержки генератора, и
[adapters](../../internal/servicerpc/adapters.go). Не редактировать generated
`pb`/`conversion.go` как независимую реализацию. Добавлять protobuf-поля новыми
номерами; сохранять совместимость старого reader и понятные ошибки старого server.
Новый сервис потребует собственного typed server/client adapter и регистрации.

## 3. Оформить БД и работу владельца

Использовать существующий [migration runner](../../internal/platform/migrations/runner.go)
и отдельный `MIGRATIONS_DIR`. Не создавать второй миграционный framework.
[Development bootstrap](../../deploy/activity/init-db.sh) — образец раздельных
owner/runtime ролей для нового кластера, а не скрипт повторной инициализации
существующих данных.

Runtime получает только собственный DSN, мигратор — owner DSN. Проверить DML
своей БД, отказ DDL и отказ CONNECT к соседним БД. Сохранить проверки привилегий
и ограничения pool из
[componentruntime/database.go](../../internal/componentruntime/database.go).
Общий PostgreSQL-инстанс остаётся допустимым первоначальным размещением;
его CPU/IO и отказоустойчивость общие, поэтому лимит числа connections не
доказывает изоляцию нагрузки.

Если нужна фоновая работа, оформить её в БД владельца: атомарное dedup/создание
operation, bounded claim, lease, fencing, retry и восстановление. CRM Events уже
имеет это в [postgres_worker.go](../../internal/services/crmevents/postgres_worker.go).
Для новых enrichment jobs CRM Events расширять своего владельца. Для read-only
модуля не создавать очередь. Core jobs зависят от Core installations/capabilities
и не являются общей очередью отдельно размещаемых продуктов.

Для durable команды расширить существующий механизм Core admission/delivery в
[activitybridge](../../internal/activitybridge/bridge.go) там, где совпадает
назначение, и определить owner inbox. Не переименовывать прежние job types и
не менять hash/scope старой команды при механическом выделении. HTTP 202 означает
admission; завершение определяется operation владельца. Очистку receipt/inbox
согласовать между владельцами с retry/dedup horizon.

## 4. Расширить policy и транспорт явно

Проверить все связанные точки; новый consumer не появляется автоматически:

| Точка | Действие |
| --- | --- |
| [capabilities.go](../../internal/services/capabilities.go), Core schema/admission | Зарегистрировать необходимую capability и её lifecycle по существующему механизму |
| [corepolicy/policy.go](../../internal/corepolicy/policy.go) | Явно расширить consumer/admission, allowedGrant, Issue/Validate и правила system/user вызовов |
| [serviceapi](../../internal/serviceapi/contracts.go) | Объявить service/action/DTO/port; использовать ограниченный scope и общую модель ошибок |
| [servicerpc/runtime.go](../../internal/servicerpc/runtime.go) | Добавить identity и конкретные разрешённые методы, endpoints/clients, mTLS registration |
| [service-certs](../../cmd/service-certs/main.go) и deployment | Добавить dev identity для тестов; для реального хоста выпустить подходящий сертификат через CA |
| [api contract](../../api/openapi.yaml), [Core routes](../../cmd/api/widget_routes.go) | Если нужна публичная операция, зарегистрировать её через существующие auth/CORS/limit/admission примитивы |

В текущем Activity v0 consumer жёстко ограничен Activity. Расширение CRM Events
на второго потребителя требует проверки schema, source ownership и потребностей
collector: отключение одного потребителя не должно прекращать сбор для другого.
Нельзя снять проверку consumer или дать широкие grants только ради прохождения
нового запроса.

На новый входящий запрос/доставку получать свежий Issue. Между переходами
сохранить короткую delegation и текущие Core DB revocation-проверки из
[ADR-0011](../adr/0011-activity-authorization-budget.md). Новый модуль не получает
доступ к OAuth secret, ключам шифрования или delegation signing key.
Для нового amoCRM метода расширять существующий Gateway/client и общий budget,
указывать allowlist сущностей, batch/response/deadline bounds. Метод, который
принимает произвольный URL, не является допустимым продуктовым портом.

## 5. Подключить оба режима исполнения

Изменить связующий код в одном scope с новым модулем:

1. [Catalog](../../internal/services/catalog.go): code, contract/product version,
   migration owner, dependencies, modes, quotas, routes/RPC.
2. [Registry](../../internal/services/registry.go): typed port validation/accessor,
   dependency registration и readiness. Remote client имеет ноль локальных
   DB connections/workers; standalone сервер указывает свой pool и workers.
3. [Config](../../internal/componentruntime/config.go): адреса, собственный DSN,
   quotas, обязательные параметры и отказ чужих DSN/секретов.
4. [Runtime](../../internal/componentruntime/runtime.go): embedded конструктор и
   standalone конструктор одной реализации, registration, startup/shutdown,
   health/metrics; Core использует одинаковый порт в обоих случаях.
5. [Dockerfile](../../Dockerfile) и Compose: entrypoint/image, owner migrations,
   сеть/identity mounts/health, отдельные ограничения ресурсов.

Registry остаётся статическим; не делать dynamic loader или generic
«вызвать метод по строке». Добавление нового продукта требует wiring. Перенос
уже зарегистрированного продукта не требует новой версии его бизнес-логики.

В текущей конфигурации `ACTIVITY_MODE` переключает граф Activity + CRM Events
целиком. В `grpc` адреса Activity, CRM Events и Gateway независимы, поэтому
процессы могут находиться на разных хостах. Не обещать уже существующий mixed
embedded/remote режим для каждого компонента: его config/composition добавляется
отдельно только при необходимости.

## 6. Доказать границы, совместимость и отказоустойчивость

Расширять соответствующие существующие проверки, а не создавать вторую систему
тестов. Использовать Docker workflow проекта из [Makefile](../../Makefile).

| Проверяемое свойство | Существующая точка расширения |
| --- | --- |
| Новый каталог не импортирует чужого владельца/Core/transport; standalone не получает чужую конфигурацию | [componentruntime/boundaries_test.go](../../internal/componentruntime/boundaries_test.go) |
| Прикладной слой зависит от своего Repository, SQL остаётся в адаптере | [crmevents/boundaries_test.go](../../internal/services/crmevents/boundaries_test.go), аналог для нового модуля |
| Registry отвергает неизвестную/двойную регистрацию и неправильное владение ресурсами | [services/registry_test.go](../../internal/services/registry_test.go) |
| Равенство прикладных результатов local/mTLS gRPC | [activity_parity_test.go](../../internal/servicerpc/activity_parity_test.go), [crmevents_parity_integration_test.go](../../internal/servicerpc/crmevents_parity_integration_test.go) |
| Недопустимый caller/method не доходит до владельца | [least_privilege_test.go](../../internal/servicerpc/least_privilege_test.go), [grants_test.go](../../internal/serviceapi/grants_test.go) |
| Ограничения payload, ошибки, Retry-After, cancellation | [response_limits_test.go](../../internal/servicerpc/response_limits_test.go), [runtime_test.go](../../internal/servicerpc/runtime_test.go) |
| Сохранение БД, command/operation IDs, работы Core при переключении процессов | [process_integration_test.go](../../internal/componentruntime/process_integration_test.go) |
| Restart, lease loss, недоступность получателя/зависимости | [process_faults_integration_test.go](../../internal/componentruntime/process_faults_integration_test.go) |

Зафиксировать данные до и после upgrade: контракт/образ/миграции, command IDs,
operations, незавершённая работа. Проверить повтор прежнего durable payload и
порядок старый client → новый server / новый client → старый server в пределах
заявленной совместимости. При невозможности mixed rollout остановить прежние
executors; не создавать для старой команды новую identity. Не объявлять проход
тестов доказательством live CRM, нагрузки production или multi-host приёмки.

## 7. Принять отдельное размещение

Для локального переключения использовать готовый
[Activity runbook](activity-v0.md). При переносе на другой физический сервер
отдельно проверить:

- DNS/маршрутизацию/firewall и соответствие удалённого имени DNS/IP SAN;
  SPIFFE URI должен соответствовать зарегистрированной identity. Hostname адреса
  используется как TLS ServerName, а локальный dev-сертификат не покрывает
  произвольный удалённый адрес.
- Доступ только к БД владельца, PostgreSQL TLS и выбранную сохранность данных.
  Сначала можно перенести только вычислительный процесс с прежней БД; перенос
  БД имеет отдельные backup/restore и согласованность с Core outbox.
- Совпадение результатов через прежний публичный Core origin. Постоянный
  тестовый виджет — `/Users/nikpeskov/Projects/sub-projects/amocrm-pro-service-2`;
  дорабатывать его вместе с backend, без нового тестера и без адресов продуктовых
  RPC в browser-коде.
- Панель/детали, повтор прежней команды, новая operation, недоступный модуль,
  восстановление и существующий lead-status. Сохранить sanitized evidence без
  токенов, приватных сертификатов, персональных данных и записей звонков.
- Суммарные pool/worker/RPC/outbound бюджеты и graceful остановку прежнего
  экземпляра. Вынос одного процесса не доказывает готовность нескольких replicas;
  для Gateway текущий бюджет имеет одного активного владельца.

В отчёте различать четыре результата: реализовано в коде; проверено локально;
развёрнуто на нужных хостах; принято через живой виджет. Указывать непроверенные
условия и порядок возврата. Готовность нового модуля не требует предварительного
переезда остальных продуктов или автоматического выделения lead-status.
