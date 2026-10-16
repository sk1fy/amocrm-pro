# Контроль задач: отключение и повторная авторизация

R06, 2026-10-16. Этот документ описывает новую реализацию; факт выкладки и живой
приёмки записывается отдельно. Правила TeamOS не хранятся в Core.

## Источники состояния

amoCRM поддерживает GET-хук отключения с client_uuid/account_id/signature:
[официальный формат](https://www.amocrm.ru/developers/content/oauth/step-by-step#Хук-об-отключении-интеграции).
HMAC-SHA256 подписывает `client_uuid|account_id` ключом OAuth client secret.
В документированном формате нет времени события/номера установки, поэтому подпись
сама по себе не позволяет удалить текущую generation после переустановки.

Новый точный публичный маршрут:
`GET /api/v1/widget/task-control/disconnect`. Только известный client UUID,
точный account и наличие capability task-control допускаются к проверке подписи.
Ключи не выдаются и не записываются в лог. Duplicate query keys отклоняются.
После durable записи сигнала ответ 202. Это приём сигнала, не подтверждение удаления.

Worker в существующем процессе проверяет `/api/v4/account` текущими OAuth credentials
через существующий limiter. Ответ 200 с правильным account отвергает старый сигнал;
401 в ответ на текущий access token + подписанный сигнал подтверждает отключение.
403, timeout, 429, некорректный account и generic OAuth refresh error оставляют unknown.
Probe не повторяет запрос с refresh после 401 и не изменяет OAuth credentials по этому
ответу. Получение свежего token перед запросом использует штатный token provider.
Проверка может остаться unknown, если credential уже reauth_required либо refresh
возвращает неопределённый отказ: нельзя выдать это за подтверждение uninstall.

Работа переживает рестарт: signal_pending/next_check_at хранятся в PostgreSQL.
Один worker берёт одну запись за tick 1 s, timeout запроса 15 s, retry 60 s.
При очереди срок растёт; нет безусловного обещания глобального времени доставки.
CAS generation+revision исключает поздний ответ после новой авторизации/сигнала.
Timeout и отсутствие heartbeat никогда не означают удаление.

| state | reasonCode | Значение |
|---|---|---|
| installed | registry_active | Реестр активен, неподтверждённого сигнала нет |
| unknown | disconnect_unverified | Сигнал принят, текущее отключение не доказано |
| notInstalled | disconnect_confirmed | Подписанный сигнал + текущий 401 |
| notInstalled | operator_uninstall | Оператор подтвердил фактическое удаление |
| unknown | product_disabled | Capability/интеграция отключена в Core |
| unknown | registry_reauth_required | OAuth требует восстановления, это не доказательство удаления |
| unknown | installation_unconfirmed | Полного свидетельства установки нет |

`revision` меняется со свидетельствами. `generation` увеличивается при подтверждённом
отключении, новой OAuth авторизации (включая active→active), отзыве service grant,
отключении product/integration. Refresh access token не означает новую установку.
Повтор подтверждённого disconnect не увеличивает generation. Новый OAuth callback
снимает прежний pending/confirmed signal, но не включает отключённый grant автоматически.
Изменения ограничены строками task_control_installation_versions; другие products
и их grants не меняются. Auth handoff запрещён для pending/confirmed отключения.

TeamOS сохраняет reasonCode и проверяет scope/generation. Его status cache ≤5 min;
после свежего notInstalled/generation mismatch старая session/proof не принимается.
Ранее выданная session может работать до истечения этого кеша; мгновенного offline revoke
нет. Повторная привязка явная: новый binding не наследует опубликованные правила;
старые draft/publications остаются для чтения согласно R02. Новую связь нельзя
считать настроенной до явной публикации и переключения режима.

## Выкладка на тестовый стенд

1. Сохранить commit/image IDs и backup БД. Применить Core migration 000025 штатным
   migrate; сначала БД, затем обновить OAuth API и worker (оба используют новые поля).
2. Сохранить существующие overlays и env. Добавить только точный disconnect location
   из `deploy/task-control-staging/public.locations.conf`. Не открывать порт 8087 целиком.
   У этого location отключён access_log: signature повторно используема и не должна
   попадать в proxy/CDN/error/request logs. Проверить вышестоящие proxy.
3. `nginx -t`, reload. Запрос без подписи даёт 400, поддельная подпись — 403; не 404.
4. В настройках именно тестовой amoCRM-интеграции указать URL отключения
   `https://2-26-66-187.sslip.io/api/v1/widget/task-control/disconnect`.
   Redirect URI установки оставить прежним. Никаких новых scopes/секретов.
5. Обновить company/gateway TeamOS с аддитивным reasonCode, сохранив R02.

Не считать установку URL или локальные тесты живой приёмкой. До теста записать
installation/binding IDs, generation/revision, режим и текущую publication.
Удалить/переустановить только разрешённый пользователем тестовый виджет, сохранить
время сигнала/проверки/первого свежего API-ответа. Проверить старую runtime session,
старый proof, повтор webhook и explicit relink. Восстановить исходный тестовый режим
с явной публикацией; не терять старые правила и не трогать другие виджеты.

## Recovery и ограничения

Если callback потерян, подпись не проходит после secret rotation, credential уже
недоступен или probe остаётся unknown, автоматическое подтверждение не обещается.
Проверить фактическое состояние в amoCRM и использовать прежний scoped CLI:

```sh
docker compose run --rm integrations uninstall \
  --actor YOUR_OPERATOR --code TASK_CONTROL_CODE --installation-id VERIFIED_INSTALLATION_UUID
```

Команда допустима только после подтверждения реального удаления. Для восстановления —
повторная OAuth-авторизация, active product/grant и явная новая привязка TeamOS.
Если hook не настроен, реестр сам по heartbeat не узнает об удалении. Постоянный
фоновый обход всех CRM аккаунтов в этой реализации не добавляется.
Rollback бинарников возможен с сохранением аддитивной migration25; не выполнять down
на живой БД, не обнулять generation и не восстанавливать старые session hashes.
