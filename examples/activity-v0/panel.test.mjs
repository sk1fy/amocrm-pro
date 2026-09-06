import test from "node:test";
import assert from "node:assert/strict";
import { observationLabel, operationLabel, panelQuery } from "./panel.mjs";

test("unknown and incomplete observations never claim zero employee activity", () => {
  assert.equal(observationLabel({ unique_events: 0 }, "unknown"), "Нет проверенных данных");
  assert.equal(observationLabel({ unique_events: 0 }, "partial"), "Полных данных нет");
  assert.equal(observationLabel({ unique_events: 0 }, "stale"), "Полных данных нет");
  assert.equal(observationLabel({ unique_events: 0 }, "verified"), "0 зарегистрировано");
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
