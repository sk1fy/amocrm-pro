# Синхронизация исходников staging — 06.10.2026

## Происхождение

Серверный пакет: sync-audit-20261006T145038Z.tar.gz.
SHA256: 55ee057187bf5fc82bee20fc9b19fcc9cc1307516eb858342a0379aae2141af0.
Проверены все 48 файлов SHA256SUMS. Fetch подтвердил серверные базы.

| Проект | База сервера и актуальный origin/main |
| --- | --- |
| amocrm-pro | d7e401c6607b3a1d1789f9db95c903b108511ee3 |
| amocrm-pro-admin | 3dfa3e38844b6f8651c8d04e5640cf7633c7cd40 |
| team-os-backend | d61a040f4985ed72fa9798c647e601da776b6590 |
| team-os | 09a987d9345289561d01c950844324a17eee6908 |
| rakurs-widgets-new | 8a76caad7b0c808ead76af4dd08b521b1fe66e6c |

Все проекты получили отдельную ветку codex/staging-sync-2026-10-06.
Прежние ветки сохранены. Tracked-изменения сохранены в именованных stash
и резервных файлах; разрешённые untracked — в отдельном архиве.
Резервная копия: amocrm-pro/tmp/staging-sync-20261006 (игнорируется Git).
Логи проверок: /private/tmp/teamos-sync-20261006.

## Перенесённое поведение

Core: DP receiver JSON/form, ограниченные DP credentials, JWT read reuse,
передача времени исходного события, admin diagnostics, миграции 22/23.
TeamOS backend: runtime route, графики, выбор источника запуска, миграция 29,
DP credential issue и редактирование сохранённого результата, защита гонки
DP/status_changed. TeamOS UI: выбор источника и сгенерированный контракт.
Admin: отдельная обработка 403 и DP диагностика. Виджет: runtime и DP.

Сохранены локальные изменения обучения TeamOS и существующий полный
amocrm-pro-service 0.6.2; отдельная Activity-интеграция не изменялась.
Конфликты manifest/i18n решены сохранением custom settings и добавлением DP.
Серверные gofmt-ошибки исправлены. В Admin устранено ложное сообщение
«событий нет» и сохранена совместимость со старым Core без DP блока.
Безопасные compose/nginx-шаблоны помещены в deploy/distribution-staging.

## Сборки

Полный ZIP 0.6.2 SHA256:
943d5cfef9527c5f80f87a4cb9daba037d0a479a517bbcdffef97213ba85770c.
Runtime bundle SHA256:
ce6a9cbc0f0ae97f866f36ef44f1eef0e80b3e9586639ecdb9c0df9cc6ff2949.
Обе сборки совпали с ранее подготовленными/опубликованными байтами.
Чистая серверная версия TeamOS прошла 399 тестов и сборку; SHA256
index.html совпал с опубликованным серверным файлом:
c6b1883ee84cbb1905518d73f67e70e8b51ccce8194de319e94cb130ed18e0a0.
ZIP-файлы и dist не включаются в Git; сохраняются исходники и упаковщик.

## Проверки

- TeamOS UI: contract check, 325 тестов, build, lint (0 ошибок).
- Чистый серверный TeamOS UI: 399 тестов и build; index SHA совпал.
- Company: unit/vet и интеграционный TestDistribution PASS (101 секунда).
- Gateway: unit/vet PASS. Генерация SQL/OpenAPI побайтово совпала.
- Widget module: 17 тестов и package PASS; полный пакет: 10 тестов PASS.
- Core: fmt/vet, race unit, OpenAPI и integration-test PASS.
  Миграции up/down, защитный отказ down и concurrent up проверены в
  отдельном проекте amocrm-pro-staging-sync-test.
- Admin: fmt/vet/lint, race unit, 136 UI tests, integration, 39 E2E,
  scripts-test (11 doubles), vulncheck PASS (0 reachable vulnerabilities).
  Проверки выполнены по целям Makefile: первоначальный make check
  остановился на конкуренции тестовой БД, после исправления все оставшиеся
  цели прошли. Это не единый повторный успешный вызов make check.
- Compose: оба шаблона config PASS. Nginx: -t с тестовым сертификатом PASS.
- Скан исходников: JWT, приватные ключи и GitHub-токены не обнаружены.

Первый Admin integration запуск выявил конкуренцию пакетов за общую
тестовую БД и пятисекундную advisory-блокировку. В тестовом compose
установлен GOFLAGS=-p=1; все интеграционные пакеты прошли последовательно.
Рабочие проверки прав и таймауты приложения не изменялись.

Первый Core integration запуск остановился из-за отсутствующего docker
compose subcommand. Повтор выполнен установленным docker-compose.
Host-проверка Admin UI не запустилась из-за Linux native dependencies;
канонический Docker workflow успешно выполнил все 136 тестов.

Через pi в VS Code повторно сверены серверные tracked-diff и все новые
исходники: изменений после аудита нет. Core worker и company healthy,
ready endpoints 200; image ID не изменились. Ничего не деплоилось.

## Ограничения

Сервер и БД во время переноса не изменялись, миграции staging не переигрывались.
Работающие серверные образы без revision labels нельзя доказательно связать
с исходниками. Следующий rollout должен собирать образы из чистых Git-коммитов
и фиксировать revision/digest. Установленная в amoCRM версия требует отдельной
проверки. Новая локальная сборка TeamOS содержит сохранённые изменения обучения
и не обязана совпадать с опубликованной серверной сборкой.

Серверная защита DP mirror использует допуск <= 1 секунды. Он перенесён
без изменения семантики; различение настоящего повторного входа через одну
секунду остаётся отдельным предметом проверки, а не доказанным свойством.

## Зафиксированные исходники

| Проект | Коммит исходников |
| --- | --- |
| amocrm-pro | 78a6448800cbde8d6342e513e1e48c6aec8cf4d8 |
| amocrm-pro-admin | 3e70f9d15b621a74b628b6394ba24ee739757757 |
| team-os-backend | 42387e9e3a560c1641909cddb201896a2d492f7b |
| team-os | 4c003f26ddda0d6cca3ca6d6f7b8ae14431df0e0 |
| rakurs-widgets-new | 5c815cc4e229f504793bb40c989ceb215e63410d |

Отчёт и индекс ADR Core фиксируются отдельным коммитом документации.
Ветки опубликованы в GitHub по запросу пользователя. Созданы пять PR;
переключение серверных исходников и развёртывание не выполнялись.
Исходники и работающие процессы — разные состояния: источник сервера
повторно сверён с аудитом, но новый согласованный Git-комплект ещё не выложен.

В TeamOS осталась прежняя незавершённая работа по удалению старых разделов
обучения; в коммит распределения включены только два серверных файла.
В Admin остались прежние root node_modules и package-lock.json.
В widget repo остались прежние .DS_Store. Секреты и сборки не коммитились.

## Pull requests

- [amocrm-pro](https://github.com/sk1fy/amocrm-pro/pull/61)
- [amocrm-pro-admin](https://github.com/sk1fy/amocrm-pro-admin/pull/13)
- [team-os-backend](https://github.com/sk1fy/team-os-backend/pull/36)
- [team-os](https://github.com/sk1fy/team-os/pull/10)
- [rakurs-widgets-new](https://github.com/sk1fy/rakurs-widgets-new/pull/2)
