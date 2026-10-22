# Конфигурация распределения на staging

Шаблоны перенесены из серверного аудита 06.10.2026. Это дополнительные
слои к основным compose-файлам, а не самостоятельный стек. Переменные
описаны в [VARIABLES.md](VARIABLES.md); живые значения хранятся вне Git.

Core использует docker-compose.yml, docker-compose.v0.yml и
core-distribution.local.yaml. Здесь также хранятся публичный и приватный
nginx-шаблоны. Настройка company и его compose-overlay описана в
[TeamOS backend](https://github.com/sk1fy/team-os-backend/blob/main/deploy/distribution-staging/README.md).
Каждый репозиторий документирует переменные своих шаблонов.

Для nginx разрешено подставлять только перечисленные в VARIABLES.md
переменные: без списка envsubst затронет собственные переменные nginx.
Сначала выполните compose config и nginx -t с сертификатами нужной среды.
Сохраните loopback-порты и ACL приватного моста.

Для новой среды создаются отдельные ключи и сертификаты. При обновлении
существующей среды сохраняйте её ключи шифрования и версии key ring:
не генерируйте их заново во время переноса исходников. Для замены ключей
нужна отдельная согласованная ротация. Миграции уже применены в staging;
проверки up/down выполняются только на изолированных тестовых БД.

Полный комплект Core/nginx находится в amocrm-pro/deploy/distribution-staging,
слой TeamOS — в team-os-backend/deploy/distribution-staging.
