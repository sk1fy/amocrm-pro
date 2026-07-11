# Recovery index: GitHub Issues и project memory

Этот файл — компактный индекс для восстановления контекста. Он **не является backlog, status board или копией acceptance checklist**. Канонические status, scope, acceptance criteria и dependencies находятся только в GitHub Issues.

## Канонические фазы

| Phase | Epic | Канонический Issue |
| --- | --- | --- |
| P0 | Dockerized Go foundation | [#3](https://github.com/sk1fy/amocrm-pro/issues/3) |
| P1 | OAuth and installation lifecycle | [#4](https://github.com/sk1fy/amocrm-pro/issues/4) |
| P2 | Resilient amoCRM client | [#5](https://github.com/sk1fy/amocrm-pro/issues/5) |
| P3 | Webhook subscriptions and reconciliation | [#9](https://github.com/sk1fy/amocrm-pro/issues/9) |
| P4 | Durable webhook ingress and inbox | [#7](https://github.com/sk1fy/amocrm-pro/issues/7) |
| P5 | PostgreSQL durable jobs | [#6](https://github.com/sk1fy/amocrm-pro/issues/6) |
| P6 | Secure widget API | [#8](https://github.com/sk1fy/amocrm-pro/issues/8) |
| P7 | Domain workflows and synchronization | [#10](https://github.com/sk1fy/amocrm-pro/issues/10) |
| P8 | Production hardening and integration readiness | [#11](https://github.com/sk1fy/amocrm-pro/issues/11) |

Агрегированный roadmap: [#12](https://github.com/sk1fy/amocrm-pro/issues/12). Любая другая нумерация фаз в старом checkpoint, PR или комментарии считается исторической и не переопределяет эту таблицу и titles GitHub Issues.

## Единственная роль каждого артефакта

- **Issue** — текущее состояние, scope, acceptance criteria и dependencies. Atomic Issue описывает bounded slice; epic Issue агрегирует atomic Issues.
- **PR** — реализация одного конкретного bounded slice и ссылки на закрываемые или обновляемые Issues.
- **ADR** — принятое архитектурное решение, его контекст и последствия. Backlog решений ведётся в [#13](https://github.com/sk1fy/amocrm-pro/issues/13), но текст решения живёт в ADR.
- **Checkpoint** — проверяемые evidence и handoff конкретной сессии: commits/PR, фактически выполненные проверки, риски и следующий канонический Issue. Checkpoint не хранит backlog, status checklist или собственный resume order.
- **README** — user-facing текущее состояние, запуск и использование. README не управляет backlog.
- **CONTEXT.md** — устойчивые факты и ограничения проекта; не status board и не очередь работ.

Если локальная память расходится с GitHub Issue, для status/scope/acceptance/dependencies побеждает Issue. Если Issue расходится с уже merged реализацией, Issue нужно синхронизировать до выбора следующего slice.

## Протокол после merge

После каждого merge ответственный агент должен в одном memory-sync проходе:

1. Отметить реально выполненные acceptance criteria в atomic Issue, приложив ссылки на PR/CI/checkpoint evidence.
2. Закрыть atomic Issue, если весь его acceptance выполнен; частично выполненный Issue оставить открытым с явным остатком scope.
3. Обновить checklist родительского epic ссылкой на atomic Issue или merged PR.
4. Закрыть epic только когда выполнен его собственный acceptance, а не только отдельный slice.
5. Обновить [#12](https://github.com/sk1fy/amocrm-pro/issues/12) только агрегированным статусом фаз и ссылками на epic Issues; не дублировать детальные checklists.
6. Обновить [#13](https://github.com/sk1fy/amocrm-pro/issues/13), если merge добавил, заменил или закрыл архитектурное решение.
7. Создать новый checkpoint с evidence/handoff. В `Next` указать ссылку на текущий открытый atomic Issue (или epic Issue, если atomic slice ещё предстоит выделить), а не локальный resume order.

## Восстановление следующей сессии

1. Перейти на актуальный `main` и найти последний merged PR/checkpoint.
2. Открыть [#12](https://github.com/sk1fy/amocrm-pro/issues/12), затем соответствующий phase epic из таблицы выше.
3. Проверить linked atomic Issues и выбрать bounded slice по их текущему status, acceptance и dependencies.
4. Использовать последний checkpoint только как evidence/handoff; его `Next` обязан вести в текущий Issue.
5. Если GitHub checklists не отражают merged PR, сначала выполнить протокол синхронизации выше и только затем начинать новую реализацию.

Исторические checkpoints сохраняются как evidence и не переписываются задним числом. Устаревший resume order в них следует игнорировать в пользу текущего Issue.
