# CORE-02: installation lifecycle после аудита

Дата: 10.09.2026. Контракт: [ADR-0018](../../adr/0018-installation-lifecycle.md).
[Повторные проверки](fixes/README.md).

Uninstall фиксирует uninstalled и закрывает продуктовый доступ. Отдельная
operator capability обновляет access token общим OAuth provider, но только
для одного UUID со статусом uninstalled; другие установки и обычный token load
остаются недоступны. Старый StoredAccessTokenProvider с no-op refresh удалён.

Reconcile/uninstall используют реестр installation_webhook_destinations:
намерение регистрации сохраняется до HTTP; URL зашифрованы с AAD установки/hash.
При смене hostname/key удаляются только известные прежние destinations.
Чужие URL сохраняются, в том числе на том же hostname. Для миграции старой
установки допустим только exact canonical path с её текущим секретным ключом.
Неизвестные старые ключи не дают основания автоматически удалить чужой URL.

Миграция 000015_webhook_ownership нужна до нового бинаря. Статусы lifecycle,
история CRM, outbox и grants не изменены. Purge и remote OAuth revoke остаются
отдельным scope. Повторный uninstall идемпотентен; при неизвестном исходе
одноразового refresh CLI сообщает о необходимости восстановить pending
или пройти reauthorization, сохраняя статус доступа.

Регрессии: ownership_regression_integration_test.go — чужой webhook, сосед
на том же hostname, смена ключа, потеря ответа регистрации, expired access,
изоляция operator credential capability и закрытый продуктовый token load.
