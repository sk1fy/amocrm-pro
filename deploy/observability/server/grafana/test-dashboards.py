"""Проверяем режим карточек и строим promtool-тесты из реальных запросов JSON."""

import json
from pathlib import Path


root = Path(__file__).resolve().parent / "dashboards"
dashboards = {}
for path in sorted(root.glob("*.json")):
    dashboard = json.loads(path.read_text())
    dashboards[dashboard["uid"]] = dashboard
    for panel in dashboard["panels"]:
        if panel["type"] not in ("stat", "gauge"):
            continue
        for target in panel.get("targets", []):
            assert target.get("instant") is True, (
                f"{path.name}/{panel['id']}: current-value card needs instant query"
            )
            assert target.get("range") is not True, (
                f"{path.name}/{panel['id']}: range query can retain historical state"
            )


def panel(uid, panel_id):
    return next(p for p in dashboards[uid]["panels"] if p["id"] == panel_id)


# Читаем выражения из дашбордов: тест не должен проходить при откате JSON.
up = panel("platform-overview", 1)
down = panel("platform-overview", 2)
status_down = panel("system-status", 1)
for current in (up, down, status_down):
    assert current["targets"][0].get("range") is False
    defaults = current["fieldConfig"]["defaults"]
    assert defaults["noValue"] == "Нет данных"
    assert any(
        mapping["type"] == "special"
        and mapping["options"].get("match") == "null"
        and mapping["options"]["result"].get("color") == "gray"
        for mapping in defaults["mappings"]
    ), "Отсутствие метрик должно отличаться от зелёного нуля"


def checks(time, up_count=None, down_count=None):
    return [
        {
            "expr": current["targets"][0]["expr"],
            "eval_time": time,
            "exp_samples": [] if value is None else [{"labels": "{}", "value": value}],
        }
        for current, value in ((up, up_count), (down, down_count), (status_down, down_count))
    ]


# 2 up → 1 down → 2 down → восстановление → stale.
# При recovery down=0, а не прошлый ненулевой результат. При stale/пустой
# Prometheus ни одна карточка не должна подменять отсутствие метрик нулём.
print(json.dumps({
    "rule_files": [],
    "evaluation_interval": "1m",
    "tests": [
        {
            "name": "scrape state transitions and stale samples",
            "interval": "1m",
            "input_series": [
                {"series": 'up{job="api", instance="api:8082"}', "values": "1 0 0 1 stale"},
                {"series": 'up{job="worker", instance="worker:8081"}', "values": "1 1 0 1 stale"},
            ],
            "promql_expr_test": (
                checks("0m", 2, 0) + checks("1m", 1, 1) + checks("2m", 0, 2)
                + checks("3m", 2, 0) + checks("4m")
            ),
        },
        {
            "name": "no scrape metrics is unknown",
            "interval": "1m",
            "input_series": [],
            "promql_expr_test": checks("0m"),
        },
        {
            "name": "samples older than lookback are unknown",
            "interval": "1m",
            "input_series": [
                {"series": 'up{job="api"}', "values": "1 _ _ _ _ _ _"},
            ],
            "promql_expr_test": checks("6m"),
        },
    ],
}, ensure_ascii=False, indent=2))
