# Activity v0: критерии до запуска процессной нагрузки

Дата: 2026-09-06. Baseline исходного кода: `a33a845`.

Стенд: локальный Docker, PostgreSQL 17 (общий instance), отдельные logical DB,
runtime/migration roles; Core API, один worker/Gateway/policy, Activity, CRM Events.
В process harness upstream amoCRM синтетический: это не установленный виджет E2E.
Core jobs 2 (integration cap1), CRM Events workers2, backfill concurrent1;
DB pools Core6+6, Activity3, Events5. Process fixture может явно уменьшить pools;
фактическая конфигурация должна быть напечатана вместе с результатом.

Функциональные gates: нет duplicate events/operations; conflict на изменённый
payload с тем же ключом; нет progress на partial/unstable окно; stale fence
отвергается; unavailable recipient после Core202 оставляет durable pending и
повторяется; разные installation/integration/actor изолированы; forged/expired/
wrong-audience/nonadmin context отвергается; direct CONNECT к чужим БД отвергается;
standalone env содержит только own DSN; режимы дают одинаковый полезный panel и
сохраняют данные, jobs, operations при embedded→grpc→embedded.

Для нагрузки заранее выбраны: synthetic CRM backlog до 10 000 событий со страницей
100 и два collector slots; второй widget ping — 50 запросов с concurrency2,
отдельный integration. Сравнить idle baseline с активным сбором. Acceptance:
errors0; P95<1s; P99<2s; loaded P95 ≤ max(3×baseline P95,100ms). Это локальные
пределы стенда, не production SLO. Снять P95/P99, errors, Core pool empty/canceled
acquire и duration, CRM backlog, amoCRM wait/request budget. Если выполнен меньший
набор или проверен только ping вместо lead-status end-to-end, указать ограничение.

Отдельно выполнить существующие race/vet/OpenAPI и Core/widget PostgreSQL tests.
Process smoke, ограниченный benchmark и mock upstream не доказывают production
ресурсную изоляцию или amoCRM browser E2E. Неисполненные проверки остаются такими
в отчёте, даже если их тесты/инструкции уже добавлены.

Дополнение, зафиксированное до запуска повторной продуктовой серии: кроме ping
выполнить 30 настоящих lead-status jobs в каждой фазе (idle/loaded), два closed-loop
callers, отдельные lead IDs, чтобы не считать no-op за проверку мутации. Измерить
admission и durable completion P95/P99. Условия: errors0; admission P95<1s/P99<2s;
completion P95<3s/P99<5s; loaded completion P95 ≤ max(3×idle P95,1s). Loaded phase
выполняется при том же backlog 10 000 events / page100 / Events workers2; подтвердить
активный collector во время фазы. Это обеспечивает повторный тест бизнес-продукта
в дополнение к инфраструктурному ping и одному lead-status smoke.
