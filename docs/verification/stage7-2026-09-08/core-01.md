# CORE-01: OAuth refresh после аудита

Дата: 10.09.2026. Актуальный контракт: [ADR-0017](../../adr/0017-oauth-refresh-lease.md).
Исходные результаты до исправлений сохранены в корневых логах этапа;
[новые проверки](fixes/README.md) относятся к окончательному коду.

HTTP выполняется вне транзакции. Финализация проверяет token_version и
lease_token. Истёкший lease сохраняет неизвестный исход; другая реплика
не повторяет одноразовый refresh. Поздний успешный ответ прежнего владельца
может завершить persist. Reauthorization повышает версию и очищает claim.

Pending содержит пару токенов, version и lease identity. Повтор после local
persist failure не вызывает HTTP. Timeout/transport/cancel в HTTP и 5xx сохраняют
claim; 429 разрешает повтор отклонённого запроса. 401/validation завершают claim
и обновляют auth status атомарно, с ограждением от новой авторизации.

После crash/потери pending нужен исходный результат или новая авторизация.
Сбрасывать lease ради повторной отправки token нельзя. Это заменяет прежний
ошибочный контракт «перехватить expired lease и снова Refresh».

Регрессии: rotation_regression_integration_test.go, обновлённый тест
TestTokenProviderDoesNotReuseExpiredRefreshLease и существующие проверки
stale 401, concurrent providers, cancellation, key rotation и persist retry.
