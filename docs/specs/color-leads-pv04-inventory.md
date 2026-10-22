# ПВ-04.01 — инвентаризация «Цветных сделок»

Дата: 2026-10-22. Статус: проверка исходников; production baseline, данные и нагрузка не подтверждены. Production-запросы и визуальные проверки не выполнялись.

## Источники и граница достоверности

- SSD HEAD `a3916751233f9a9389a264ff70909a5c555c0242`: `Modules/Widgets/ColorLeads/{Routes/api.php,Providers/RouteServiceProvider.php,Http/Controllers/WidgetColorLeadsController.php,Models/RkrsColorLead.php}`, `app/Actions/Widget/SettingsGroupAction.php`, `app/Models/WidgetSetting.php`, `app/Http/Middleware/WidgetPaidToValid.php`.
- widgets-dev HEAD `d000ac7335942c18458d4baf61cc7cbed4a72f8d`: `rkrs_colorfull_lead/widget/script.js`, `manifest.json`, `lib/app/{rakurs,account}.js`, `vue/src/components/{SettingsWidget,RuleForm,MultiSelect}.vue`, `vue/src/mainProd.js`.
- Manifest содержит version `1.0.4`, `init_once:false`, locations `everywhere,card_sdk,settings,llist,advanced_settings,widget_page`.
- Это локальные исходники, а не доказательство состава опубликованного ZIP. В SSD есть чужие незакоммиченные изменения bridge/routing; они не проверены как развёрнутый барьер. Не изменялись.
- `templates/amo-widget-card-base.twig` вызывается из script.js, но отсутствует в изученном дереве. Сверка опубликованного ZIP, hash/manifest и source map обязательна до пилота.

## HTTP и данные SSD

Все пять маршрутов — POST `/api/v1/rkrs_color_leads/…`; account определяется из `amo_account_id`, app из `app_name` с default `rkrs_color_leads`. Контроллер сам не выводит account из проверенного Core-контекста. Подключён `WidgetPaidToValid`; наличие оплаты не является доказательством доверенной идентичности или владения исполнением. Новый API должен получать tenant/product/installation из проверенного контекста ПВ-03.

| Метод | Запрос и фактическая семантика | Ответ |
|---|---|---|
| getSettings | account/app; `WidgetSetting` по `(amo_id,widget)`; при пустом settings fallback к `Widgets.widget_init_setting` | decoded JSON либо `[]` |
| setSettings | `settings` приводится к array, весь JSON заменяется через updateOrCreate | сохранённый settings |
| getColorLeads | `leads_ids` необязателен; пустой список читает **все** строки аккаунта | коллекция полных моделей |
| createColorLeads | `data[]`; upsert key `(amo_account_id,lead_id,stage_id,pipeline_id)` | число обработанных элементов |
| updateColorLeads | `data[]`; upsert key `(amo_account_id,lead_id)`; stage/pipeline перезаписываются | число обработанных элементов |

Общий успешный envelope `{data,message,result:true}`. Исключения возвращают HTTP 200 с `result:false`; subscription middleware тоже может вернуть HTTP 200 с иной формой ошибки (`status:0`, `success:false`). Проверять только HTTP status недостаточно. Явной транзакции массива, schema validation, optimistic revision и идемпотентности команд в контроллере нет; частичное выполнение возможно.

`RkrsColorLead` использует таблицу `rkrs_color_leads`, `guarded=[]`. Подтверждены поля, используемые кодом: `amo_account_id,lead_id,stage_id,pipeline_id,borderColor,backgroundColor,timestamp,updated_at`; необязательные цвета/timestamp записываются как null. Eloquent timestamps остаются включёнными по умолчанию. SQL migration таблицы в просмотренном `database` не найдена: физические типы, indexes, uniqueness, nullable, число строк и дубликаты требуют schema export. Нельзя молча объединять записи по lead_id: create/update используют разные ключи.

Это **не подтверждённая append-only история**: update заменяет состояние, create тоже upsert. Отдельного history API/модели в модуле не обнаружено. Сохранять сырые legacy-строки и происхождение при миграции; полную историю событий из них восстановить нельзя. Изученный browser script вообще не вызывает get/create/updateColorLeads; его окраска вычисляется из правил и CRM GET. Это не доказывает отсутствие других потребителей таблицы.

## Правила и совместимость поведения

Настройки формы: `{main_settings: Rule[]}`. Rule содержит `id` (форма создаёт Date.now()), `name`, `enabled`, `bgColor`, `borderColor`, `textColor`, `conditions:[{fieldId,operator,value}]`, опционально `allowedUsers`.

- Порядок массива определяет приоритет: первое enabled правило, все условия которого истинны, побеждает. Пустой conditions даёт true (`every`). Отсутствующий/пустой allowedUsers разрешает всех; непустой использует strict `includes(userId)`.
- Синтетические fieldId: `504473000` — название, `5044780000` — responsible_user_id. Остальные — custom_fields_values. Берётся только **первое** значение поля, даже multiselect. Отсутствующее поле => false.
- Операторы: equals, notEquals, contains (без учёта регистра), greater, less, greaterDays, lessDays. Unknown => false. Numeric greater/less приводятся к Number; обычное equality для большинства полей сравнивает String, user — Number; checkbox сравнивает `value === "1"` (boolean true не равен строке "1"). Это особенности совместимости, не рекомендуемый новый дизайн.
- Date использует начало дня в **часовом поясе браузера**. greaterDays N>0: now в интервале `[fieldMidnight-N*86400, fieldMidnight]`; N=0 — равенство календарного дня. lessDays: строго позже `fieldMidnight+N*86400`. Форма сохраняет date через `new Date(YYYY-MM-DD)`/Unix seconds; перенос в account timezone без согласования меняет результат.
- Канбан: фон карточки, border `3px solid`, цвет и fontWeight600 `.pipeline_leads__field`. Список: фон `.list-row` и `.list-row__cell-status`, без текста/обводки. Старый код не сбрасывает стиль, если правило перестало совпадать: новые изменения поведения следует явно тестировать/документировать.
- UI разрешает типы text/select/multiselect/radiobutton/date/numeric/checkbox/textarea/url/streetaddress/user. `date_time` есть в названиях, но исключён фильтром доступных полей.
- Форма умеет добавлять/редактировать/удалять/включать правила; локальное редактирование сохраняется отдельным Save в SSD целым массивом. `loadFromLocalStorage(rkrs_colorful_rules)` определён, но onMounted вызывает remote load, а не этот fallback.

## Выполнение браузера и все выявленные классы эффектов

| Путь | Эффект / ресурс | Что нужно остановить |
|---|---|---|
| init / colorExtractLeads | SSD getSettings, CRM GET `/api/v4/leads?filter[id][]=…` последовательными batch50, затем DOM CSS | abort запросов; generation/owner проверка после await и перед каждым DOM эффектом |
| Kanban/list observers | MutationObserver `.hs-scroll-map`/body class, debounce1500ms; document ready; anonymous beforeunload | хранить handles, disconnect обоих observers, cancel debounce/ready, снять namespaced listeners |
| repeated init | `init_once:false`, ресурсы могут создаваться заново | registry единственного экземпляра на context; dispose прежнего до нового |
| advancedSettings | заменяет work-area HTML; `rkrs.renderHeader`, `app.settings` монтируют интерфейсы | иметь явные unmount handles, удалить лишь собственный DOM/CSS |
| SettingsWidget mount/save | CRM custom_fields?limit=250, users (два запроса из разных loaders), SSD settings read/write; timer700ms | отмена/игнорирование late responses, запрет Save при stale/freeze, fence на сервере перед commit |
| MultiSelect | anonymous document.body click listener, Vue watchers/DOM | remove listener + unmount; mainProd сейчас не возвращает app или unmount |
| settings/install | async Twig вставки, глобальный window.rakurs; ActivationRakurs: document/element listeners, dropdown.onclick, IMask | cancellable template callbacks, listener/mask cleanup, ownership global reference |
| ActivationRakurs | GET roles/phoneList; POST `/api/widget/install`; таймер200ms вызывает native widget Save, timer1000ms install callback | блокировать всю цепочку установки, cancel timers, reconcile уже отправленный install; не повторять коммерческие эффекты при техническом mirror |
| install callback/account helper | ещё timer1000ms -> window.open; GET widgets/users -> POST `/api/widget/account/update` -> localStorage | запрет late callback/window.open/POST, abort и durable reconciliation, account-scoped storage |
| общий header bundle | содержит пути paid-status, contactManager, pay/widget, pay/widget/invoice, rkrs_client_time/commonPay; ссылки OAuth/status | отдельно проверить достижимость событий и оформить payment wrapper; найденный URL в bundle сам по себе не доказывает вызов |
| tools.js | определены POST salesbot/run и PATCH leads | в script.js нет вызовов tools; **не считать подтверждёнными записями этого виджета**, но исключить обход при новом включении helper |

`callbacks.destroy` и `onRouteState` отсутствуют. Observer disconnect происходит только перед уходом документа, fetch/jQuery requests не оснащены abort/owner fence. Удаление DOM или прекращение вызовов loader не останавливает замкнутые callbacks. Отказ SSD API не останавливает прямые CRM GET/DOM изменения. Все эти ресурсы являются границей безопасного адаптера; полная замена jQuery/MutationObserver сама по себе не нужна.

## Cutover и rollback — обязательный допуск

1. Подтвердить опубликованный baseline и все пути исполнения; подготовить управляемый legacy adapter либо доказуемый протокол отключения **всех** старых вкладок/клиентов. Отдельная интеграция изолирует ZIP, но не запрещает одновременный старый widget.
2. Перевести account/product в freezing, закрыть приём старых effects на сервере, зафиксировать epoch/generation. Остановить клиентские listeners/observers/timers, отключить формы и независимые запросы; обработать уже отправленные write commands до snapshot. Не признавать abort доказательством отсутствия server commit.
3. Сохранить settings JSON целиком, legacy color rows без collapse, контрольные суммы/количества и settings revision. Доказать отсутствие старого исполнителя; вкладка без cleanup/fence не даёт такого ACK и не допускает активацию Core. Обычный баннер «обновите» недостаточен.
4. Активировать нового владельца только после drain/сверки и подтверждённого admission; reloadRequired не снимается SPA-навигацией. Проверять lease/epoch/generation перед effect, особенно после сна вкладки/await. Offline/localStorage не даёт права запуска legacy.
5. Rollback симметричен: freeze Core, drain/reconcile, экспорт актуальных настроек/данных, проверка совместимости и import в SSD, новый epoch, затем разрешение SSD. Возврат старого snapshot с потерей новых записей запрещён. При недоказанном drain/ошибке cleanup оставаться blocked/read-only.

Это требования к реализации и приёмке, **не заявление об уже обеспеченной безопасности** или выполненном переключении.

## Метрики и оставшиеся доказательства

Подтверждена только серверная instrumentation: файл `storage/logs/rkrs_color_leads.log`, request/success/failure events, count lead_ids/items/returned, settings_keys и exception metadata. Нет прочитанных production значений throughput/error rate/latency. Нельзя на основании кода обещать скорость, квоты или процент ошибок.

Для допуска нужны: опубликованный ZIP/hash; SQL schema и read-only account export; число settings/rules/color rows/дубликатов и объём; известные потребители color CRUD; error rate/latency p50/p95/p99 и фактические CRM запросы; account-scoped snapshot checksums; golden fixtures спорных типов/date/timezone; доказательства остановки старых клиентов и inflight write reconciliation; успешный пробный rollback. Визуальную проверку пользователь отложил: она остаётся отдельной невыполненной приёмкой.
