# ADR-0017: OAuth refresh без транзакции на время HTTP

Статус: принято с исправлениями аудита 10.09.2026 (CORE-01).

## Контракт

Короткая транзакция захватывает credentials по token_version и записывает
lease_token / lease_until. HTTP выполняется после commit. Финализация проверяет
версию и идентификатор claim, сохраняет пару и освобождает lease. Внешний вызов
не держит SQL row lock или соединение пула.

## Одноразовый refresh

Живой lease заставляет других callers ждать. Истёкший lease не разрешает второй
Refresh: запрос мог уже израсходовать token. Другая реплика возвращает
ErrRefreshOutcomeUnknown, не меняя installation status. Gateway трактует его
как unavailable. Прежний владелец может поздно сохранить успешный ответ, пока
версия и lease identity совпадают.

TTL 45s ограничивает ожидание; безопасность не зависит от завершения HTTP до TTL.
На истёкший claim новый lease не выдаётся. SaveInstallation повышает версию и
очищает claim, ограждая reauthorization от старого ответа. Заменённый lease той
же версии также не допускает финализацию прежнего владельца.

После успешного ответа pending-пара хранится в памяти вместе с version/lease.
Пять коротких попыток persist используют WithoutCancel; следующий вызов того
же provider повторяет persist. Устаревший caller не стирает pending новой версии.

| Исход | Поведение |
| --- | --- |
| HTTP success, включая cancel после ответа | Сохранить по version+lease; не повторять Refresh |
| 401 / validation от refresh endpoint | Атомарно освободить claim и пометить текущую версию reauth_required; disabled/uninstalled не активировать |
| 429 | Освободить claim; позже можно повторить отклонённый запрос |
| Timeout, cancellation в HTTP, transport error, 5xx | Сохранить claim как неизвестный исход; повтор одноразового token запрещён |
| Ошибка decrypt или отмена до вызова Gateway | Освободить claim: внешний вызов ещё не начинался |
| Crash после начала HTTP / потеря pending | Claim блокирует повтор; требуется reauthorization, если прежний владелец не может закончить persist |

MarkReauthRequired от API-401 пропускает незавершённый claim, включая истёкший:
поздний успешный refresh не закрывается ложным auth status. Дефинитивный отказ
самого refresh обрабатывается атомарно с lock order installation → credentials.

Не сбрасывать lease вручную для повторной отправки token с неизвестным исходом.
Новая OAuth-авторизация штатно заменяет credentials и разрешает восстановление.
Таблица pending-токенов не вводится; durable claim хранится в миграции 000012.

## Operator uninstall

NewUninstallTokenProvider использует ту же ротацию и привязан к одному UUID
со статусом uninstalled. Его создаёт integrations CLI для удаления webhook
подписок. Он не меняет status и не открывает обычный TokenProvider.
Другой installation ID или повторная активация установки отклоняются.

## Выпуск

Нужна Core-миграция 000012_oauth_refresh_lease; для полного lifecycle исправления
также нужна 000015. Старые refresh executors остановить/дренировать до переключения:
прежний бинарь способен перехватить истёкший lease. Существующие неизвестные
claims разрешаются успешной финализацией прежнего владельца либо reauthorization.
Down не возвращает уже ротированные токены.
