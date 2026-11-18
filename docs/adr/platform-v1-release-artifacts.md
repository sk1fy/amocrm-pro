# ADR: явные артефакты выпуска Platform V1

Принято для P11. Development Compose сохраняет локальную PostgreSQL и сборку.
Выпуск использует отдельный Compose и внешнюю операторскую PostgreSQL: это
сохраняет границу owner/access/jobs и исключает случайные dev credentials.

Release identity — три immutable Docker images, полный source revision и
несекретные configuration/identity IDs. Проверка сначала разрешает все image
references в IDs и затем использует только IDs. OCI label и бинарный
`--version` должны совпасть с source commit; SQL фактического мигратора
сверяется с git. Это обнаруживает смешанные сборки и ошибочную упаковку,
но не является подписью или attestation происхождения.

TLS и курсорный ключ предоставляет оператор. Management listener остаётся
в приватной Docker-сети, публичные host binds по умолчанию loopback.
Обычная исходящая сеть нужна для внешней БД и Core Gateway. Отдельный
оркестратор и secret framework в P11 не вводятся.

Откат использует совместимый полный P09 API на сохранённой схеме. P08
не поддерживает ленту и историю V1. Recovery проверяет сохранение бизнес-фактов
на синтетических данных; реальный amoCRM account P10 проверяет владелец вручную.
