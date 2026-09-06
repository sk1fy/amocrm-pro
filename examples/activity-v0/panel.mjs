// A small adapter for an existing amoCRM widget. Authentication is injected;
// this module never reads, stores, manufactures, or reuses a widget JWT.
const rootPath = "/api/v1/widget/activity";
const coverageLabels = {
  unknown: "История ещё не проверена",
  partial: "Период проверен частично",
  stale: "Данные требуют обновления",
  verified: "Выбранный период проверен",
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
export function observationLabel(summary, coverage) {
  const count = summary?.unique_events;
  if (coverage === "unknown" || !Number.isSafeInteger(count)) return "Нет проверенных данных";
  if (coverage !== "verified" && count === 0) return "Полных данных нет";
  return `${count} зарегистрировано${coverage === "verified" ? "" : " (данные неполные или устарели)"}`;
}
export function operationLabel(receipt) {
  return operationLabels[receipt?.state] || "Состояние неизвестно";
}
export function panelQuery(from, to, users) {
  const start = Math.floor(new Date(from).getTime() / 1000);
  const end = Math.floor(new Date(to).getTime() / 1000);
  if (!Number.isSafeInteger(start) || start <= 0 || !Number.isSafeInteger(end) || end <= start || end - start > 31 * 86400) {
    throw new Error("Выберите период до 31 дня с окончанием после начала.");
  }
  const ids = users.trim() ? users.split(",").map((id) => id.trim()) : [];
  if (ids.length > 100 || ids.some((id) => !/^[1-9]\d*$/.test(id) || !Number.isSafeInteger(Number(id))) || new Set(ids).size !== ids.length) {
    throw new Error("Укажите до 100 разных ID сотрудников через запятую.");
  }
  const query = new URLSearchParams({ from: String(start), to: String(end), limit: "100" });
  if (ids.length) query.set("user_ids", ids.join(","));
  return query;
}

export function mountActivityPanel(element, authorizedRequest) {
  if (!element || typeof authorizedRequest !== "function") throw new TypeError("A mount element and authorizedRequest are required");
  const doc = element.ownerDocument;
  let destroyed = false, busy = false, timer, pollCount = 0, receipt = null, pendingCommand = null, activeQuery = null;
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
  const status = make("div"); status.setAttribute("aria-live", "polite");
  const form = make("form", undefined, "amocrm-activity-v0__controls");
  const input = (label, type, value, parent = form) => {
    const wrap = make("label", label), field = make("input"); field.type = type; field.value = value; wrap.append(field); parent.append(wrap); return field;
  };
  const localTime = (date) => new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
  const now = new Date();
  const from = input("Начало (время браузера)", "datetime-local", localTime(new Date(now.getTime() - 86400000)));
  const to = input("Окончание (время браузера)", "datetime-local", localTime(now));
  const users = input("ID сотрудников, через запятую", "text", ""); users.maxLength = 2000;
  const button = (label, callback, parent = form) => { const b = make("button", label); b.type = "button"; b.addEventListener("click", callback); parent.append(b); return b; };
  const refreshButton = button("Показать", () => run(() => load()));
  const syncButton = button("Синхронизировать", () => run(() => command("sync", { kind: "sync" })));
  button("Догрузить выбранный период", () => run(() => {
    const q = panelQuery(from.value, to.value, users.value);
    return command("sync", { kind: "backfill", from: Number(q.get("from")), to: Number(q.get("to")) });
  }));
  button("Приостановить сбор", () => run(() => command("sync", { kind: "disable" })));
  const operation = make("p"); operation.setAttribute("aria-live", "polite");
  const operationActions = make("div");
  const pollButton = button("Проверить операцию", () => run(() => poll()), operationActions); pollButton.disabled = true;
  const retryButton = button("Повторить доставку запроса", () => run(() => submitPending()), operationActions); retryButton.hidden = true;
  const table = make("table");
  const eventTable = make("table");
  const pageControls = make("div");
  const nextButton = button("Следующая страница событий", () => run(() => load(true)), pageControls); nextButton.disabled = true;
  const settings = make("form", undefined, "amocrm-activity-v0__controls");
  const initial = input("Начальная глубина, дней", "number", "2", settings); initial.min = "1"; initial.max = "7";
  const retention = input("Хранение, дней", "number", "7", settings); retention.min = "2"; retention.max = "30";
  button("Сохранить настройки", () => run(() => {
    const initialDays = Number(initial.value), retentionDays = Number(retention.value);
    if (!Number.isInteger(initialDays) || initialDays < 1 || initialDays > 7 || !Number.isInteger(retentionDays) || retentionDays < 2 || retentionDays > 30 || retentionDays < initialDays) throw new Error("Глубина: 1–7 дней. Хранение: 2–30 дней и не меньше глубины.");
    return command("settings", { initial_days: initialDays, retention_days: retentionDays });
  }), settings);
  for (const f of [form, settings]) f.addEventListener("submit", (event) => event.preventDefault());
  container.append(heading, note, form, error, status, table, make("h3", "События выбранной страницы"), eventTable, pageControls, operation, operationActions, make("h3", "Настройки"), settings, make("p", "Новые настройки применяются к следующему запросу синхронизации. Приостановка сбора сохраняет доступ к истории; отключением продукта управляет оператор."));
  element.replaceChildren(container);

  const formatTime = (unix, timezone) => {
    if (!unix) return "неизвестно";
    try { return new Intl.DateTimeFormat("ru-RU", { dateStyle: "short", timeStyle: "medium", ...(timezone ? { timeZone: timezone } : {}) }).format(new Date(unix * 1000)); }
    catch { return new Date(unix * 1000).toISOString(); }
  };
  async function request(path, method = "GET", body, key) {
    const result = await authorizedRequest({ path: rootPath + path, method, body, headers: key ? { "Idempotency-Key": key } : {} });
    if (destroyed) throw new Error("Панель закрыта");
    if (!result || result.status < 200 || result.status >= 300) {
      const code = result?.body?.error?.code;
      const messages = { permission_denied: "Нет допуска Activity или подтверждённых прав администратора.", unauthenticated: "Авторизация запроса не подтверждена.", reauth_required: "Нужна повторная авторизация установки.", conflict: "Ключ запроса уже использован с другими параметрами.", invalid_argument: "Параметры выходят за допустимые границы.", resource_exhausted: "Лимит запросов. Повторите позже.", rate_limited: "Лимит запросов. Повторите позже." };
      const failure = new Error(messages[code] || "Сервис временно недоступен. Состояние выполнения не подтверждено.");
      failure.status = result?.status || 0;
      throw failure;
    }
    return result.body;
  }
  async function run(action) {
    if (destroyed || busy) return;
    busy = true; error.textContent = "";
    for (const b of container.querySelectorAll("button")) b.setAttribute("aria-disabled", "true");
    refreshButton.disabled = syncButton.disabled = true;
    try { await action(); } catch (err) { if (!destroyed) error.textContent = err.message || "Не удалось выполнить запрос"; }
    finally { busy = false; if (!destroyed) { refreshButton.disabled = syncButton.disabled = false; for (const b of container.querySelectorAll("button")) b.removeAttribute("aria-disabled"); } }
  }
  function renderRows(target, titles, rows) {
    const head = make("thead"), tr = make("tr"); for (const title of titles) tr.append(make("th", title)); head.append(tr);
    const body = make("tbody"); for (const row of rows) { const line = make("tr"); for (const value of row) line.append(make("td", String(value))); body.append(line); }
    target.replaceChildren(head, body);
  }
  async function load(next = false) {
    if (!next) activeQuery = panelQuery(from.value, to.value, users.value);
    if (!activeQuery) return;
    const panel = await request("/panel?" + activeQuery.toString());
    const freshness = panel.data.status;
    const format = (unix) => formatTime(unix, panel.timezone);
    status.replaceChildren(
      make("strong", coverageLabels[panel.coverage] || coverageLabels.unknown),
      make("p", `Часовой пояс аккаунта: ${panel.timezone || "неизвестно"}. Сбор: ${freshness.enabled ? freshness.state : "приостановлен или не настроен"}.`),
      make("p", `Хранимая история с ${format(freshness.history_from)}. Проверено: ${format(freshness.verified_from)} — ${format(freshness.verified_through)}.`),
      make("p", `Текущее окно: ${format(freshness.window_from)} — ${format(freshness.window_to)}, страница ${freshness.next_page || "—"}.`),
      make("p", `Последний успешный проход: ${format(freshness.last_success_at)}. Последнее событие: ${format(freshness.last_event_at)}. Отставание: ${freshness.verified_through ? `${freshness.lag_seconds} сек.` : "неизвестно"}.`),
      make("p", `Ошибка: ${freshness.error_code || "нет"}. Повторная авторизация: ${freshness.reauth_required ? "требуется" : "не требуется"}.`),
      make("p", freshness.verification === "stabilized_api_scan" ? "Проверка основана на повторном согласованном чтении API. Поздние изменения источника и недоступная история могут повлиять на полноту." : "Способ проверки истории пока не подтверждён."),
    );
    const summaries = new Map((panel.data.summaries || []).map((item) => [item.user_id, item]));
    renderRows(table, ["Сотрудник", "Группа", "События за период", "Последнее зарегистрированное событие"], (panel.users || []).map((user) => {
      const summary = summaries.get(user.id) || { unique_events: 0, last_event_at: 0 };
      return [user.name || String(user.id), user.group_name || "—", observationLabel(summary, panel.coverage), summary.last_event_at ? format(summary.last_event_at) : "Нет зарегистрированного события"];
    }));
    renderRows(eventTable, ["Время", "ID сотрудника", "Тип", "Сущность"], (panel.data.events || []).map((event) => [format(event.created_at), event.created_by, event.type, `${event.entity_type} ${event.entity_id}`]));
    nextButton.disabled = !panel.data.next_cursor;
    if (panel.data.next_cursor) activeQuery.set("cursor", panel.data.next_cursor);
    initial.value = String(panel.settings.initial_days); retention.value = String(panel.settings.retention_days);
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
      pendingCommand = null; retryButton.hidden = true; operation.textContent = operationLabel(receipt); pollButton.disabled = false; pollCount = 0; schedulePoll();
    } catch (err) {
      if ([400, 401, 403, 409, 429].includes(err.status)) {
        pendingCommand = null; retryButton.hidden = true; operation.textContent = "Запрос отклонён; новая операция не подтверждена.";
      } else {
        retryButton.hidden = false; operation.textContent = "Результат приёма неизвестен. Повтор использует тот же ключ и новые данные авторизации.";
      }
      throw err;
    }
  }
  function schedulePoll() {
    clearTimeout(timer);
    if (pollCount >= 12 || destroyed || ["succeeded", "failed", "paused"].includes(receipt?.state)) return;
    timer = setTimeout(() => { if (!busy) void run(poll); else schedulePoll(); }, 5000);
  }
  async function poll() {
    if (!receipt?.operation_id || receipt.state === "succeeded") return;
    // A manual check can finish before an already scheduled check fires.
    // Cancel that timer so a completed operation refreshes the panel once.
    clearTimeout(timer);
    pollCount++;
    receipt = await request("/operations/" + encodeURIComponent(receipt.operation_id));
    operation.textContent = `${operationLabel(receipt)}${receipt.error_code ? ` · ${receipt.error_code}` : ""}`;
    if (receipt.state === "succeeded") { pollButton.disabled = true; await load(); return; }
    schedulePoll();
  }
  void run(() => load());
  return { refresh: () => run(() => load()), destroy: () => { destroyed = true; clearTimeout(timer); element.replaceChildren(); } };
}
