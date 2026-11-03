# ADR-0045: размещение 13 продуктов Rakurs и владельцы их данных

Статус: Proposed — подготовлено к review ПВ-01.07; не перенос реализации.
Дата: 2026-10-22.

## Решение

Для простых продуктов выбираем встроенный модуль Core по
[ADR-0008](0008-multi-widget-capability-boundary.md) и
[ADR-0009](0009-service-modules-and-fair-admission.md), по образцу
[leadstatus](../../internal/services/leadstatus). Отдельный продукт не требует
отдельного процесса или БД. Его таблицы, migrations, settings и jobs остаются
у Core, а прикладной код и регистрация имеют явное имя продукта.

Для тяжёлых продуктов выбираем владельца по
[ADR-0012](0012-separable-product-module-contract.md): собственная логическая
БД/роль/миграции, Repository-порт, одна прикладная реализация для embedded и
gRPC, ограниченные зависимости и ресурсы. Первое размещение может быть
embedded на том же хосте; физический перенос — отдельная операционная
проверка. Отдельного владельца выбираем, когда продукт имеет долговечный домен
со своими операциями/данными и требует независимой изоляции ресурсов, квот,
миграций или релизов. Длительные вычисления, массовые изменения и внешний
credential lifecycle усиливают эту необходимость; одно наличие настроек или
своей папки её не доказывает. Владелец должен иметь ограниченный контракт и
проверяемый перенос, иначе дробление лишь добавляет связи.

Третий путь — **технический адаптер Core + существующий бизнес-владелец TeamOS**:
Распределение по [ADR-0032](0032-lead-distribution-ownership-and-access.md) и Контроль задач
по [ADR-0038](0038-task-control-hybrid-integration.md). Core держит installation,
admission, binding и технические effects; правила и business state остаются
в TeamOS. Его выбираем при уже существующем владельце бизнес-домена; новая
копия правил в Core не создаётся. Эти три пути не требуют 13 новых БД.

Существующие [catalog](../../internal/services/catalog.go) и
[registry](../../internal/services/registry.go) статические. Новый продукт
потребует capability, admission, wiring и тестов границ. Исключение
leadstatus из existing boundary tests не является автоматическим исключением
для всех будущих встроенных модулей: их допустимые зависимости добавляются
явно, без глобального отключения проверок.

## Матрица продуктов

Названия слева — продуктовый план из 13 позиций. Приведённые каталоги SSD
подтверждают текущие границы реализации, но не заменяют сверку коммерческого
кода, integration ID и активных установок перед переносом. Все продукты
используют Core installation/admission и обёртку доступа/оплаты; ниже
перечислены дополнительные зависимости, которые различаются по продуктам.

| № | Продукт и свидетельство SSD | Выбранный путь и обоснование | Владелец данных после переноса | Дополнительные зависимости |
| --- | --- | --- | --- | --- |
| 1 | Аналитика — [AnalyticsV3 routes](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/Modules/Widgets/AnalyticsV3/Routes/api.php) | Отдельный owner `analytics`, сначала embedded: длительные расчёты/экспорты и независимые лимиты | Analytics: настройки отчётов, расчётные данные, checkpoints, операции и очередь | Узкие Gateway чтения; Google export через контракт владельца экспорта. CRM Events только после проверки второго consumer |
| 2 | Автопоиск дубликатов — [DuplicatesV2 routes](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/Modules/Widgets/DuplicatesV2/Routes/api.php) | Отдельный owner `duplicates`, сначала embedded: массовый/фоновый поиск и длительные операции | Duplicates: правила, задания, кандидаты, receipts эффектов и очередь | Gateway чтение/изменение строго выбранных сущностей; Core durable ingress |
| 3 | Google таблицы — [UnloadingTable routes](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/Modules/Widgets/UnloadingTable/Routes/api.php) | Отдельный owner `google-export`, сначала embedded: экспорт, auto-update, курсоры и независимые внешние квоты | Google-export: задания, mappings, checkpoints, outcome экспорта и защищённое хранилище Google credentials | Узкие amoCRM Gateway чтения; на волне ПВ-07 OAuth/refresh и запись в Google передаются владельцу Google-export вместе. До передачи продолжает работать SSD; промежуточный адаптер не считается завершённым переносом. Секреты не копируются в Core settings |
| 4 | Контроль активности — [Activity models](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/Modules/Widgets/Activity/Models) | Существующий owner Activity по ADR-0012. Две внутренние функции: наблюдение и учёт работы; один коммерческий продукт | Activity: настройки/панели/сессии учёта; CRM Events: исходные события и coverage. До переноса учёта его сессии остаются у SSD | CRM Events и разрешённые Gateway методы; Telegram adapter по необходимости. Новый интерфейс 2.0 и расчёт учёта — отдельная реализация, не подразумеваются готовыми |
| 5 | DaData — [контроллер](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/Modules/Widgets/Dadata/Http/Controllers/Widget/rkrs_dadata/MainController.php) | Встроенный Core `dadata`: ограниченный запрос обогащения и настройки, без отдельного долговечного домена | Core: настройки, привязка защищённой external credential reference, receipts изменений | Узкий DaData adapter с отдельными timeout/quota; Gateway только для разрешённого сохранения в CRM |
| 6 | Процессы — [Processes routes](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/Modules/Widgets/Processes/Routes/api.php) | Отдельный owner `processes`, сначала embedded: состояние запусков, шаги и своя очередь | Processes: определения текущих сценариев, версии, run/step state и receipts | Core durable ingress и узкие Gateway commands; опциональные Google/Telegram ports только для подтверждённых сценариев |
| 7 | Оповещения Telegram — [NotificationsTg routes](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/Modules/Widgets/NotificationsTg/Routes/api.php) | Встроенный Core `telegram-notifications`: правила уведомлений и bounded jobs; транспорт выделен портом | Core: правила, destinations references, операции/outcomes; SSD временно владеет bot credentials и транспортными привязками | Узкий Telegram adapter к SSD; Gateway для нужного обогащения. Вывод SSD из работы требует отдельного переноса adapter/секретов |
| 8 | Первый звонок — [SaveFirstCall routes](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/Modules/Widgets/SaveFirstCall/Routes/api.php) | Встроенный Core `first-call`: ограниченная обработка события и запись результата | Core: правила, факт первого звонка/дедупликация, receipts и настройки | Core ingress, Gateway notes/selected update. Не вводить собственный polling всех событий без доказанной необходимости |
| 9 | Копирование сделок — [CopyLead](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/Modules/Widgets/CopyLead) | Встроенный Core `lead-copy`: конкретная автоматизация с текущей durable jobs инфраструктурой | Core: правила/mapping, operation и история создания/unknown outcomes | Core ingress, Gateway чтение и create/update; отдельная процедура сверки неизвестного создания |
| 10 | Цветные сделки — [модель цвета](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/Modules/Widgets/ColorLeads/Models/RkrsColorLead.php) | Встроенный Core `lead-colors`: небольшой продукт и первый пилот | Core: настройки и сохранённые цвета сделок, migration mapping | Browser admission, при необходимости узкая проверка доступа к CRM entity; TeamOS не нужен |
| 11 | Группировка полей — [GroupingFields routes](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/Modules/Widgets/GroupingFields/Routes/api.php) | Встроенный Core `field-groups`: компактная конфигурация и UI | Core: схемы групп/настройки, версия конфигурации | Browser admission, Gateway справочники полей только при необходимости; без отдельного collector |
| 12 | Распределение сделок — [legacy SSD](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/Modules/Widgets/LeadDistribution), [Core catalog](../../internal/services/catalog.go) | Новый продукт: встроенный Core technical adapter + существующий бизнес-владелец TeamOS по ADR-0032; это не автоматический перенос старого Distribution | TeamOS company: правила, люди, графики, очередь решений; Core: binding, admission, технические operations/effects; SSD: коммерческая проекция | Подтверждённый company binding, scoped TeamOS HTTP, Gateway и billing wrapper. Новые installation/OAuth сразу в Core; legacy клиенты SSD не переключаются этим решением |
| 13 | Дата перехода в статус — идентичность кода ещё требует сверки; [StageTime](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/Modules/Widgets/StageTime/Http/Controllers/WidgetStageTimeController.php) именует себя «Время на этапе» | Встроенный Core `status-transition-date` как проектное имя: обработка перехода/отображение даты без отдельного сервиса | Core: подтверждённые правила, даты/история и receipts; точный набор переносимых таблиц после сверки | Core ingress и узкий Gateway update/read. Не присваивать продукту `rkrs_stage_time` автоматически и не объявлять его существующим `lead-status` |

Проектные service codes в таблице не являются зарегистрированным каталогом.
Для №13 решение о размещении принято, но соответствие коммерческого продукта
legacy routes и таблицам — **обязательная проверка до миграции этого
продукта**. Факт наличия `StageTime` доказывает только этот модуль; название
«Дата перехода» само по себе не доказывает его identity. Аналогично пустой
[CopyLead api.php](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/Modules/Widgets/CopyLead/Routes/api.php)
не доказывает отсутствие поведения в legacy web routes/controllers.

## Общие границы, обязательные для трёх путей

1. **Runtime scope и коммерческая история различаются.** Настройки и новые
   операции адресуются проверенной installation + generation и product code;
   владелец исполнения — account + product, только Activity допускает
   отдельный function scope. Owner epoch монотонен. Стабильная commercial
   identity account/product сохраняет оплату и историю переустановок, но не
   разрешает старому job/токену работать в новом поколении. Наследование
   настроек после reinstall — явная copy/migration с audit и новой scope,
   не случайное чтение всех данных аккаунта по `amo_account_id` из браузера.
2. **Admission перед эффектом.** Проверка trusted actor/installation,
   integration capability, generation, owner epoch, доступа к конкретному
   действию/ресурсу и коммерческого допуска выполняется на входе и перед
   исполнением/мутацией. Unknown policy даёт unavailable/deny, не доступ.
   Capability revoke и owner change имеют определённый порядок с mutation
   guard; переключение ждёт durable quiesce старых исполнителей. Кэш владельца
   и наличие записи SSD не заменяют эти проверки.
3. **Настройки имеют владельца и typed contract.** Общий будущий settings
   provider не считается реализованным: нынешний leadstatus и Activity
   не доказывают универсальный settings API для 13 продуктов. В ПВ-02
   проектируются version/revision, validation, actor permissions, ограничение
   payload, миграция схемы и registration для Core-owned продукта. Для
   отдельного owner Core передаёт typed command в порт владельца; не читает
   его SQL и не хранит параллельную редактируемую копию. Секреты не возвращаются
   через browser settings.
4. **Core владеет OAuth и допуском своего трафика.** Модуль не создаёт
   `amocrm.Client` и не получает refresh token. Новая потребность API
   реализуется узким Gateway port с grants, actor/service principal и общим
   бюджетом ПВ-01.05. Наличие общего Go interface не выдаёт весь его доступ.
5. **События и jobs по владельцам.** Встроенные продукты используют Core
   queue с честным admission и bounded concurrency. Отдельный owner имеет
   собственные inbox/queue/leases только если работа долговечна. Не вводятся
   общий automation engine, новый event bus или копия TeamOS. CRM Events
   становится общим источником только после реализации независимого
   consumer lifecycle, coverage и grants; параметр consumer в DTO этого
   ещё не гарантирует.
6. **Оплата — одна обёртка.** Новая установка нового продукта принадлежит
   Core; SSD получает связанную проекцию для текущего расчёта оплаты и
   согласованных endpoints. Проекция не запускает старый runtime и не
   требует копии amoCRM OAuth. Оплаченный пакет не даёт доступ к несвязанной
   installation и не отменяет revoke. Новый Distribution не ждёт переноса
   старого Distribution или остальных 12 продуктов.

## Границы первого внедрения и последующее выделение

Физическое выделение встроенного продукта допускается при измеренной
необходимости независимого масштабирования, релизов, ресурсов или владения,
с отдельным планом переноса таблиц/receipts и выполнением ADR-0012. Наличие
папки продукта не означает, что он уже готов к remote deployment. Обратное
объединение владельцев данных без миграции также не подразумевается.

Пилот Цветных сделок проверяет встроенный путь без нового standalone service.
Следующий пилот Копирования проверяет durable side effects и неизвестный
исход. Activity сохраняет уже созданные границы Activity/CRM Events.
Google/Telegram adapters не превращаются в обязательный предварительный
универсальный сервис для всех продуктов; реализуются нужные consumer
операции. Пока SSD adapter нужен, полное выключение SSD не достигнуто.

`task-control` из [ADR-0038](0038-task-control-hybrid-integration.md) не входит
в эти 13 позиций. Его Core/TeamOS admission и hybrid contracts — полезный
существующий пример; это не разрешение считать его данными или реализацией
Контроля активности. TeamOS обязателен для бизнес-домена Распределения,
для остальных продуктов зависимость добавляется только по их собственной
функции, а не ради унификации интерфейса.

## Критерии review и будущей приёмки

- Ровно 13 продуктовых строк; наблюдение и учёт работы — две функции одной
  строки, не две подписки. Legacy/new Distribution различены.
- Для каждого выбран data owner, scope настроек, bounded операции и
  разрешённые зависимости. Встроенный путь явно разрешён в architecture
  tests, отдельные владельцы проверяются требованиями ADR-0012.
- Будущая приёмка продукта включает подмену account/installation, старое
  generation/epoch, revoke capability, недоступность policy/billing,
  межпродуктовое чтение настроек, повтор команды и неизвестный внешний
  результат. Одинаковая внешняя семантика сохраняется при смене размещения.
- Перед миграцией каждой волны legacy module/route/product/integration IDs
  подтверждаются реестром и выборкой установок; mapping №13 не считается
  установленным по похожему имени. Это не блокирует foundation и первый пилот.
- Реализация, deploy, физические БД и новые settings routes не созданы этим
  ADR. ПВ-01 поставляет согласуемое решение и проверяемые условия следующих
  этапов.
