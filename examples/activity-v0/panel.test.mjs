import test from "node:test";
import { readFile } from "node:fs/promises";
import assert from "node:assert/strict";
import {
  calendarDayRange, entityHref, formatDateTimeLocal, isKnownTimezone, observationLabel,
  operationLabel, panelQuery, parseDateTimeLocal, mountActivityPanel,
} from "./panel.mjs";

test("unknown and incomplete observations never claim zero employee activity", () => {
  assert.equal(observationLabel({ unique_events: 0 }, "unknown"), "0 зарегистрировано; полнота периода не подтверждена");
  assert.equal(observationLabel({ unique_events: 0 }, "partial"), "0 зарегистрировано; полнота периода не подтверждена");
  assert.equal(observationLabel({ unique_events: 0 }, "stale"), "0 зарегистрировано; полнота периода не подтверждена");
  assert.equal(observationLabel({ unique_events: 0 }, "verified"), "Нет событий за выбранный период");
  assert.equal(observationLabel({ unique_events: 0 }, "partial", "unverified_empty"), "0 зарегистрировано; полнота периода не подтверждена");
  assert.equal(observationLabel({ unique_events: 0 }, "verified", "no_events"), "Нет событий за выбранный период");
  assert.match(observationLabel({ unique_events: 4 }, "partial"), /^4 зарегистрировано/);
});
test("Core acceptance is shown as delivery pending, never completion", () => {
  assert.match(operationLabel({ state: "pending_delivery" }), /ожидается доставка/);
  assert.equal(operationLabel({ state: "succeeded" }), "Завершено");
  assert.equal(operationLabel({ state: "retry" }), "Ожидается повтор выполнения");
  assert.equal(operationLabel({ state: "completed" }), "Состояние неизвестно");
  assert.equal(operationLabel({ state: "unexpected" }), "Состояние неизвестно");
});
test("bounded employee period filter supplies no caller identity", () => {
  const q = panelQuery("2026-09-01T10:00:00Z", "2026-09-02T10:00:00Z", "7, 9");
  assert.equal(q.get("user_ids"), "7,9"); assert.equal(q.get("limit"), "100"); assert.equal(q.has("actor_id"), false);
  for (const users of ["7,7", "-1", "abc", "9007199254740992", Array.from({ length: 101 }, (_, i) => i + 1).join(",")]) assert.throws(() => panelQuery("2026-09-01", "2026-09-02", users));
  assert.throws(() => panelQuery("2026-01-01", "2026-09-02", ""));
  assert.throws(() => panelQuery("bad", "2026-09-02", ""));
});
test("panel query sends group, categories, compact and never actor_id", () => {
  const q = panelQuery(100, 200, "7", { compact: true, groupId: 3, categories: ["tasks", "nope", "calls"] });
  assert.equal(q.get("from"), "100");
  assert.equal(q.get("to"), "200");
  assert.equal(q.get("compact"), "true");
  assert.equal(q.get("group_id"), "3");
  assert.equal(q.get("categories"), "tasks,calls");
  assert.equal(q.has("actor_id"), false);
  assert.equal(q.has("include_unknown_authors"), false);
});
test("today and yesterday use account timezone calendar days", () => {
  const now = Date.UTC(2026, 8, 8, 12, 0, 0);
  const moscow = calendarDayRange("today", "Europe/Moscow", now);
  assert.equal(formatDateTimeLocal(moscow.from, "Europe/Moscow"), "2026-09-08T00:00:00");
  assert.equal(formatDateTimeLocal(moscow.to, "Europe/Moscow"), "2026-09-08T23:59:50");
  assert.equal(moscow.to - moscow.from, 86390);
  const utcYesterday = calendarDayRange("yesterday", "UTC", now);
  assert.equal(formatDateTimeLocal(utcYesterday.from, "UTC"), "2026-09-07T00:00:00");
  assert.equal(parseDateTimeLocal("2026-09-08T15:00", "Europe/Moscow"), now / 1000);
  assert.equal(formatDateTimeLocal(parseDateTimeLocal("2026-09-08T10:00", "UTC"), "UTC"), "2026-09-08T10:00:00");
  assert.equal(isKnownTimezone(""), false);
  assert.throws(() => calendarDayRange("today", "", now));
});
test("entityHref allows only CRM object paths", () => {
  assert.equal(entityHref("leads", 15), "/leads/detail/15");
  assert.equal(entityHref("lead", 15), "/leads/detail/15");
  assert.equal(entityHref("contacts", 2), "/contacts/detail/2");
  assert.equal(entityHref("company", 3), "/companies/detail/3");
  assert.equal(entityHref("customers", 4), "/customers/detail/4");
  assert.equal(entityHref("task", 9), "");
  assert.equal(entityHref("note", 9), "");
  assert.equal(entityHref("talk", 9), "");
  assert.equal(entityHref("unknown", 9), "");
});

class Element {
  constructor(tag, document) {
    this.tagName = tag; this.ownerDocument = document; this.children = []; this.listeners = new Map();
    this.attributes = new Map(); this.text = ""; this.className = ""; this.hidden = false; this.dataset = {};
    this.disabled = false; this.value = ""; this.type = ""; this.checked = false; this.selected = false;
    this.href = ""; this.rel = ""; this.target = ""; this.open = false;
  }
  set textContent(value) { this.text = String(value); this.children = []; }
  get textContent() { return this.text + this.children.map((child) => child.textContent).join(""); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.text = ""; this.children = children; }
  setAttribute(name, value) {
    this.attributes.set(name, String(value));
    if (name === "class") this.className = String(value);
    if (name === "href") this.href = String(value);
    if (name === "rel") this.rel = String(value);
    if (name === "target") this.target = String(value);
    if (name.startsWith("data-")) this.dataset[name.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase())] = String(value);
  }
  removeAttribute(name) { this.attributes.delete(name); }
  addEventListener(name, callback) { this.listeners.set(name, callback); }
  matches(selector) {
    if (selector.startsWith(".")) return this.className.split(/\s+/).includes(selector.slice(1));
    const [tag, cls] = selector.split(".");
    if (tag && this.tagName !== tag) return false;
    return !cls || this.className.split(/\s+/).includes(cls);
  }
  querySelectorAll(selector) {
    return this.children.flatMap((child) => [...(child.matches(selector) ? [child] : []), ...child.querySelectorAll(selector)]);
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  click() { if (!this.disabled) return this.listeners.get("click")?.({ preventDefault() {}, stopPropagation() {} }); }
}

function panelBody(overrides = {}) {
  return {
    users: overrides.users ?? [
      { id: 7, name: "Alpha", group_id: 3, group_name: "Sales" },
      { id: 9, name: "Beta", group_id: 3, group_name: "Sales" },
    ],
    timezone: overrides.timezone ?? "Europe/Moscow",
    coverage: overrides.coverage ?? "verified",
    freshness: overrides.freshness ?? "current",
    empty_reason: overrides.empty_reason ?? "",
    interpretation_version: 1,
    settings: { initial_days: 2, retention_days: 7 },
    data: {
      events: overrides.events ?? [],
      summaries: overrides.summaries ?? [],
      next_cursor: overrides.next_cursor ?? "",
      payloads_omitted: overrides.payloads_omitted ?? true,
      status: overrides.status ?? {
        enabled: true, state: "running", history_from: 10, verified_from: 11, verified_through: 12,
        window_from: 10, window_to: 20, next_page: "3", last_success_at: 12, last_event_at: 12,
        lag_seconds: 9, error_code: "", reauth_required: false, verification: "stabilized_api_scan",
      },
    },
  };
}
function mount(request) {
  const document = { createElement(tag) { return new Element(tag, document); } };
  const element = document.createElement("div");
  const panel = mountActivityPanel(element, request);
  const flush = async (n = 8) => { for (let i = 0; i < n; i++) await new Promise((resolve) => setImmediate(resolve)); };
  const find = (text) => element.querySelectorAll("button").find((button) => button.textContent === text);
  return { element, panel, flush, find };
}

test("mounted journal sends compact group categories without actor_id", async () => {
  const paths = [];
  const request = async ({ path }) => {
    paths.push(path);
    assert.equal(path.includes("actor_id"), false);
    return { status: 200, body: panelBody() };
  };
  const { element, flush, find } = mount(request);
  await flush();
  assert.match(paths[0], /\/panel\?/);
  assert.match(paths[0], /compact=true/);
  const group = element.querySelector("select.amocrm-activity-v0__groups");
  for (const option of group.children) option.selected = option.value === "3";
  for (const box of element.querySelectorAll("input")) if (box.type === "checkbox" && box.value === "tasks") box.checked = true;
  find("Показать").click();
  await flush();
  const last = paths.at(-1);
  assert.match(last, /group_id=3/);
  assert.match(last, /categories=tasks/);
  assert.match(last, /compact=true/);
  assert.equal(last.includes("include_unknown_authors"), false);
  assert.equal(last.includes("actor_id"), false);
});
test("stale panel generation is ignored", async () => {
  let releaseSlow, n = 0;
  const slow = new Promise((resolve) => { releaseSlow = resolve; });
  const request = async ({ path }) => {
    if (!path.includes("/panel?")) assert.fail(path);
    n++;
    if (n === 1) {
      await slow;
      return { status: 200, body: panelBody({ users: [{ id: 1, name: "Slow", group_id: 1, group_name: "G" }] }) };
    }
    return { status: 200, body: panelBody({ users: [{ id: 2, name: "Fast", group_id: 1, group_name: "G" }] }) };
  };
  const { element, flush, find } = mount(request);
  await flush();
  find("Показать").click();
  await flush();
  assert.match(element.textContent, /Fast/);
  assert.doesNotMatch(element.textContent, /Slow/);
  releaseSlow();
  await flush();
  assert.match(element.textContent, /Fast/);
  assert.doesNotMatch(element.textContent, /Slow/);
});
test("open event is preserved across a second panel payload", async () => {
  const event = {
    id: "e1", created_at: 100, created_by: 7, type: "task_completed", entity_id: 15, entity_type: "leads",
    value_before: { text: "old" }, value_after: { text: "new" },
    view: { title: "Задача завершена", author_label: "Alpha", entity_label: "Сделка #15", detail_state: "available", details: [{ key: "result", label: "Результат", text: "done", before: "old", after: "new" }] },
  };
  let reads = 0;
  const request = async ({ path }) => {
    if (!path.includes("/panel?")) assert.fail(path);
    reads++;
    return { status: 200, body: panelBody({ events: [event], payloads_omitted: false }) };
  };
  const { element, panel, flush, find } = mount(request);
  await flush();
  find("Подробнее").click();
  await flush();
  assert.match(element.textContent, /Результат/);
  await panel.refresh();
  await flush();
  assert.equal(reads, 2);
  assert.match(element.textContent, /Результат/);
  assert.match(element.textContent, /Задача завершена/);
});
test("compact page fetches event detail once on expand", async () => {
  const event = {
    id: "evt.1", created_at: 100, created_by: 7, type: "incoming_call", entity_id: 15, entity_type: "leads",
    value_before: null, value_after: null,
    view: { title: "Входящий звонок", author_label: "Alpha", entity_label: "Сделка #15", detail_state: "omitted" },
  };
  const detail = {
    ...event, value_before: [], value_after: { duration: 12 },
    view: {
      ...event.view, detail_state: "available",
      details: [
        { key: "duration", label: "Длительность", after: 12, source: "notes_api" },
        { key: "name", label: "Имя", text: "Lead", source: "catalog", current: true },
      ],
    },
  };
  let details = 0;
  const request = async ({ path, method }) => {
    if (path.includes("/panel?")) return { status: 200, body: panelBody({ events: [event], payloads_omitted: true }) };
    if (path === "/api/v1/widget/activity/events/evt.1" && method === "GET") {
      assert.equal(path.includes("?"), false);
      details++;
      return { status: 200, body: detail };
    }
    assert.fail(`${method} ${path}`);
  };
  const { element, flush, find } = mount(request);
  await flush();
  const link = element.querySelector("a");
  assert.equal(link.href, "/leads/detail/15");
  assert.equal(link.rel, "noopener noreferrer");
  assert.equal(link.target, "_blank");
  find("Подробнее").click();
  await flush();
  assert.equal(details, 1);
  assert.match(element.textContent, /Длительность/);
  assert.match(element.textContent, /Текущее значение справочника/);
  find("Скрыть").click();
  await flush();
  find("Подробнее").click();
  await flush();
  assert.equal(details, 1);
});
test("unavailable detail is shown without a fetch", async () => {
  const event = {
    id: "e-unavail", created_at: 100, created_by: 7, type: "mystery", entity_id: 4, entity_type: "task",
    view: { title: "Событие mystery", author_label: "Alpha", entity_label: "Задача #4", detail_state: "unavailable" },
  };
  const request = async ({ path }) => {
    if (path.includes("/panel?")) return { status: 200, body: panelBody({ events: [event] }) };
    assert.fail("should not fetch " + path);
  };
  const { element, flush, find } = mount(request);
  await flush();
  assert.equal(element.querySelector("a"), null);
  find("Подробнее").click();
  await flush();
  assert.match(element.textContent, /Подробности недоступны/);
});
test("missing event detail is unavailable", async () => {
  const event = {
    id: "missing", created_at: 100, created_by: 7, type: "common_note_added", entity_id: 8, entity_type: "contacts",
    view: { title: "Примечание добавлено", author_label: "Alpha", entity_label: "Контакт #8", detail_state: "omitted" },
  };
  const request = async ({ path }) => {
    if (path.includes("/panel?")) return { status: 200, body: panelBody({ events: [event] }) };
    if (path === "/api/v1/widget/activity/events/missing") return { status: 404, body: { error: { code: "not_found" } } };
    assert.fail(path);
  };
  const { element, flush, find } = mount(request);
  await flush();
  find("Подробнее").click();
  await flush();
  assert.match(element.textContent, /Событие недоступно/);
});
test("summary keeps zero rows for directory users without events", async () => {
  const request = async ({ path }) => {
    if (!path.includes("/panel?")) assert.fail(path);
    return { status: 200, body: panelBody({
      coverage: "verified",
      summaries: [{ user_id: 7, unique_events: 3, first_event_at: 10, last_event_at: 11, task_completed_events: 2, unique_completed_tasks: 1, category_counts: [{ category: "tasks", count: 3 }] }],
    }) };
  };
  const { element, flush } = mount(request);
  await flush();
  assert.match(element.textContent, /Alpha/);
  assert.match(element.textContent, /Beta/);
  assert.match(element.textContent, /Нет событий за выбранный период/);
  assert.match(element.textContent, /Задачи завершены: 0 \(0 уник\.\)/);
  assert.match(element.textContent, /Задачи: 3/);
});
test("short status omits collector diagnostics", async () => {
  const request = async ({ path }) => ({ status: 200, body: panelBody({ empty_reason: "no_events", coverage: "verified" }) });
  const { element, flush } = mount(request);
  await flush();
  const status = element.querySelector(".amocrm-activity-v0__status");
  const diagnostics = element.querySelector(".amocrm-activity-v0__diagnostics");
  assert.match(status.textContent, /Выбранный период проверен/);
  assert.match(status.textContent, /Сбор актуален/);
  assert.match(status.textContent, /Нет событий за выбранный период/);
  assert.doesNotMatch(status.textContent, /Отставание|Текущее окно|stabilized_api_scan|страница 3/);
  assert.match(diagnostics.textContent, /Отставание: 9 сек/);
  assert.match(diagnostics.textContent, /Текущее окно/);
  assert.match(diagnostics.textContent, /stabilized_api_scan|повторном согласованном чтении/);
});

test("calendar presets and refresh keep inclusive 23:59:50", async (t) => {
  const paths = [];
  const h = mount(async ({path}) => { paths.push(path); return {status: 200, body: panelBody()}; });
  t.after(() => h.panel.destroy());
  await h.flush();
  for (const [label, preset] of [["Вчера", "yesterday"], ["Сегодня", "today"]]) {
    h.find(label).click(); await h.flush();
    const expected = calendarDayRange(preset, "Europe/Moscow");
    assert.match(formatDateTimeLocal(expected.to, "Europe/Moscow"), /23:59:50$/);
    for (let i = 0; i < 2; i++) {
      const query = new URL(paths.at(-1), "https://test.invalid").searchParams;
      assert.equal(Number(query.get("to")), expected.to);
      assert.equal(Number(query.get("from")), expected.from);
      if (!i) await h.panel.refresh();
    }
  }
});

test("filtered directory preserves employees and departments through drill and back", async (t) => {
  const all = [{id:7,name:"Alpha",group_id:3,group_name:"Sales"},{id:9,name:"Beta",group_id:4,group_name:"Support"}];
  const paths = [];
  const h = mount(async ({path}) => {
    paths.push(path);
    const q = new URL(path, "https://test.invalid").searchParams;
    const ids = q.get("user_ids")?.split(",");
    return {status:200,body:panelBody({users:ids ? all.filter(u=>ids.includes(String(u.id))) : all})};
  });
  t.after(() => h.panel.destroy());
  await h.flush();
  for (const option of h.element.querySelector("select.amocrm-activity-v0__users").children) option.selected = true;
  h.find("Показать").click(); await h.flush();
  h.find("Alpha").click(); await h.flush();
  assert.equal(h.element.querySelector("select.amocrm-activity-v0__users").children.length, 2);
  assert.equal(h.element.querySelector("select.amocrm-activity-v0__groups").children.length, 3);
  h.find("Все выбранные сотрудники").click(); await h.flush();
  assert.equal(new URL(paths.at(-1), "https://test.invalid").searchParams.get("user_ids"), "7,9");
});

function compactFixture() {
  return {id:"e1",created_at:100,created_by:7,type:"common_note_added",entity_type:"lead",entity_id:31,view:{detail_state:"omitted"}};
}
function enrichedFixture(text, state = "available") {
  return {...compactFixture(),value_after:[{text}],view:{detail_state:"available",enrichment_state:state,details:[{text}]}};
}
test("transient detail failure retries on reopening without remount", async (t) => {
  let reads = 0;
  const h = mount(async ({path}) => path.includes("/events/")
    ? (++reads === 1 ? {status:503,body:{error:{code:"unavailable"}}} : {status:200,body:enrichedFixture("Recovered")})
    : {status:200,body:panelBody({events:[compactFixture()]})});
  t.after(() => h.panel.destroy());
  await h.flush();h.find("Подробнее").click();await h.flush();
  h.find("Скрыть").click();h.find("Подробнее").click();await h.flush();
  assert.equal(reads,2);assert.match(h.element.textContent,/Recovered/);
});
test("refresh reloads pending enrichment and preserves the open event", async (t) => {
  let reads = 0;
  const h = mount(async ({path}) => path.includes("/events/")
    ? {status:200,body:++reads===1 ? enrichedFixture("Pending", "pending") : enrichedFixture("Ready")}
    : {status:200,body:panelBody({events:[compactFixture()]})});
  t.after(() => h.panel.destroy());
  await h.flush();h.find("Подробнее").click();await h.flush();
  assert.match(h.element.textContent,/Pending/);
  await h.panel.refresh();await h.flush();
  assert.equal(reads,2);assert.match(h.element.textContent,/Ready/);assert.ok(h.find("Скрыть"));
  h.find("Скрыть").click();h.find("Подробнее").click();await h.flush();assert.equal(reads,2);
});
test("a late detail response cannot replace the refreshed card", async (t) => {
  let release, reads = 0;
  const h = mount(async ({path}) => {
    if (!path.includes("/events/")) return {status:200,body:panelBody({events:[compactFixture()]})};
    if (++reads===1) {await new Promise(resolve=>{release=resolve;});return {status:200,body:enrichedFixture("Old response")};}
    return {status:200,body:enrichedFixture("New response")};
  });
  t.after(() => h.panel.destroy());
  await h.flush();h.find("Подробнее").click();await h.flush();await h.panel.refresh();
  release();await h.flush();
  assert.equal(reads,2);assert.match(h.element.textContent,/New response/);assert.doesNotMatch(h.element.textContent,/Old response/);
});

test("audit: unknown coverage retains known counts in employee rows", async () => {
  for (const coverage of ["unknown", "partial"]) {
    assert.match(observationLabel({ unique_events: 7 }, coverage), /^7 зарегистрировано/);
    const { element, flush, panel } = mount(async () => ({ status: 200, body: panelBody({ coverage, summaries: [{user_id: 7, unique_events: 7}, {user_id: 9, unique_events: 0}] }) }));
    await flush();
    assert.match(element.querySelectorAll("tr").find(row => row.textContent.includes("Alpha")).textContent, /7 зарегистрировано/);
    assert.match(element.querySelectorAll("tr").find(row => row.textContent.includes("Beta")).textContent, /0 зарегистрировано/);
    panel.destroy();
  }
  for (const count of [undefined, null, -1, NaN, "7"]) assert.equal(observationLabel({unique_events: count}, "unknown"), "Нет проверенных данных");
});

test("audit: local time rejects DST gaps and chooses the earlier repeated hour", () => {
  const tz = "Europe/Oslo";
  assert.throws(() => parseDateTimeLocal("2026-03-29T02:30:00", tz));
  assert.throws(() => parseDateTimeLocal("2026-02-30T10:00:00", tz));
  assert.throws(() => parseDateTimeLocal("2026-09-08T10:00:00", "unknown"));
  const ordinary = "2026-03-29T03:30:00";
  assert.equal(formatDateTimeLocal(parseDateTimeLocal(ordinary,tz),tz),ordinary);
  assert.equal(parseDateTimeLocal("2026-10-25T02:30:00",tz),Date.parse("2026-10-25T00:30:00Z")/1000);
  for (const [from,to] of [["2026-03-29T02:30:00","2026-03-29T04:00:00"],["2026-03-29T01:00:00","2026-03-29T02:30:00"]]) assert.throws(()=>panelQuery(from,to,"",{timezone:tz}));
});

// When run by Activity CI these are actual public HTTP cards, collected via
// PostgreSQL and mTLS, rather than hand-authored presenter-shaped fixtures.
if (process.env.ACTIVITY_UI_CARDS_FIXTURE) test("audit: public HTTP cards render enum, message and removed values", async () => {
 const cards=JSON.parse(await readFile(process.env.ACTIVITY_UI_CARDS_FIXTURE,"utf8"));
 for (const [id,wants] of Object.entries({enum:["Новое имя (#1)","Большой (#1)","#2 (название варианта недоступно)","Текущее значение справочника"],incoming:["Входящее сообщение","msg-in","Текст отсутствует","32"],outgoing:["Исходящее сообщение","Текст из payload","test-channel"],removed:["Значение: null","Новое имя (#1)"]})) {
  const {element,flush,find,panel}=mount(async({path})=>({status:200,body:path.includes("/events/")?cards[id]:panelBody({events:[{id,type:cards[id].type,view:{detail_state:"omitted"}}]})}));
  await flush();find("Подробнее").click();await flush();
  for(const want of wants)assert.ok(element.textContent.includes(want), `${id}: missing ${want}`);
  panel.destroy();
 }
});

test("audit: manual DST gaps on either bound never issue a panel request", async () => {
 for(const badBound of [0,1]) {
  let calls=0;
  const {element,flush,find,panel}=mount(async()=>{calls++;return {status:200,body:panelBody({timezone:"Europe/Oslo"})}});
  await flush();
  const bounds=element.querySelectorAll("input").filter(input=>input.type==="datetime-local");
  bounds[0].value="2026-03-29T01:30:00";bounds[1].value="2026-03-29T03:30:00";
  bounds[badBound].value="2026-03-29T02:30:00";
  find("Показать").click();await flush();
  assert.equal(calls,1);assert.match(element.textContent,/время не существует/);
  panel.destroy();
 }
});
