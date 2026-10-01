"use strict";
// Offline review artifact. No fetch, storage, timers for business logic or CRM effects.
const state = {
  surface: "team",
  scenario: "day",
  role: "manager",
  data: "ready",
  access: "allow",
  page: "queue",
  filter: "all",
  selected: "waiting_shift",
  revision: 7,
  draftRevision: 7,
  keep: true,
  paused: false,
  conflict: false,
  evidence: "unresolved",
  recoveryResolved: false,
  overrides: {},
};
const dictionary = {
  checking: [
    "Проверяется",
    "info",
    "Проверяем этап и доступность сотрудников.",
  ],
  waiting_shift: [
    "Ожидает смены",
    "wait",
    "Сейчас нет сотрудников на смене. Повторная проверка завтра в 09:00.",
  ],
  needs_configuration: [
    "Требует настройки",
    "bad",
    "Для участников не найден подходящий рабочий график.",
  ],
  paused: [
    "Приостановлено",
    "wait",
    "Группа приостановлена. Очередь сохранена.",
  ],
  waiting_previous_operation: [
    "Ожидает предыдущую операцию",
    "unknown",
    "Уточняем предыдущую попытку для этой сделки. Новое назначение пока не начнётся.",
  ],
  assigning: [
    "Назначается",
    "info",
    "Команда принята. Ждём подтверждения от amoCRM.",
  ],
  outcome_unknown: [
    "Результат уточняется",
    "unknown",
    "Ответ не получен. Сначала проверим, кто назначен в amoCRM.",
  ],
  assigned: ["Назначена", "ok", "Назначение подтверждено в amoCRM."],
  kept: [
    "Ответственный сохранён",
    "ok",
    "Текущий ответственный доступен и входит в группу.",
  ],
  unchanged: [
    "Распределена без изменения",
    "ok",
    "По очереди выбран текущий ответственный. Смена в amoCRM не требовалась.",
  ],
  cancelled: ["Отменена", "neutral", "Сделка вышла с настроенного этапа."],
  error: [
    "Ошибка",
    "bad",
    "Не удалось выполнить назначение. Получатель отключён в amoCRM.",
  ],
};
const el = (id) => document.getElementById(id);
const esc = (value) =>
  String(value).replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const badge = (key) =>
  `<span class="badge ${dictionary[key]?.[1] || "unknown"}">${dictionary[key]?.[0] || "Неизвестно"}</span>`;
const isManager = () => state.role === "manager";
const canOperate = () => state.role === "manager" || state.role === "operator";
const unavailable = () =>
  ["loading", "error", "unknown", "empty"].includes(state.data);
const writable = () => isManager() && state.data === "ready";
const action = (name, label, enabled = true, primary = false) =>
  `<button data-action="${name}" ${enabled ? "" : "disabled"} class="${primary ? "primary" : ""}">${label}</button>`;
function notice(text) {
  el("announcement").textContent = `Макет: ${text}`;
  el("announcement").classList.add("visible");
}
function currentStatus() {
  return state.scenario === "day"
    ? "assigned"
    : state.scenario === "night"
      ? "waiting_shift"
      : state.recoveryResolved
        ? "assigned"
        : "outcome_unknown";
}
function setPage(page) {
  state.page = page;
  state.draftRevision = state.revision;
  state.conflict = false;
  render();
}
function shell(content) {
  const navs =
    state.surface === "team"
      ? [
          ["groups", "Группы"],
          ["queue", "Очередь сделок"],
          ["employees", "Сотрудники"],
          ["history", "История"],
          ["settings", "Настройки"],
        ]
      : state.surface === "widget"
        ? [
            ["installation", "Подключение"],
            ["settings", "Общие настройки"],
            ["lead", "Карточка сделки"],
          ]
        : [
            ["health", "Подключение"],
            ["delivery", "Доставка"],
            ["operations", "Операции"],
          ];
  const brand =
    state.surface === "team"
      ? "Team<span>OS</span>"
      : state.surface === "widget"
        ? "rakurs<span>.</span>"
        : "amo<span>pro</span> / admin";
  return `<div class="shell ${state.surface}-shell"><aside class="sidebar"><div class="brand">${brand}</div><div class="nav-label">${state.surface === "team" ? "РАСПРЕДЕЛЕНИЕ" : state.surface === "widget" ? "ВНУТРИ amoCRM" : "ДИАГНОСТИКА"}</div><nav class="nav" aria-label="Разделы">${navs.map(([key, label]) => `<button data-page="${key}" ${state.page === key ? 'class="active" aria-current="page"' : ""}>${label}</button>`).join("")}</nav></aside><main class="workspace"><div class="breadcrumbs">Демо-компания / Распределение / ${state.surface === "admin" ? "Тестовое подключение" : "Входящие обращения"}</div>${content}</main></div>`;
}
function heading(title, description, buttons = "") {
  return `<div class="heading"><div><h1>${title}</h1><p>${description}</p></div><div class="actions">${buttons}</div></div>`;
}
function freshness() {
  if (state.data === "stale")
    return `<div class="banner"><strong>Данные могли измениться</strong><p>Последнее наблюдение: 01.10.2026, 09:30 (Москва). Изменения временно недоступны.</p>${action("refresh", "Обновить данные")}</div>`;
  if (state.data === "unknown")
    return `<div class="banner neutral"><strong>Данные ещё не получены</strong><p>Время последнего наблюдения неизвестно. Отсутствие наблюдения не означает отсутствие сделок.</p></div>`;
  return "";
}
function placeholder() {
  if (state.data === "loading")
    return `<section class="panel" aria-busy="true" aria-label="Загрузка данных"><div class="section-head"><h2>Загружаем данные…</h2></div>${Array.from({ length: 5 }, (_, i) => `<div class="skeleton ${i % 2 ? "short" : ""}"></div>`).join("")}</section>`;
  if (state.data === "error")
    return `<section class="panel empty" role="alert"><div class="symbol">!</div><h2>Не удалось получить данные</h2><p>Источник временно недоступен. Это не пустая очередь. Сохранённые назначения продолжает обрабатывать сервер.</p>${action("refresh", "Повторить чтение", true, true)}</section>`;
  if (state.data === "unknown")
    return `<section class="panel empty"><div class="symbol">—</div><h2>Нет наблюдения</h2><p>Состояние источника пока неизвестно. Значения и счётчики не заменяются нулями.</p>${action("refresh", "Получить данные")}</section>`;
  if (state.data === "empty")
    return `<section class="panel empty"><div class="symbol">○</div><h2>${state.page === "groups" ? "Группы ещё не созданы" : "Пока нет данных"}</h2><p>Источник ответил успешно. Здесь появятся ${state.page === "employees" ? "доступные участники" : "сделки и события"} после настройки распределения.</p>${action("setup", "Открыть настройки", isManager(), true)}</section>`;
  return "";
}
function accessBlocked() {
  if (state.access === "allow") return "";
  return `<section class="panel empty"><div class="symbol">◇</div><h2>${state.access === "deny" ? "Нет доступа к сделкам" : "Не удалось проверить права"}</h2><p>${state.access === "deny" ? "Названия, сотрудники и число недоступных вам сделок не показываются. Запросите доступ у администратора." : "Данные сделок скрыты до успешной серверной проверки. Это не означает пустую очередь."}</p>${state.access === "unavailable" ? action("check-access", "Повторить проверку") : ""}</section>`;
}
function summary() {
  return `<div class="cards"><div class="card"><div class="metric-label">${state.role === "employee" ? "Доступные мне назначения" : "Подтверждённые назначения"} · сегодня</div><div class="metric">${state.scenario === "day" ? "12" : "8"}</div></div><div class="card"><div class="metric-label">Доступные вам сделки · ожидают смены</div><div class="metric">${state.scenario === "night" ? "3" : "1"}</div></div><div class="card"><div class="metric-label">На смене · из 3 участников</div><div class="metric">${state.scenario === "night" ? "0" : "2"}</div></div></div>`;
}
function team() {
  const buttons =
    state.page === "queue"
      ? action(
          "pause",
          state.paused ? "Возобновить группу" : "Приостановить группу",
          writable(),
        ) + action("setup", "Настройки", isManager())
      : "";
  const names = {
    groups: "Группы распределения",
    queue: "Входящие обращения",
    employees: "Сотрудники группы",
    history: "История решений",
    settings: "Настройки распределения",
  };
  let top =
    heading(
      names[state.page] || names.queue,
      state.page === "queue"
        ? "Продажи → Новая заявка · По очереди · Москва"
        : state.page === "settings"
          ? "Одни правила для TeamOS и виджета amoCRM"
          : "Демо-компания · Europe/Moscow",
      buttons,
    ) + freshness();
  if (unavailable()) return top + placeholder();
  if (["queue", "history"].includes(state.page) && state.access !== "allow")
    return top + accessBlocked();
  if (state.page === "settings") return top + settings();
  if (state.page === "employees") return top + employees();
  if (state.page === "history") return top + history();
  if (state.page === "groups")
    return (
      top +
      `<section class="panel"><div class="section-head"><h2>Группы <span class="pill-number">1</span></h2>${action("setup", "Настроить группу", isManager())}</div><div class="body"><div class="heading"><div><h2>Входящие обращения</h2><p>Продажи → Новая заявка</p></div>${badge(state.paused ? "paused" : "assigned")}</div><div class="mini-grid"><div><small>Распределение</small><p>По очереди · 3 участника</p></div><div><small>Очередь</small><p>${state.access === "allow" ? (state.scenario === "night" ? "3 ожидают смены" : "1 требует внимания") : "Сведения о сделках недоступны"}</p></div></div><div class="divider"></div>${action("open-queue", "Открыть очередь", true, true)}</div></section>`
    );
  return (
    top +
    (state.paused
      ? `<div class="banner">Группа приостановлена. Ожидание сохранено, результаты отправленных операций уточняются.</div>`
      : "") +
    summary() +
    queue()
  );
}
function queue() {
  const keys = [
    currentStatus(),
    ...[
      "checking",
      "waiting_shift",
      "needs_configuration",
      "paused",
      "waiting_previous_operation",
      "assigning",
      "kept",
      "unchanged",
      "cancelled",
      "error",
    ].filter((k) => k !== currentStatus()),
  ];
  const rows = keys.filter((original) => {
    const k = state.overrides[original] || original;
    return (
      state.filter === "all" ||
      (state.filter === "waiting" &&
        [
          "waiting_shift",
          "needs_configuration",
          "paused",
          "waiting_previous_operation",
        ].includes(k)) ||
      (state.filter === "done" &&
        ["assigned", "kept", "unchanged", "cancelled"].includes(k)) ||
      (state.filter === "attention" && ["outcome_unknown", "error"].includes(k))
    );
  });
  return `<div class="tabs" aria-label="Фильтры очереди">${[
    ["all", "Все"],
    ["waiting", "Ожидают"],
    ["done", "Завершены"],
    ["attention", "Требуют внимания"],
  ]
    .map(
      ([v, l]) =>
        `<button data-filter="${v}" class="${state.filter === v ? "active" : ""}" aria-pressed="${state.filter === v}">${l}</button>`,
    )
    .join(
      "",
    )}</div><section class="panel"><div class="section-head"><div><h2>Сделки</h2><p>Обновлено: 01.10.2026, ${state.scenario === "night" ? "22:10" : "10:42"} (Москва) · показаны доступные вам сделки</p></div>${action("refresh", "Обновить")}</div><div class="table-wrap"><table class="queue-table"><thead><tr><th>Сделка</th><th>Состояние</th><th>Ответственный / план</th><th>Следующий шаг</th><th></th></tr></thead><tbody>${
    rows.length
      ? rows
          .map((original, i) => {
            const k = state.overrides[original] || original;
            return `<tr><td data-label="Сделка"><strong>${i === 0 ? "Заявка на консультацию" : `Тестовая заявка ${i + 1}`}</strong><small>#${1001 + i} · Новая заявка</small></td><td data-label="Состояние">${badge(k)}</td><td data-label="Ответственный / план"><strong>${k === "assigned" ? "Елена Волкова" : "Иван Орлов"}</strong><small>${k === "waiting_shift" ? "План: Елена, завтра в 09:00 · предварительно" : k === "assigning" ? "План: Елена · ждём подтверждения" : "—"}</small></td><td data-label="Следующий шаг">${k === "outcome_unknown" ? "Проверить результат" : k === "needs_configuration" ? "Настроить график" : k === "waiting_shift" ? "Автоматически после смены" : k === "error" ? "Устранить причину" : "См. историю решения"}</td><td class="right">${`<button data-detail="${original}">Подробнее</button>`}</td></tr>`;
          })
          .join("")
      : `<tr><td colspan="5">В выбранном фильтре сделок нет.</td></tr>`
  }</tbody></table></div></section>`;
}
function employees() {
  return `<section class="panel"><div class="section-head"><div><h2>Участники · порядок распределения</h2><p>Доступность определяет сервер по графику и данным amoCRM</p></div>${action("schedule", "Графики", true)}</div><div class="table-wrap"><table><thead><tr><th>Порядок / сотрудник</th><th>Доступность сейчас</th><th>Следующая смена</th><th>Связь с amoCRM</th></tr></thead><tbody>${[
    ["Елена Волкова", "ЕВ", "1"],
    ["Иван Орлов", "ИО", "2"],
    ["Анна Миронова", "АМ", "3"],
  ]
    .map(
      ([n, a, num], i) =>
        `<tr><td><div class="people"><span>${num}</span><span class="avatar">${a}</span><strong>${n}</strong></div></td><td><span class="badge ${i === 2 ? "wait" : state.scenario === "night" ? "neutral" : "ok"}">${i === 2 ? "Не настроен график" : state.scenario === "night" ? "Вне смены" : "На смене"}</span><small>${i === 2 ? "График не задан" : state.scenario === "night" ? "Рабочий день завершён" : "Сегодня 09:00–18:00"}</small></td><td>${i === 2 ? "Не определена" : "Завтра, 09:00"}</td><td>${i === 2 ? "Требует сопоставления" : "Подтверждена"}</td></tr>`,
    )
    .join(
      "",
    )}</tbody></table></div><div class="body"><p class="note">Порядок меняет администратор в настройках. Онлайн-присутствие не определяет доступность.</p></div></section>`;
}
function history() {
  return `<section class="panel"><div class="section-head"><h2>Последние решения</h2><span class="muted">Сегодня · Москва</span></div><div class="body"><ol class="timeline"><li><time>10:42:08</time><p><strong>Назначение подтверждено</strong> — сделку получила Елена Волкова.</p></li><li><time>10:42:06</time><p>Отправлена команда назначения. Ожидали подтверждения amoCRM.</p></li><li><time>10:42:05</time><p>Выбрана Елена — следующая доступная сотрудница по очереди.</p></li><li><time>10:42:04</time><p>Сделка вошла на этап «Новая заявка».</p></li></ol><p class="note">История реальных решений доступна для просмотра. Очистка и сброс очереди отсутствуют.</p></div></section>`;
}
function settings() {
  return `${!isManager() ? '<div class="banner neutral">Доступен просмотр. Изменять правила может администратор вашей компании.</div>' : ""}${state.conflict ? '<div class="banner error" role="alert"><strong>Правила изменены в другом интерфейсе</strong><p>Ваш черновик сохранён. Загрузите актуальную версию, сравните изменения и сохраните снова.</p>' + action("load-version", "Загрузить актуальную версию") + "</div>" : ""}<section class="panel"><div class="section-head"><div><h2>Общие правила</h2><p>Версия ${state.revision} · изменения видны в обоих интерфейсах</p></div>${badge(state.paused ? "paused" : "assigned")}</div><div class="body"><div class="form-grid"><div class="field"><label for="group-name">Название группы</label><input id="group-name" value="Входящие обращения" readonly></div><div class="field"><label for="pipeline">Воронка и этап</label><select id="pipeline" ${writable() ? "" : "disabled"}><option>Продажи → Новая заявка</option></select></div><div class="field"><label for="algorithm">Способ распределения</label><select id="algorithm" ${writable() ? "" : "disabled"}><option>По очереди</option></select><small>Участники выбираются среди доступных по графику.</small></div><div class="field"><label for="timezone">Часовой пояс компании</label><input id="timezone" value="Europe/Moscow · Москва" readonly></div><div class="full check"><input id="keep" type="checkbox" ${state.keep ? "checked" : ""} ${writable() ? "" : "disabled"}><div><label for="keep"><strong>Сохранять доступного текущего ответственного</strong></label><p>Если он входит в группу и сейчас работает, сделка остаётся у него. Эта настройка включена по умолчанию.</p></div></div><div class="full"><h3>Порядок участников</h3><div class="chip-list"><span class="chip">1. Елена Волкова</span><span class="chip">2. Иван Орлов</span><span class="chip">3. Анна Миронова</span></div><p class="note">Порядок показан как пример. Графики и исключения редактируются в TeamOS.</p></div></div><div class="divider"></div><div class="actions">${action("save", "Сохранить правила", writable(), true)}${action("schedule", "Открыть графики в TeamOS", true)}${action("conflict", "Смоделировать параллельную правку", writable())}</div><p class="note">В макете сохраняется только переключатель сохранения ответственного. «Смоделировать правку» повышает общую версию: следующее сохранение демонстрирует конфликт.</p></div></section>`;
}
function widget() {
  const title =
    state.page === "installation"
      ? "Подключение распределения"
      : state.page === "settings"
        ? "Настройки виджета"
        : "Распределение в карточке";
  let top =
    heading(title, "Виджет amoCRM · демонстрационная установка") + freshness();
  if (unavailable()) return top + placeholder();
  if (state.page === "settings") return top + settings();
  if (state.page === "installation")
    return (
      top +
      `<section class="panel"><div class="body"><h2>Подключить общие правила TeamOS</h2><div class="install-step"><span class="step-num">1</span><div><h3>Подключение amoCRM</h3><p>Демо-аккаунт авторизован. Секреты хранятся на сервере.</p><span class="badge ok">Подключение подтверждено</span></div></div><div class="install-step"><span class="step-num">2</span><div><h3>Компания TeamOS</h3><p>Демо-компания. Связь подтверждается администратором компании и администратором amoCRM.</p>${action("binding", "Показать подтверждение связи", isManager())}</div></div><div class="install-step"><span class="step-num">3</span><div><h3>Группа и графики</h3><p>Входящие обращения → Новая заявка. 2 из 3 сотрудников сопоставлены.</p>${action("setup", "Открыть общие настройки", true, true)}</div></div><p class="note">Это макет подключения. Реальные OAuth, установка ZIP и проверка прав будут реализованы на следующих этапах.</p></div></section>`
    );
  if (state.access !== "allow") return top + accessBlocked();
  const k = currentStatus();
  return (
    top +
    `<div class="split"><section class="panel"><div class="section-head"><h2>Заявка на консультацию</h2><span class="muted">#1001</span></div><div class="body"><small>Демонстрационная карточка amoCRM</small><dl class="kv"><dt>Воронка</dt><dd>Продажи</dd><dt>Этап</dt><dd>Новая заявка</dd><dt>Ответственный</dt><dd>${k === "assigned" ? "Елена Волкова" : "Иван Орлов"}</dd></dl><p class="note">Левая часть — контекст карточки. Справа — компактный блок нового виджета.</p></div></section><section class="widget-card"><h2>rakurs. Распределение</h2>${badge(k)}<p class="note">${dictionary[k][2]}</p><dl class="kv"><dt>Группа</dt><dd>Входящие обращения</dd><dt>Текущий ответственный</dt><dd>${k === "assigned" ? "Елена Волкова" : "Иван Орлов"}</dd><dt>План передачи</dt><dd>${k === "waiting_shift" ? "Елена · завтра, 09:00" : "—"}</dd></dl>${k === "waiting_shift" ? '<p class="note">Получатель предварительный. Перед назначением график проверяется снова.</p>' : ""}<p class="note">Обновлено 01.10.2026, ${state.scenario === "night" ? "22:10" : "10:42"} (Москва)</p><div class="actions"><button data-detail="${k}">История решения</button>${k === "outcome_unknown" ? action("reconcile", "Проверить результат", writable()) : ""}</div>${!isManager() ? '<p class="note">Вы можете просматривать доступные вам сделки. Управление — у администратора компании.</p>' : ""}</section></div>`
  );
}
function observation(name, value, extra = "") {
  const unavailableData = ["error", "unknown", "loading"].includes(state.data);
  const label =
    state.data === "error"
      ? "Источник недоступен"
      : state.data === "unknown"
        ? "Неизвестно"
        : state.data === "stale"
          ? "Устарело"
          : "Актуально";
  return `<div class="card"><div><div class="metric-label">${name}</div><div class="status-line"><strong>${unavailableData ? "—" : value}</strong><span class="badge ${state.data === "ready" ? "ok" : state.data === "stale" ? "wait" : "unknown"}">${label}</span></div><p class="note">${state.data === "unknown" ? "Наблюдения ещё нет" : "Наблюдение: 01.10.2026, " + (state.data === "stale" ? "09:30" : "10:42") + " (Москва)"}</p>${extra}</div></div>`;
}
function admin() {
  const headingText = {
    health: "Состояние подключения",
    delivery: "Доставка событий",
    operations: "Операции назначения",
  };
  let top =
    heading(
      headingText[state.page],
      "Демо-аккаунт · Распределение · данные владельца",
      action("refresh", "Обновить чтение"),
    ) + freshness();
  if (state.data === "loading") return top + placeholder();
  if (state.data === "empty")
    return (
      top +
      `<section class="panel empty"><h2>Подключений нет</h2><p>Источник ответил успешно. Для выбранной области нет настроенного подключения распределения.</p></section>`
    );
  const controlsAllowed = canOperate() && state.data === "ready";
  const roleNote =
    state.role === "employee"
      ? '<div class="banner neutral">Роль: наблюдатель. Диагностика доступна, команды скрыты.</div>'
      : "";
  if (state.page === "health")
    return (
      top +
      roleNote +
      `<div class="cards">${observation("Подключение amoCRM", "Активно")}${observation("Подписка на события", "Подтверждена")}${observation("Связь с TeamOS", "Подтверждена")}</div><section class="panel"><div class="section-head"><h2>Диагностика</h2><span class="badge ${state.data === "ready" ? "ok" : "unknown"}">${state.data === "ready" ? "Источник ответил" : "Нужна проверка"}</span></div><div class="body"><dl class="kv"><dt>Модуль распределения</dt><dd>${state.data === "ready" ? "Включён" : "—"}</dd><dt>Последнее доставленное событие</dt><dd>${state.data === "ready" ? "Сегодня, 10:42:04" : "—"}</dd><dt>Событий в ожидании</dt><dd>${state.data === "ready" ? "0" : "—"}</dd><dt>Возраст самого старого</dt><dd>${state.data === "ready" ? "Нет ожидающих" : "—"}</dd></dl><div class="divider"></div><div class="actions">${state.role === "employee" ? "" : action("connection-check", "Проверить подключение", controlsAllowed) + action("subscription", "Восстановить подписку", controlsAllowed) + action("module-pause", "Приостановить модуль", controlsAllowed)}</div><p class="note">Проверка подключения — отдельная операция. Принятие запроса не означает успешное восстановление.</p></div></section>`
    );
  if (
    state.data === "error" ||
    state.data === "unknown" ||
    state.data === "empty"
  )
    return top + placeholder();
  if (state.page === "delivery")
    return (
      top +
      roleNote +
      `<div class="cards">${observation("Ожидают доставки", "1")}${observation("Возраст старейшего", "2 мин")}${observation("Последний успех", "10:42:04")}</div><section class="panel"><div class="section-head"><h2>Сохранённое событие</h2><span class="badge wait">Повтор запланирован</span></div><div class="body"><dl class="kv"><dt>Получатель</dt><dd>TeamOS</dd><dt>Причина</dt><dd>Временная недоступность</dd><dt>Следующая попытка</dt><dd>Сегодня, 10:45</dd><dt>Корреляция</dt><dd class="mono">demo-event-01</dd></dl><div class="actions">${state.role === "employee" ? "" : action("redeliver", "Повторить доставку", controlsAllowed)}</div><p class="note">Повторяет доставку этого события с прежним идентификатором. Не создаёт новое назначение.</p></div></section>`
    );
  return (
    top +
    roleNote +
    `<section class="panel"><div class="section-head"><h2>Операция назначения</h2>${badge(state.recoveryResolved ? "assigned" : "outcome_unknown")}</div><div class="body"><dl class="kv"><dt>Операция</dt><dd class="mono">demo-operation-01</dd><dt>Состояние</dt><dd>${state.recoveryResolved ? "Результат подтверждён" : "Ответ от amoCRM потерян"}</dd><dt>Наблюдение</dt><dd>01.10.2026, 10:42</dd></dl><div class="banner neutral"><strong>${state.recoveryResolved ? "Завершение попытки подтверждено" : "Сначала уточните результат"}</strong><p>${state.recoveryResolved ? "Результат подтверждён владельцем. Новая команда не требуется." : "Совпадение получателя при чтении не доказывает завершение ранее отправленного запроса. Повторная команда недоступна."}</p></div><div class="actions">${state.role === "employee" || state.recoveryResolved ? "" : action("admin-reconcile", "Проверить результат", controlsAllowed, true)}</div><p class="note">Сверка читает фактическое состояние. Не назначает нового получателя. Неизвестный результат не отображается как ошибка с кнопкой слепого повтора.</p></div></section>`
  );
}
function detail(original) {
  if (state.access !== "allow") return;
  state.selectedOriginal = original;
  const key = state.overrides[original] || original;
  state.selected = key;
  el("dialog-body").innerHTML =
    `<h2>Заявка на консультацию</h2>${badge(key)}<p class="note">${dictionary[key][2]}</p><dl class="kv"><dt>Группа</dt><dd>Входящие обращения</dd><dt>Текущий ответственный</dt><dd>${key === "assigned" ? "Елена Волкова" : "Иван Орлов"}</dd><dt>План</dt><dd>${key === "waiting_shift" ? "Елена · предварительно" : "—"}</dd><dt>Следующая проверка</dt><dd>${key === "waiting_shift" ? "Завтра, 09:00" : "По состоянию операции"}</dd></dl><div class="divider"></div><ol class="timeline"><li><time>10:42:04</time><p>Получено событие входа на этап.</p></li><li><time>10:42:05</time><p>${dictionary[key][2]}</p></li></ol><div class="divider"></div><div class="actions">${["outcome_unknown", "waiting_previous_operation"].includes(key) ? action("reconcile", "Проверить результат", writable()) : ["waiting_shift", "needs_configuration", "paused", "error"].includes(key) ? action("recalculate", "Пересчитать ожидание", writable()) : ""}${["waiting_shift", "needs_configuration", "paused", "checking"].includes(key) ? action("cancel", "Отменить ожидание", writable()) : ""}${action("crm", "Открыть в amoCRM", false)}</div><p class="note">Ссылка на amoCRM отключена: сделка вымышленная. Все действия здесь — демонстрация.</p>`;
  el("detail").showModal();
}
function render() {
  el("app").innerHTML = shell(
    state.surface === "team"
      ? team()
      : state.surface === "widget"
        ? widget()
        : admin(),
  );
}
for (const id of ["surface", "scenario", "role", "data", "access", "evidence"])
  el(id).addEventListener("change", (e) => {
    state[id] = e.target.value;
    if (id === "surface")
      state.page =
        state.surface === "team"
          ? "queue"
          : state.surface === "widget"
            ? "lead"
            : "health";
    if (id === "scenario") {
      state.data = "ready";
      el("data").value = "ready";
      state.filter = "all";
      state.overrides = {};
      state.recoveryResolved = false;
    }
    if (el("detail").open) el("detail").close();
    el("announcement").classList.remove("visible");
    render();
  });
document.addEventListener("click", (event) => {
  const btn = event.target.closest("button");
  if (!btn || btn.disabled) return;
  if (btn.dataset.page) {
    setPage(btn.dataset.page);
    return;
  }
  if (btn.dataset.filter) {
    state.filter = btn.dataset.filter;
    render();
    return;
  }
  if (btn.dataset.detail) {
    detail(btn.dataset.detail);
    return;
  }
  const a = btn.dataset.action;
  if (!a) return;
  if (a === "setup") {
    setPage("settings");
    return;
  }
  if (a === "open-queue") {
    setPage("queue");
    return;
  }
  if (a === "refresh") {
    state.data = "ready";
    el("data").value = "ready";
    render();
    notice("чтение обновлено на демонстрационных данных.");
    return;
  }
  if (a === "check-access") {
    notice(
      "проверка прав не подключена. Измените «Доступ к сделке» в панели макета.",
    );
    return;
  }
  if (a === "schedule") {
    if (state.surface === "widget") {
      state.surface = "team";
      el("surface").value = "team";
    }
    setPage("employees");
    notice(
      "показан пример перехода к графикам TeamOS; редактор графиков вне этого макета.",
    );
    return;
  }
  if (a === "conflict") {
    state.revision += 1;
    notice(
      "другой интерфейс сохранил правила. Нажмите «Сохранить правила» для проверки конфликта.",
    );
    return;
  }
  if (a === "load-version") {
    state.draftRevision = state.revision;
    state.conflict = false;
    render();
    notice(
      "актуальная версия загружена; ваш переключатель сохранён для сравнения.",
    );
    return;
  }
  if (a === "save") {
    const keep = el("keep");
    if (keep) state.keep = keep.checked;
    if (state.draftRevision !== state.revision) {
      state.conflict = true;
      render();
      return;
    }
    state.revision += 1;
    state.draftRevision = state.revision;
    render();
    notice(
      "правила сохранены локально. Переключите интерфейс — настройки общие.",
    );
    return;
  }
  if (a === "pause") {
    state.paused = !state.paused;
    render();
    notice(
      state.paused
        ? "группа приостановлена; неопределённые результаты требуют сверки."
        : "группа возобновлена с повторной проверкой сделок.",
    );
    return;
  }
  if (a === "reconcile" || a === "admin-reconcile") {
    if (el("detail").open) el("detail").close();
    if (state.evidence === "unresolved") {
      notice(
        "получатель совпадает, но завершение прежнего запроса не доказано. Результат остаётся неопределённым; новый запрос запрещён.",
      );
      return;
    }
    state.recoveryResolved = true;
    if (state.selectedOriginal)
      state.overrides[state.selectedOriginal] = "assigned";
    render();
    notice(
      "завершение прежней попытки и её результат подтверждены в демонстрации. Повторного запроса не было.",
    );
    return;
  }
  if (a === "cancel") {
    const original = state.selectedOriginal;
    state.overrides[original] = "cancelled";
    el("detail").close();
    render();
    detail(original);
    notice("ожидание отменено в строке очереди и карточке макета.");
    return;
  }
  if (a === "recalculate") {
    const original = state.selectedOriginal;
    state.overrides[original] = "waiting_shift";
    el("detail").close();
    render();
    detail(original);
    notice("план ожидания обновлён в строке очереди и карточке макета.");
    return;
  }
  if (a === "binding") {
    notice(
      "демонстрация: обе стороны связи подтверждены. Настоящая авторизация не выполнялась.",
    );
    return;
  }
  const messages = {
    "connection-check":
      "проверка принята; её результат будет отдельным наблюдением.",
    subscription: "восстановление подписки принято. Успех ещё не подтверждён.",
    "module-pause":
      "запрос паузы принят. Результаты отправленных операций ещё нужно выяснить.",
    redeliver:
      "событие повторно передано с прежним идентификатором; нового назначения не создано.",
  };
  notice(messages[a] || "действие не подключено к серверу.");
});
render();
