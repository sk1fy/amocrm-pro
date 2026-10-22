# ADR-0042: обёртка оплаты Core с расчётом в SSD

Статус: Proposed — готово к review ПВ-01, не реализовано. Дата: 22.10.2026.
Задача: ПВ-01.04. Связанные решения: [зеркало установок](0041-core-ssd-installation-bridge.md),
[защищённый HTTP](0033-distribution-signed-http-connection.md).

## Решение и существующий расчёт

Продукты спрашивают Core о коммерческом доступе; Core получает типизированное
решение SSD через billing adapter. SSD остаётся единственным владельцем расчёта
на переходный период. Core не вычисляет заново подписки, партнёрские льготы,
стоимость и trial и не выдаёт бесплатный доступ при недоступности SSD.
Новое Распределение устанавливается в Core, имеет зеркало для оплаты в SSD и не
ждёт переноса старых виджетов. Тарифы и состав новой подписки определяет НП-01;
отсутствие утверждённой политики означает `pending`, а не default free/trial.

Основа серверного решения — [AccountWidgetPaymentResolver](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/app/Support/Widget/AccountWidgetPaymentResolver.php)
с явно загруженными `account` и `widget` и authoritative PaymentContext.
Существующий приоритет `resolvePaidTo`: активный top-list, активная подписка с
продуктом в `apps`, партнёрская льгота, бонус клиента Ракурса, личный `paid_to`.
`isPaid` отдельно разрешает бесплатный продукт. Бонус зависит от `rakurs_status`,
`pay_type` и `type_rakurs`; без загруженного `account` он не вычисляется.

Нельзя просто преобразовать ответ [GetPaidStatusAction](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/app/Actions/Account/GetPaidStatusAction.php)
в access grant: это UI payload с баннерами/ценами, форматированием и округлением
срока до конца дня. `Carbon::parse(null)` не является доказательством оплаты.
[AccountWidget::check](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/app/Models/AccountWidget.php) проверяет
`status` и дату и также не эквивалентен `resolver::isPaid`; [WidgetPaidToValid](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/app/Http/Middleware/WidgetPaidToValid.php)
оборачивает отказ legacy HTTP 200. Новый контракт не переносит эти неоднозначности.

Выбор resolver как источника **не меняет молча доступ всей SSD**. До пилота нужны
shadow-сравнения нового решения с реальными legacy исходами на бесплатных,
платных, null-date, партнёрских, бонусных и пакетных случаях. Различия классифицируются
как ошибка нового адаптера либо явное исправление legacy поведения с согласованной
приёмкой. Нельзя объявлять совместимость только по совпавшей строке `paid_to`.

## Разделение доступа

Коммерческое решение — только один из входов Core. Итоговое исполнение требует
активной установки, действующего OAuth/grant, разрешения пользователя, capability,
текущего владельца/epoch и коммерческого allow. Paid не даёт прав на чужие данные.
OAuth revoked, uninstall, административный запрет и отзыв server grant закрывают
исполнение независимо от billing cache/grace. Unpaid не удаляет OAuth или данные.

Операция в очереди хранит ссылку на коммерческий ключ, но не вечный admission.
Worker повторно проверяет актуальные lifecycle/authorization/owner fences и
billing состояние непосредственно перед внешним эффектом. Между ожиданием
очереди и исполнением старая оплата не считается разрешением. Если полномочия
отозваны во время уже отправленного внешнего запроса, результат сохраняется для
сверки, а повтор требует нового разрешения; это не обещание отменить внешнее действие.

## Узкий набор операций

Пути `/internal/v1/core/...` ниже новые, **ещё не реализованные**. Применяется
signed HTTPS, отдельные credentials, account/product grant и replay-защита
ADR-0041. Core проверяет browser principal сам; токены и произвольные параметры
legacy API не пересылаются.

| Новый endpoint | Scope, назначение | Что переиспользуется в SSD |
| --- | --- | --- |
| `GET /internal/v1/core/billing/{account}/{product}/entitlement` | `billing:read`; получить решение и версии | Resolver + authoritative loader, не legacy UI action |
| `GET /internal/v1/core/billing/{account}/{product}/quote?tariff_id=...&users=...` | `billing:quote`; только разрешённый тариф/количество | [QuoteOurWidgetTariff](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/app/Services/Billing/QuoteOurWidgetTariff.php), текущая логика `/api/private/rakurscrm/quoteOurWidgetTariff/{account}` через типизированный adapter |
| `GET /internal/v1/core/billing/{account}/{product}/payments?cursor=...&limit=...` | `billing:history`; доступ администратора проверен Core; максимум 100 записей | Выборка [ClientBillingController](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/app/Http/Controllers/API/private/ClientBillingController.php), с обязательной фильтрацией по account/product и redaction |
| Core: `POST /internal/v1/ssd/billing/events` | SSD→Core `billing:invalidate`; durable уведомление об изменении | Новый outbox/inbox, ниже |

Для quote требуется серверный mapping `tariff_id` на текущий SSD тариф и
`policy_revision`; индекс массива legacy API не становится стабильным ID Core.
Изменённый тариф возвращает conflict и новый список разрешённых тарифов. Цена не
приходит из браузера и не фиксирует оплату. History не возвращает историю других
продуктов/аккаунтов; пакетная подписка отображается с разрешёнными общими сведениями
без чужих позиций, токенов платёжных страниц и персональных реквизитов.

Существующие [routes](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/routes/api.php)
`/api/account/get/paid-status/{clientId}/{app}` и `getClientBilling` полезны как
источники существующей логики/совместимости, но не вызываются из Core как открытый
универсальный proxy. Account-wide `getClientWidgets` требует отдельной scoped
проекции, если будет нужен UI-каталог; он не входит в первоначальный allowlist.
`/api/widget/install`, `/api/pay/integration/free`, `createClientCash`,
`markClientInvoicePaid`, `transactionAdd/Take`, произвольные обновления срока,
отмена платежей и массовая выдача trial **запрещены credentials этого моста**.

Для первого пилота Цветных сделок обязательны entitlement, синхронизация
зеркала и invalidation. Quote/history реализуются как отдельные потребляемые
операции Billing, без искусственной блокировки пилота, если его UI их не
использует. Платёжный путь нового Распределения должен быть принят отдельно.

Создание счёта/эквайринга — отдельная следующая операция после НП-01: собственный
scoped checkout intent, уникальный business ID и восстановление результата через
SSD. Текущие `/api/account/pay/widget`, `/api/account/pay/widget/invoice` и
`createClientPayment` не разрешаются автоматически только потому, что существует
зеркало. До реализации checkout можно сохранить утверждённый существующий процесс
оплаты SSD; способ открытия платёжной страницы требует отдельного проверяемого
доступа и не использует сырой account ID как авторизацию.

## Формат решения и ошибки

Ответ entitlement содержит `schema_version`, account/product, SSD mapping ID,
Core `installation_id/generation`, `decision=allow|deny|pending`, `reason_code`,
`billing_revision`, `policy_revision`, `evaluated_at`, `fresh_until`,
`decision_expires_at`, `limits` (например, доступные пользователи с определённой
семантикой), `policy_version`, `source=ssd`. Signed read содержит ожидаемые installation_id
и installation_generation текущей установки Core. Несовпадение с зеркалом SSD —
409 mirror_generation_mismatch и pending до reconciliation, без выдачи grant
прошлого поколения. Cache key включает account/product/installation/generation;
при записи и использовании snapshot атомарно сверяется текущий Core lifecycle
и generation, а не только billing/policy revisions. Коммерческий факт account+
product сохраняется через переустановку, но используется через заново связанный
snapshot; старый grant не переносится. Время передаётся RFC3339 UTC; смысл
коммерческой даты/конца дня нормализуется SSD с его действующей timezone политикой.
Core не округляет дату самостоятельно. По окончании срока решение перестаёт быть
allow даже без события invalidation.

`reason_code`: `paid`, `subscription`, `partner`, `rakurs_bonus`, `free_product`,
`unpaid`, `policy_pending`, `mirror_missing`, `source_incomplete`. Для первых пяти
нужна доказанная существующая конфигурация. Если resolver не возвращает причину,
SSD adapter добавляет типизированную диагностику в общей ветке расчёта; нельзя
скопировать правила в независимую вторую реализацию. `decision_expires_at` —
подтверждённая граница действующего права либо null для права без фиксированной
даты. Динамическое `now()->addYear()` бонуса — не обещание клиенту оплатить год:
для него фиксируются reason, revision политики и ограниченная свежесть.

Нулевой/отсутствующий срок платного продукта не преобразуется в текущую дату.
Нет зеркала — `pending/mirror_missing` с запуском reconciliation; неизвестный
продукт/alias — `422 unknown_product`. Нет нужных данных для расчёта по пользователям —
`pending/source_incomplete`, а не ноль пользователей. Для корректного бизнес-отказа
`200` и `decision=deny`; транспортная ошибка — `503`, подпись — `401`, scope —
`403`, revision/mapping conflict — `409`, throttling — `429` с Retry-After.
Ни один HTTP/network error не становится `allow` или новым коммерческим trial.

## Кэш и ограниченное послабление

Следующие значения — **инженерное предложение для review**, не утверждённая
коммерческая политика: positive/negative TTL 5 минут; максимальный grace 24 часа
от последнего успешного authoritative allow. `fresh_until` ограничивается также
сроком права; grace никогда не продлевает известный `decision_expires_at`.
Для платёжных мутаций grace не применяется. В метриках/UI различаются:

| Состояние Core | Поведение |
| --- | --- |
| `fresh_allow` | Выполнять при остальных актуальных разрешениях |
| `fresh_deny` | Отказывать; показывать причину и доступный процесс оплаты |
| `stale_allow`, SSD недоступен, revision не инвалидирована | Разрешать только до min(последний authoritative allow + 24 ч, известная граница права), пытаться обновить |
| `stale_deny` | Продолжать отказ и обновление; сбой не отменяет известный deny |
| `unknown/pending`, предыдущего allow нет | Не выполнять платные действия; синхронизировать зеркало/политику |
| `invalidated`, snapshot ниже известной revision | Старый allow не использовать; `pending` до свежего решения, известный deny остаётся deny |
| `grace_expired` или истёкший срок | Не выполнять; данные сохранять, запросы восстановления ограничивать |

TTL измеряется локально монотонным временем; после рестарта проверяется persisted
время и допустимый clock skew. Повторная выдача того же старого snapshot или ошибка
refresh не сдвигает grace. API и worker используют один сохранённый watermark
invalidation; независимые in-memory кэши не могут снова принять прежний allow.
Финальный TTL/grace фиксируется в конфигурации/приёмке ПВ-03, не зашивается в продукты.

## Порядок изменения подписки и invalidation

[PaymentContext::forSingle](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/app/Support/Widget/PaymentContext.php)
кэширует `subscription:{account}`, партнёров и читает `RKRS_TOP_LIST2`.
[RunCustomJobsCommand](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/app/Console/Commands/RunCustomJobsCommand.php)
заполняет top-list через `Cache::remember(..., null)`. В
[Subscription](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/app/Models/Subscription.php) и зарегистрированных
[observers](https://github.com/Filipp22/rakurs-ssd/blob/a3916751233f9a9389a264ff70909a5c555c0242/app/Providers/AppServiceProvider.php) нет готовой
полной цепочки invalidation Core. Очистить только Core cache недостаточно:
следующий запрос может снова получить старое решение из SSD cache.

Для нового endpoint создаётся authoritative loader всех источников (подписка,
партнёрская льгота, запись top-продукта, account bonus flags, каталог, индивидуальный
срок), который передаёт данные существующему resolver без legacy кэшей. Loader
не заменяет правила resolver; он гарантирует согласованный источник этих правил.

1. Каждая коммерческая мутация фиксирует новое состояние, увеличивает
   `billing_revision` аккаунта и пишет outbox **в одной транзакции**. Изменение
   состава `Subscription.apps` касается объединения старого и нового набора:
   надёжнее инвалидировать все entitlement аккаунта. Изменение каталога/правил
   увеличивает `policy_revision` продукта и публикует product-wide invalidation.
2. Все пути записи обязаны участвовать: оплата/возврат, ручное продление,
   subscription create/update/delete, top-list, partner `free_at`, account
   `rakurs_status`, тариф/free flag, лимиты пользователей. Eloquent observer
   недостаточен для SQL/mass updates; сначала инвентаризировать writers, затем
   общий transactional writer или эквивалентная DB гарантия. Пока путь может
   обойти revision/outbox, критерий готовности не выполнен.
3. Entitlement читает committed source rows и обе revision в согласованном
   snapshot. Нельзя приклеить новую revision к старому cache. Если согласованность
   обеспечить не удалось, возвращается `503`, не предположительный свежий grant.
4. SSD доставляет `event_id`, account или product scope, новые revisions,
   `affected_scope`, `occurred_at`, hash через outbox. Core одной транзакцией
   сохраняет inbox, повышает соответствующие watermarks и инвалидирует только
   snapshot, отстающий хотя бы по одному применимому watermark; уже актуальный
   snapshot той же или большей revision сохраняется.
   ACK идёт после commit; повтор ID с иным hash — `409`.
5. Ответ read принимается только если обе его revision не ниже известных
   watermarks. Проверка и запись cache атомарны с invalidation. Задержанный read
   не может воскресить старый allow. События с меньшей revision — no-op; потеря
   уведомления обнаруживается authoritative refresh и reconcile.

Пример обязательной гонки: подписка разрешала продукт в revision 41; начался read;
администратор удалил продукт, commit 42 + outbox; Core получил invalidation 42;
поздний allow 41 отклоняется. Если read 42 пришёл до invalidation 42, позднее событие
не ухудшает согласованное состояние. Изменение нескольких полей subscription не
публикует промежуточное коммерческое состояние. Product policy и account revisions
сравниваются покомпонентно, а не как одна произвольная строка.

Это bounded eventual consistency между платформами: до доставки уведомления или
TTL Core может ещё знать прежний коммерческий результат. Система не обещает
мгновенный межсерверный отзыв оплаты во время partition. Ограничение срока/grace
явно ограничивает это окно; lifecycle/OAuth revoke Core не ждёт этой очереди.
Часы тоже меняют исход (`paid_to`, окончание льготы), поэтому expiry проверяется
без события, даже если revision не менялась.

## Приёмка и передача в ПВ-03

- Fixtures и shadow-сравнение покрывают resolver precedence, free, null/expired
  paid date, пакет без продукта, удаление продукта из пакета, партнёра и бонус;
  bonus не публикуется как оплаченный год. Расхождения с legacy задокументированы.
- Платёж, ручная правка, изменение/удаление подписки и каталога публикуют durable
  revision; искусственный crash между commit/ACK не теряет invalidation.
- Переставленные события и гонка read 41/invalidate 42 не восстанавливают allow;
  stale SSD cache намеренно заполнен старым значением и не влияет на новый endpoint.
- First install без SSD/политики остаётся pending; stale deny не становится allow;
  grace имеет конечную границу и не возобновляется повторным старым ответом.
- Перед worker effect проверяется актуальный доступ; revoke/uninstall прерывает
  дальнейшее исполнение даже при fresh paid/grace. Уже отправленные запросы учитываются
  отдельно, не запускаются повторно без выяснения результата.
- HMAC replay, cross-account/product, чужая история платежей и произвольная
  платёжная мутация отклоняются; ключи не видны в браузере/логах.

Результат ПВ-03 — adapter, authoritative loader, полный охват writers/outbox,
Core cache/inbox/watermarks, метрики возраста snapshot и доставки, сверка и эти
проверки. Этот ADR не переносит биллинг в Core и не подтверждает готовность checkout,
тарифов НП-01 или production реализации.
