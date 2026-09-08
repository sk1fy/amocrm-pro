// A small adapter for an existing amoCRM widget. Authentication is injected;
// this module never reads, stores, manufactures, or reuses a widget JWT.
const rootPath = "/api/v1/widget/activity";
const eventIdPattern = /^[A-Za-z0-9_.:-]{1,128}$/;
const coverageLabels = {
  unknown: "История ещё не проверена",
  partial: "Период проверен частично",
  stale: "Данные требуют обновления",
  verified: "Выбранный период проверен",
};
const freshnessLabels = {
  current: "Сбор актуален",
  lagging: "Сбор отстаёт; уже проверенный период не считается из‑за этого неполным",
  error: "Ошибка сбора",
  reauth_required: "Нужна повторная авторизация установки",
};
const operationLabels = {
  pending_delivery: "Принято платформой; ожидается доставка",
  accepted: "Принято сервисом",
  running: "Выполняется",
  retry: "Ожидается повтор выполнения",
  succeeded: "Завершено",
  failed: "Ошибка выполнения",
  paused: "Приостановлено",
};
const categoryLabels = {
  tasks: "Задачи",
  calls: "Звонки",
  notes: "Примечания",
  messages: "Сообщения",
  status: "Этапы",
  budget: "Бюджет",
  responsible: "Ответственные",
  custom_fields: "Поля",
  relations: "Связи",
  attachments: "Вложения",
  other: "Прочее",
};
const knownCategories = Object.keys(categoryLabels);
const entityPaths = { lead: "leads", leads: "leads", contact: "contacts", contacts: "contacts", company: "companies", companies: "companies", customer: "customers", customers: "customers" };
const periodError = "Выберите период до 31 дня с окончанием после начала.";
const usersError = "Укажите до 100 разных ID сотрудников через запятую.";
const zeroSummary = { unique_events: 0, first_event_at: 0, last_event_at: 0, entity_count: 0, task_completed_events: 0, unique_completed_tasks: 0, category_counts: [] };

export function observationLabel(summary, coverage, emptyReason) {
  const count = summary?.unique_events;
  if (coverage === "unknown" || !Number.isSafeInteger(count)) return "Нет проверенных данных";
  if (coverage === "verified" && count === 0) return "Нет событий за выбранный период";
  if (coverage !== "verified" && count === 0) return emptyReason === "unverified_empty" ? "0 зарегистрированных событий (покрытие неполное)" : "Полных данных нет";
  return `${count} зарегистрировано${coverage === "verified" ? "" : " (данные неполные или устарели)"}`;
}
export function operationLabel(receipt) {
  return operationLabels[receipt?.state] || "Состояние неизвестно";
}
export function isKnownTimezone(timezone) {
  if (!timezone || typeof timezone !== "string") return false;
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: timezone }).format(0);
    return true;
  } catch {
    return false;
  }
}
function zoneParts(ms, timezone) {
  const parts = {};
  for (const part of new Intl.DateTimeFormat("en-US", {
    timeZone: timezone, year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23",
  }).formatToParts(new Date(ms))) {
    if (part.type !== "literal") parts[part.type] = part.value;
  }
  return parts;
}
function pad2(value) {
  return String(value).padStart(2, "0");
}
export function formatDateTimeLocal(unix, timezone) {
  if (!Number.isFinite(unix) || !isKnownTimezone(timezone)) return "";
  const parts = zoneParts(unix * 1000, timezone);
  if (!parts.year) return "";
  const hour = parts.hour === "24" ? "00" : parts.hour;
  return `${parts.year}-${pad2(parts.month)}-${pad2(parts.day)}T${pad2(hour)}:${pad2(parts.minute)}:${pad2(parts.second)}`;
}
export function parseDateTimeLocal(value, timezone) {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(String(value || ""));
  if (!match || !isKnownTimezone(timezone)) throw new Error(periodError);
  const asUTC = Date.UTC(Number(match[1]), Number(match[2]) - 1, Number(match[3]), Number(match[4]), Number(match[5]), Number(match[6] || 0));
  const offset = (ms) => {
    const parts = zoneParts(ms, timezone);
    return Date.UTC(Number(parts.year), Number(parts.month) - 1, Number(parts.day), Number(parts.hour), Number(parts.minute), Number(parts.second || 0)) - ms;
  };
  let instant = asUTC - offset(asUTC);
  instant = asUTC - offset(instant);
  const unix = Math.floor(instant / 1000);
  if (!Number.isSafeInteger(unix) || unix <= 0) throw new Error(periodError);
  return unix;
}
function addUtcDays(ymd, days) {
  const [year, month, day] = ymd.split("-").map(Number);
  const date = new Date(Date.UTC(year, month - 1, day + days));
  return `${date.getUTCFullYear()}-${pad2(date.getUTCMonth() + 1)}-${pad2(date.getUTCDate())}`;
}
export function calendarDayRange(preset, timezone, nowMs = Date.now()) {
  if ((preset !== "today" && preset !== "yesterday") || !isKnownTimezone(timezone)) throw new Error("Часовой пояс аккаунта неизвестен.");
  const parts = zoneParts(nowMs, timezone);
  const today = `${parts.year}-${pad2(parts.month)}-${pad2(parts.day)}`;
  const startDay = addUtcDays(today, preset === "yesterday" ? -1 : 0);
  const from = parseDateTimeLocal(`${startDay}T00:00:00`, timezone);
  return { from, to: parseDateTimeLocal(`${addUtcDays(startDay, 1)}T00:00:00`, timezone) - 10 };
}
export function entityHref(entityType, entityId) {
  const kind = entityPaths[entityType];
  const id = typeof entityId === "number" ? entityId : Number(entityId);
  if (!kind || !Number.isSafeInteger(id) || id <= 0) return "";
  return `/${kind}/detail/${id}`;
}
function unixBound(value, timezone) {
  if (typeof value === "number") return value;
  if (timezone && typeof value === "string" && !/[zZ]|[+-]\d{2}:\d{2}$/.test(value)) return parseDateTimeLocal(value, timezone);
  return Math.floor(new Date(value).getTime() / 1000);
}
function userIds(users) {
  const raw = Array.isArray(users) ? users.map((id) => String(id).trim()) : String(users || "").trim() ? String(users).split(",").map((id) => id.trim()) : [];
  if (raw.length > 100 || raw.some((id) => !/^[1-9]\d*$/.test(id) || !Number.isSafeInteger(Number(id))) || new Set(raw).size !== raw.length) throw new Error(usersError);
  return raw;
}
export function panelQuery(from, to, users = "", options = {}) {
  const start = unixBound(from, options.timezone);
  const end = unixBound(to, options.timezone);
  if (!Number.isSafeInteger(start) || start <= 0 || !Number.isSafeInteger(end) || end <= start || end - start > 31 * 86400) throw new Error(periodError);
  const ids = userIds(users);
  const query = new URLSearchParams({ from: String(start), to: String(end), limit: "100" });
  if (ids.length) query.set("user_ids", ids.join(","));
  if (options.compact) query.set("compact", "true");
  const groupId = Number(options.groupId);
  if (Number.isSafeInteger(groupId) && groupId > 0) query.set("group_id", String(groupId));
  const categories = [...new Set((options.categories || []).filter((key) => categoryLabels[key]))];
  if (categories.length) query.set("categories", categories.join(","));
  return query;
}
function jsonText(value) {
  if (value == null || value === "") return "";
  if (typeof value === "string") {
    try {
      const parsed = JSON.parse(value);
      if (parsed && typeof parsed === "object") return JSON.stringify(parsed, null, 2);
    } catch { /* CRM text stays a plain string */ }
    return value;
  }
  if (typeof value === "object") {
    try { return JSON.stringify(value, null, 2); } catch { return String(value); }
  }
  return String(value);
}
function hasEventPayload(event) {
  if (event?.view?.details?.length) return true;
  return event?.value_before != null || event?.value_after != null;
}
function eventTitle(event) {
  return event?.view?.title || (event?.type ? `Событие ${event.type}` : "Событие");
}
function categoryMetrics(summary) {
  const parts = [`Задачи завершены: ${summary.task_completed_events || 0} (${summary.unique_completed_tasks || 0} уник.)`];
  for (const item of summary.category_counts || []) {
    if (categoryLabels[item.category]) parts.push(`${categoryLabels[item.category]}: ${item.count}`);
  }
  return parts.join("; ");
}
function emptyJournalLabel(panel) {
  if (panel?.empty_reason === "no_events") return "Нет событий за выбранный период";
  if (panel?.empty_reason === "unverified_empty") return "0 зарегистрированных событий при неполном покрытии";
  if (panel?.coverage === "unknown") return "Нет проверенных данных";
  return "Нет событий на этой странице";
}

export function mountActivityPanel(element, authorizedRequest) {
  if (!element || typeof authorizedRequest !== "function") throw new TypeError("A mount element and authorizedRequest are required");
  const doc = element.ownerDocument;
  let destroyed = false, commandBusy = false, timer, pollCount = 0, receipt = null, pendingCommand = null, activeQuery = null;
  // Unix seconds are the query source of truth; datetime-local is account-TZ display.
  let loadGen = 0, timezone = "", fromUnix = Math.floor(Date.now() / 1000) - 86400, toUnix = fromUnix + 86400;
  let lastPanel = null, payloadsOmitted = false, savedUserIds = null;
  const openEventIds = new Set(), cardCache = new Map(), fetchingIds = new Map(), directoryUsers = new Map(), events = [];
  let cardEpoch = 0;
  const seenIds = new Set();
  const make = (tag, text, className) => {
    const node = doc.createElement(tag);
    if (text !== undefined) node.textContent = text;
    if (className) node.className = className;
    return node;
  };
  const container = make("section", undefined, "amocrm-activity-v0");
  const heading = make("h2", "Activity · CRM-события");
  const note = make("p", "Здесь показаны зарегистрированные CRM-события. Их отсутствие не доказывает неактивность сотрудника.");
  const error = make("p", "", "amocrm-activity-v0__error"); error.setAttribute("role", "alert");
  const status = make("div", undefined, "amocrm-activity-v0__status"); status.setAttribute("aria-live", "polite");
  const diagnostics = make("details", undefined, "amocrm-activity-v0__diagnostics");
  diagnostics.append(make("summary", "Диагностика"));
  const diagnosticsBody = make("div");
  diagnostics.append(diagnosticsBody);
  const form = make("form", undefined, "amocrm-activity-v0__controls");
  const input = (label, type, value, parent = form) => {
    const wrap = make("label", label), field = make("input"); field.type = type; field.value = value; wrap.append(field); parent.append(wrap); return field;
  };
  const tzHint = make("p", "Часовой пояс аккаунта ещё не получен.", "amocrm-activity-v0__hint");
  form.append(tzHint);
  const from = input("Начало (часовой пояс аккаунта)", "datetime-local", "");
  const to = input("Окончание (часовой пояс аккаунта)", "datetime-local", "");
  from.step = to.step = "1";
  const button = (label, callback, parent = form) => { const b = make("button", label); b.type = "button"; b.addEventListener("click", callback); parent.append(b); return b; };
  const todayButton = button("Сегодня", () => applyPreset("today")); todayButton.disabled = true;
  const yesterdayButton = button("Вчера", () => applyPreset("yesterday")); yesterdayButton.disabled = true;
  const groupWrap = make("label", "Отдел");
  const groupSelect = make("select", undefined, "amocrm-activity-v0__groups");
  groupWrap.append(groupSelect); form.append(groupWrap);
  const userWrap = make("label", "Сотрудники");
  const userSelect = make("select", undefined, "amocrm-activity-v0__users"); userSelect.multiple = true; userSelect.size = 6;
  userWrap.append(userSelect); form.append(userWrap);
  const categoryBox = make("fieldset", undefined, "amocrm-activity-v0__categories");
  categoryBox.append(make("legend", "Категории"));
  const categoryInputs = knownCategories.map((key) => {
    const wrap = make("label"), box = make("input");
    box.type = "checkbox"; box.value = key;
    wrap.append(box, make("span", categoryLabels[key]));
    categoryBox.append(wrap);
    return box;
  });
  form.append(categoryBox);
  const advanced = make("details", undefined, "amocrm-activity-v0__advanced");
  advanced.append(make("summary", "Дополнительно: ID сотрудников"));
  const users = input("ID сотрудников, через запятую", "text", "", advanced); users.maxLength = 2000;
  form.append(advanced);
  const refreshButton = button("Показать", () => { void loadPanel(); });
  const syncButton = button("Синхронизировать", () => run(() => command("sync", { kind: "sync" })));
  const backfillButton = button("Догрузить выбранный период", () => run(() => {
    const q = buildQuery();
    return command("sync", { kind: "backfill", from: Number(q.get("from")), to: Number(q.get("to")) });
  }));
  const pauseButton = button("Приостановить сбор", () => run(() => command("sync", { kind: "disable" })));
  const backButton = button("Все выбранные сотрудники", () => restoreUsers()); backButton.hidden = true;
  const operation = make("p"); operation.setAttribute("aria-live", "polite");
  const operationActions = make("div");
  const pollButton = button("Проверить операцию", () => run(() => poll()), operationActions); pollButton.disabled = true;
  const retryButton = button("Повторить доставку запроса", () => run(() => submitPending()), operationActions); retryButton.hidden = true;
  const table = make("table");
  const journalNote = make("p", "", "amocrm-activity-v0__journal-status");
  const eventTable = make("table");
  const pageControls = make("div");
  const nextButton = button("Следующая страница событий", () => { void loadPanel({ append: true }); }, pageControls); nextButton.disabled = true;
  const settings = make("form", undefined, "amocrm-activity-v0__controls");
  const initial = input("Начальная глубина, дней", "number", "2", settings); initial.min = "1"; initial.max = "7";
  const retention = input("Хранение, дней", "number", "7", settings); retention.min = "2"; retention.max = "30";
  const saveButton = button("Сохранить настройки", () => run(() => {
    const initialDays = Number(initial.value), retentionDays = Number(retention.value);
    if (!Number.isInteger(initialDays) || initialDays < 1 || initialDays > 7 || !Number.isInteger(retentionDays) || retentionDays < 2 || retentionDays > 30 || retentionDays < initialDays) throw new Error("Глубина: 1–7 дней. Хранение: 2–30 дней и не меньше глубины.");
    return command("settings", { initial_days: initialDays, retention_days: retentionDays });
  }), settings);
  const commandButtons = [syncButton, backfillButton, pauseButton, saveButton, pollButton, retryButton];
  for (const f of [form, settings]) f.addEventListener("submit", (event) => event.preventDefault());
  container.append(heading, note, form, error, status, diagnostics, table, make("h3", "События выбранной страницы"), journalNote, eventTable, pageControls, operation, operationActions, make("h3", "Настройки"), settings, make("p", "Новые настройки применяются к следующему запросу синхронизации. Приостановка сбора сохраняет доступ к истории; отключением продукта управляет оператор."));
  element.replaceChildren(container);

  const formatTime = (unix, zone) => {
    if (!unix) return "неизвестно";
    try { return new Intl.DateTimeFormat("ru-RU", { dateStyle: "short", timeStyle: "medium", ...(zone ? { timeZone: zone } : {}) }).format(new Date(unix * 1000)); }
    catch { return new Date(unix * 1000).toISOString(); }
  };
  function selectedValues(select) {
    return [...select.children].filter((option) => option.selected && option.value).map((option) => option.value);
  }
  function selectedUserText() {
    const named = selectedValues(userSelect);
    return named.length ? named.join(",") : users.value;
  }
  function buildQuery() {
    let start = fromUnix, end = toUnix;
    if (timezone && from.value && to.value) {
      start = parseDateTimeLocal(from.value, timezone);
      end = parseDateTimeLocal(to.value, timezone);
    }
    return panelQuery(start, end, selectedUserText(), {
      compact: true,
      groupId: selectedValues(groupSelect)[0],
      categories: categoryInputs.filter((box) => box.checked).map((box) => box.value),
    });
  }
  function applyTimezone(panelTz) {
    if (!isKnownTimezone(panelTz)) {
      timezone = "";
      tzHint.textContent = "Часовой пояс аккаунта неизвестен; календарные пресеты недоступны.";
      todayButton.disabled = yesterdayButton.disabled = true;
      return;
    }
    timezone = panelTz;
    tzHint.textContent = `Период в часовом поясе аккаунта: ${timezone}`;
    todayButton.disabled = yesterdayButton.disabled = false;
    if (!from.value) from.value = formatDateTimeLocal(fromUnix, timezone);
    if (!to.value) to.value = formatDateTimeLocal(toUnix, timezone);
  }
  function applyPreset(preset) {
    if (!timezone) {
      error.textContent = "Часовой пояс аккаунта неизвестен.";
      return;
    }
    const range = calendarDayRange(preset, timezone);
    fromUnix = range.from; toUnix = range.to;
    from.value = formatDateTimeLocal(fromUnix, timezone);
    to.value = formatDateTimeLocal(toUnix, timezone);
    void loadPanel();
  }
  function fillSelect(select, items, multiple) {
    const selected = new Set(selectedValues(select));
    select.replaceChildren();
    for (const item of items) {
      const option = make("option", item.label);
      option.value = item.value;
      option.selected = selected.has(item.value);
      select.append(option);
    }
    if (!multiple && !selectedValues(select).length && select.children[0]) select.children[0].selected = true;
  }
  function fillDirectory(panelUsers, complete) {
    if (complete) directoryUsers.clear();
    for (const user of panelUsers || []) directoryUsers.set(String(user.id), user);
    panelUsers = [...directoryUsers.values()];
    const groups = new Map();
    fillSelect(userSelect, (panelUsers || []).map((user) => ({ value: String(user.id), label: user.name || `Пользователь #${user.id}` })), true);
    for (const user of panelUsers || []) {
      if (!user.group_id || groups.has(String(user.group_id))) continue;
      groups.set(String(user.group_id), user.group_name || `Отдел #${user.group_id}`);
    }
    fillSelect(groupSelect, [{ value: "", label: "Все отделы" }, ...[...groups].map(([value, label]) => ({ value, label }))], false);
  }
  function drillUser(userId) {
    if (savedUserIds === null) savedUserIds = { named: selectedValues(userSelect), manual: users.value, groups: selectedValues(groupSelect) };
    users.value = "";
    for (const option of userSelect.children) option.selected = option.value === String(userId);
    backButton.hidden = false;
    void loadPanel();
  }
  function restoreUsers() {
    const wanted = new Set(savedUserIds?.named || []);
    users.value = savedUserIds?.manual || "";
    const groups = new Set(savedUserIds?.groups || []);
    for (const option of groupSelect.children) option.selected = groups.has(option.value);
    for (const option of userSelect.children) option.selected = wanted.has(option.value);
    savedUserIds = null;
    backButton.hidden = true;
    void loadPanel();
  }
  async function request(path, method = "GET", body, key) {
    const result = await authorizedRequest({ path: rootPath + path, method, body, headers: key ? { "Idempotency-Key": key } : {} });
    if (destroyed) throw new Error("Панель закрыта");
    if (!result || result.status < 200 || result.status >= 300) {
      const code = result?.body?.error?.code;
      const messages = { permission_denied: "Нет допуска Activity или подтверждённых прав администратора.", unauthenticated: "Авторизация запроса не подтверждена.", reauth_required: "Нужна повторная авторизация установки.", conflict: "Ключ запроса уже использован с другими параметрами.", invalid_argument: "Параметры выходят за допустимые границы.", resource_exhausted: "Лимит запросов. Повторите позже.", rate_limited: "Лимит запросов. Повторите позже." };
      const failure = new Error(path.startsWith("/events/") && result?.status === 404 ? "Событие недоступно" : (messages[code] || "Сервис временно недоступен. Состояние выполнения не подтверждено."));
      failure.status = result?.status || 0;
      throw failure;
    }
    return result.body;
  }
  async function run(action) {
    if (destroyed || commandBusy) return;
    commandBusy = true; error.textContent = "";
    for (const b of commandButtons) b.setAttribute("aria-disabled", "true");
    syncButton.disabled = true;
    try { await action(); } catch (err) { if (!destroyed) error.textContent = err.message || "Не удалось выполнить запрос"; }
    finally {
      commandBusy = false;
      if (!destroyed) {
        syncButton.disabled = false;
        for (const b of commandButtons) b.removeAttribute("aria-disabled");
      }
    }
  }
  function renderStatus(panel) {
    const empty = panel.empty_reason === "no_events" ? "Нет событий за выбранный период" : panel.empty_reason === "unverified_empty" ? "0 зарегистрированных событий при неполном покрытии" : "";
    status.replaceChildren(
      make("strong", coverageLabels[panel.coverage] || coverageLabels.unknown),
      make("p", freshnessLabels[panel.freshness] || ""),
      empty ? make("p", empty) : make("p", ""),
    );
  }
  function renderDiagnostics() {
    const panel = lastPanel;
    const freshness = panel?.data?.status || {};
    const format = (unix) => formatTime(unix, panel?.timezone);
    diagnosticsBody.replaceChildren(
      make("p", `Часовой пояс аккаунта: ${panel?.timezone || "неизвестно"}. Сбор: ${freshness.enabled ? freshness.state : "приостановлен или не настроен"}.`),
      make("p", freshness.verified_through ? `Данные проверены до ${format(freshness.verified_through)}.` : "Проверенная граница периода неизвестна."),
      make("p", `Хранимая история с ${format(freshness.history_from)}. Проверено: ${format(freshness.verified_from)} — ${format(freshness.verified_through)}.`),
      make("p", `Текущее окно: ${format(freshness.window_from)} — ${format(freshness.window_to)}, страница ${freshness.next_page || "—"}.`),
      make("p", `Последний успешный проход: ${format(freshness.last_success_at)}. Последнее событие: ${format(freshness.last_event_at)}. Отставание: ${freshness.verified_through ? `${freshness.lag_seconds} сек.` : "неизвестно"}.`),
      make("p", `Ошибка: ${freshness.error_code || "нет"}. Повторная авторизация: ${freshness.reauth_required ? "требуется" : "не требуется"}.`),
      make("p", freshness.verification === "stabilized_api_scan" ? "Проверка основана на повторном согласованном чтении API. Поздние изменения источника и недоступная история могут повлиять на полноту." : "Способ проверки истории пока не подтверждён."),
      make("p", `Версия интерпретации: ${panel?.interpretation_version || "—"}. Страница без больших payload: ${panel?.data?.payloads_omitted ? "да" : "нет"}.`),
      make("p", receipt ? `Операция ${receipt.operation_id || "—"} · ${receipt.delivery_state || receipt.state || "—"} · ${receipt.error_code || "нет"}` : "Операция не запрошена."),
    );
  }
  function renderSummary(panel) {
    const summaries = new Map((panel.data?.summaries || []).map((item) => [item.user_id, item]));
    const format = (unix) => formatTime(unix, panel.timezone);
    const head = make("thead"), header = make("tr");
    for (const title of ["Сотрудник", "Группа", "События за период", "Категории", "Первое", "Последнее"]) header.append(make("th", title));
    head.append(header);
    const body = make("tbody");
    for (const user of panel.users || []) {
      const summary = summaries.get(user.id) || zeroSummary;
      const row = make("tr");
      const name = make("td");
      const pick = make("button", user.name || String(user.id));
      pick.type = "button";
      pick.addEventListener("click", () => drillUser(user.id));
      name.append(pick);
      row.append(
        name,
        make("td", user.group_name || "—"),
        make("td", observationLabel(summary, panel.coverage, panel.empty_reason)),
        make("td", categoryMetrics(summary)),
        make("td", summary.first_event_at ? format(summary.first_event_at) : "—"),
        make("td", summary.last_event_at ? format(summary.last_event_at) : "Нет зарегистрированного события"),
      );
      body.append(row);
    }
    table.replaceChildren(head, body);
  }
  function entityCell(event) {
    const td = make("td");
    const href = entityHref(event.entity_type, event.entity_id);
    const label = event.view?.entity_label || [event.entity_type, event.entity_id].filter((part) => part !== undefined && part !== "").join(" ");
    if (href) {
      const link = make("a", label || href);
      link.href = href;
      link.rel = "noopener noreferrer";
      link.target = "_blank";
      link.setAttribute("href", href);
      link.setAttribute("rel", "noopener noreferrer");
      link.setAttribute("target", "_blank");
      td.append(link);
    } else td.textContent = label;
    return td;
  }
  function factBlock(item) {
    const block = make("div", undefined, "amocrm-activity-v0__fact");
    block.append(make("strong", item.label || item.key || "Поле"));
    if (item.text) block.append(make("p", item.text));
    if (item.before != null && item.before !== "") block.append(prePair("До", item.before));
    if (item.after != null && item.after !== "") block.append(prePair("После", item.after));
    if (item.source) block.append(make("p", `Источник: ${item.source}`));
    if (item.current) block.append(make("p", "Текущее значение справочника, не историческое до/после", "amocrm-activity-v0__current"));
    return block;
  }
  function prePair(label, value) {
    const wrap = make("div");
    wrap.append(make("p", label), make("pre", jsonText(value)));
    return wrap;
  }
  function detailCopy(event) {
    const view = event?.view || {};
    if (event?.id && fetchingIds.has(event.id)) return "Загрузка…";
    if (event?._unavailable) return "Событие недоступно";
    if (event?._retryable) return "Не удалось загрузить подробности. Закройте и откройте карточку для повтора.";
    if (view.detail_state === "unavailable") return "Подробности недоступны";
    if (view.detail_state === "pending") return "Подробности ещё загружаются";
    if (view.detail_state === "omitted" && !hasEventPayload(event)) return "Подробности не включены в эту страницу";
    if (view.enrichment_state === "pending") return "Обогащение ещё не готово";
    if (view.enrichment_state === "unavailable" || view.enrichment_state === "error" || view.enrichment_state === "retry") return "Обогащение недоступно";
    return "";
  }
  function renderCard(host, event) {
    host.replaceChildren();
    const view = event?.view || {};
    const state = detailCopy(event);
    if (state) host.append(make("p", state));
    if (view.details?.length) for (const item of view.details) host.append(factBlock(item));
    else if (event && (event.value_before != null || event.value_after != null)) {
      host.append(prePair("До", event.value_before), prePair("После", event.value_after));
    } else if (!state) host.append(make("p", "Нет подробностей для отображения"));
  }
  function eventRecord(id) {
    return cardCache.get(id) || events.find((item) => item.id === id);
  }
  function renderEvents(panel) {
    const format = (unix) => formatTime(unix, panel?.timezone);
    const head = make("thead"), header = make("tr");
    for (const title of ["Время", "Автор", "Событие", "Сущность", ""]) header.append(make("th", title));
    head.append(header);
    const body = make("tbody");
    for (const event of events) {
      const view = event.view || {};
      const row = make("tr");
      row.append(
        make("td", format(event.created_at)),
        make("td", view.author_label || String(event.created_by ?? "")),
        make("td", eventTitle(event)),
        entityCell(event),
      );
      const actions = make("td");
      const open = openEventIds.has(event.id);
      const more = make("button", open ? "Скрыть" : "Подробнее");
      more.type = "button";
      more.addEventListener("click", (ev) => { ev.stopPropagation?.(); void toggleEvent(event.id); });
      actions.append(more);
      row.append(actions);
      row.addEventListener("click", () => { void toggleEvent(event.id); });
      body.append(row);
      if (open) {
        const detailRow = make("tr", undefined, "amocrm-activity-v0__card");
        const cell = make("td");
        cell.colSpan = 5;
        cell.setAttribute("colspan", "5");
        const host = make("div");
        renderCard(host, eventRecord(event.id) || event);
        cell.append(host);
        detailRow.append(cell);
        body.append(detailRow);
      }
    }
    eventTable.replaceChildren(head, body);
    journalNote.textContent = events.length ? (payloadsOmitted ? "Крупные до/после загружаются при открытии карточки." : "") : emptyJournalLabel(panel || lastPanel || {});
  }
  function shouldFetchDetail(event) {
    // Cache within a panel read; refresh invalidates cards, including pending enrichment.
    const cached = cardCache.get(event.id);
    if (cached?._retryable || event._retryable) return true;
    if (hasEventPayload(cached) || hasEventPayload(event)) return false;
    if (cached?._unavailable || event.view?.detail_state === "unavailable" || event.view?.detail_state === "pending") return false;
    return event.view?.detail_state === "omitted" || payloadsOmitted;
  }
  async function toggleEvent(id) {
    if (openEventIds.has(id)) { openEventIds.delete(id); renderEvents(lastPanel); return; }
    openEventIds.add(id);
    renderEvents(lastPanel);
    await fetchDetail(id);
  }
  async function fetchDetail(id) {
    const event = eventRecord(id);
    if (!event || fetchingIds.has(id) || !shouldFetchDetail(event)) return;
    const epoch = cardEpoch;
    fetchingIds.set(id, epoch);
    renderEvents(lastPanel);
    try {
      if (!eventIdPattern.test(id)) throw Object.assign(new Error("Событие недоступно"), { status: 404 });
      const detail = await request("/events/" + encodeURIComponent(id));
      if (destroyed || epoch !== cardEpoch) return;
      cardCache.set(id, detail);
      error.textContent = "";
    } catch (err) {
      if (destroyed || epoch !== cardEpoch) return;
      cardCache.set(id, { ...(event || { id }), _unavailable: err.status === 404, _retryable: err.status !== 404, view: { ...(event?.view || {}), detail_state: "unavailable" } });
      if (!destroyed && err?.message) error.textContent = err.message;
    } finally {
      if (fetchingIds.get(id) === epoch) fetchingIds.delete(id);
      if (!destroyed && epoch === cardEpoch && openEventIds.has(id)) renderEvents(lastPanel);
    }
  }
  function acceptEvents(pageEvents, append) {
    if (!append) { events.length = 0; seenIds.clear(); }
    for (const event of pageEvents || []) {
      if (!event?.id || seenIds.has(event.id)) continue;
      seenIds.add(event.id);
      events.push(event);
      if (hasEventPayload(event)) cardCache.set(event.id, event);
    }
  }
  async function loadPanel(options = {}) {
    const gen = ++loadGen;
    const append = options.append === true;
    if (!append) {
      try { activeQuery = buildQuery(); }
      catch (err) { error.textContent = err.message || periodError; return; }
    }
    if (!activeQuery) return;
    if (!options.keepOpen && !append) {
      error.textContent = "";
      journalNote.textContent = "Загрузка…";
      openEventIds.clear();
    }
    try {
      const query = new URLSearchParams(activeQuery);
      const panel = await request("/panel?" + query.toString());
      // Ignore a slower previous panel response after a newer filter/read.
      if (destroyed || gen !== loadGen) return;
      if (!append) {
        cardEpoch++;
        cardCache.clear();
        fetchingIds.clear();
      }
      lastPanel = panel;
      payloadsOmitted = Boolean(panel.data?.payloads_omitted);
      fromUnix = Number(query.get("from")) || fromUnix;
      toUnix = Number(query.get("to")) || toUnix;
      applyTimezone(panel.timezone);
      fillDirectory(panel.users, !query.has("user_ids") && !query.has("group_id"));
      renderStatus(panel);
      renderDiagnostics();
      renderSummary(panel);
      acceptEvents(panel.data?.events, append);
      nextButton.disabled = !panel.data?.next_cursor;
      if (panel.data?.next_cursor) activeQuery.set("cursor", panel.data.next_cursor);
      else activeQuery.delete("cursor");
      if (panel.settings) {
        initial.value = String(panel.settings.initial_days);
        retention.value = String(panel.settings.retention_days);
      }
      renderEvents(panel);
      if (!append) {
        for (const id of openEventIds) {
          if (gen !== loadGen || destroyed) break;
          await fetchDetail(id);
        }
      }
    } catch (err) {
      if (destroyed || gen !== loadGen) return;
      error.textContent = err.message || "Не удалось выполнить запрос";
      journalNote.textContent = "Не удалось загрузить журнал";
    }
  }
  async function command(path, body) {
    if (pendingCommand) throw new Error("Сначала повторите запрос с неизвестным результатом; его ключ сохранён в этой панели.");
    pendingCommand = { path, body, key: globalThis.crypto.randomUUID() };
    await submitPending();
  }
  async function submitPending() {
    if (!pendingCommand) return;
    try {
      receipt = await request("/" + pendingCommand.path, "POST", pendingCommand.body, pendingCommand.key);
      pendingCommand = null; retryButton.hidden = true; operation.textContent = operationLabel(receipt); pollButton.disabled = false; pollCount = 0; renderDiagnostics(); schedulePoll();
    } catch (err) {
      if ([400, 401, 403, 409, 429].includes(err.status)) {
        pendingCommand = null; retryButton.hidden = true; operation.textContent = "Запрос отклонён; новая операция не подтверждена.";
      } else {
        retryButton.hidden = false; operation.textContent = "Результат приёма неизвестен. Повтор использует тот же ключ и новые данные авторизации.";
      }
      renderDiagnostics();
      throw err;
    }
  }
  function schedulePoll() {
    clearTimeout(timer);
    if (pollCount >= 12 || destroyed || ["succeeded", "failed", "paused"].includes(receipt?.state)) return;
    timer = setTimeout(() => { if (!commandBusy) void run(poll); else schedulePoll(); }, 5000);
  }
  async function poll() {
    if (!receipt?.operation_id || receipt.state === "succeeded") return;
    // A manual check can finish before an already scheduled check fires.
    // Cancel that timer so a completed operation refreshes the panel once.
    clearTimeout(timer);
    pollCount++;
    receipt = await request("/operations/" + encodeURIComponent(receipt.operation_id));
    operation.textContent = operationLabel(receipt);
    renderDiagnostics();
    if (receipt.state === "succeeded") { pollButton.disabled = true; await loadPanel({ keepOpen: true }); return; }
    schedulePoll();
  }
  void loadPanel();
  return { refresh: () => loadPanel({ keepOpen: true }), destroy: () => { destroyed = true; clearTimeout(timer); element.replaceChildren(); } };
}
