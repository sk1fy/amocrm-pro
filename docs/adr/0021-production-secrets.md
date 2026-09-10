# ADR-0021: хранение секретов текущего контура

- **Status:** Accepted for the current single-host topology; production KMS
  boundary deferred.
- **Date:** 2026-09-10.

## Контекст

CORE-06 требует выбрать production-хранение секретов и границу KMS. Runtime уже
шифрует client secrets, OAuth tokens и webhook keys AES-256-GCM keyring
(`ENCRYPTION_KEYS` + `ACTIVE_ENCRYPTION_KEY_VERSION`). Development Compose
держит демонстрационный ключ в env. Целевой удалённый сервер в этой сессии
недоступен (BASE-01). Appendix G: KMS не условие первого подробного экрана.

## Решение

Для текущего single-host контура оставляем существующий keyring в защищённой
runtime-конфигурации процесса (env или файл, одинаковый у API, worker и
operator CLI). Публичный development key вне `APP_ENV=development` запрещён.
Клиент KMS, Vault и cloud CMK **не внедряются**.

Production-хранилище и KMS **не выбраны**: нет доступа к целевому серверу и его
secret manager. Не считать development env окончательной схемой. Когда появится
хост, решение должно сохранить decryptability: старые версии ключа остаются в
кольце, пока ciphertext не переписан. Процедуры — в
[secrets-rotation.md](../runbooks/secrets-rotation.md).

## Последствия

- нет ложной зависимости от AWS/GCP KMS в первом пилоте;
- оператор отвечает за одинаковое кольцо на всех процессах и за overlap при
  ротации;
- смена схемы хранения — отдельное ADR, когда известна инфраструктура.
