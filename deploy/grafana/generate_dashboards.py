#!/usr/bin/env python3
"""Generates the four Grafana dashboards of the demo environment.

The dashboards share their styling, colours, variables, reload and fault annotations and links,
which is easy to get wrong by hand across four JSON files. Edit this script and run it from anywhere:

    python3 deploy/grafana/generate_dashboards.py

It rewrites deploy/grafana/dashboards/*.json, which Grafana picks up within seconds. It needs
only the Python standard library.
"""
import json, os

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "dashboards")
DS = {"type": "prometheus", "uid": "ege-balancer-prometheus"}

# Colours follow the profiles in deploy/docker-compose.yml: fast backends in greens and blues,
# medium in yellows, the slow one red, the one with large answers purple.
COLOURS = {
    "backend-1": "green", "backend-2": "semi-dark-green", "backend-3": "light-green",
    "backend-4": "blue", "backend-5": "light-blue", "backend-6": "semi-dark-blue",
    "backend-7": "yellow", "backend-8": "dark-yellow",
    "backend-9": "red", "backend-10": "purple",
}

def by_name(expr):
    """Adds a name label holding the backend without its port."""
    return f'label_replace({expr}, "name", "$1", "backend", "([^:]+).*")'

def target(expr, legend="", ref="A", instant=False, fmt=None):
    t = {"refId": ref, "datasource": DS, "expr": expr, "legendFormat": legend, "range": not instant, "instant": instant}
    if fmt:
        t["format"] = fmt
    return t

def colour_overrides():
    return [{"matcher": {"id": "byName", "options": name},
             "properties": [{"id": "color", "value": {"mode": "fixed", "fixedColor": colour}}]}
            for name, colour in COLOURS.items()]

def fixed(name, colour):
    return {"matcher": {"id": "byName", "options": name},
            "properties": [{"id": "color", "value": {"mode": "fixed", "fixedColor": colour}}]}

class Layout:
    def __init__(self):
        self.y, self.next_id = 0, 1
    def pid(self):
        self.next_id += 1
        return self.next_id - 1

def row(layout, title):
    p = {"type": "row", "title": title, "id": layout.pid(), "collapsed": False,
         "gridPos": {"x": 0, "y": layout.y, "w": 24, "h": 1}, "panels": []}
    layout.y += 1
    return p

def timeseries(layout, title, targets, x, w, h, unit=None, per_backend=False, stack=False,
               description="", overrides=None, min_zero=True, max_value=None, legend_right=False, no_value=None,
               log_scale=False):
    defaults = {
        "custom": {"drawStyle": "line", "lineWidth": 2, "fillOpacity": 12, "gradientMode": "opacity",
                   "showPoints": "never", "spanNulls": True, "lineInterpolation": "smooth",
                   "stacking": {"mode": "normal" if stack else "none", "group": "A"},
                   "axisSoftMin": 0 if min_zero and not log_scale else None},
        "color": {"mode": "palette-classic"},
    }
    # A logarithmic axis shows a handful of failures next to thousands of successes; it has no zero.
    if log_scale: defaults["custom"]["scaleDistribution"] = {"type": "log", "log": 10}
    if unit: defaults["unit"] = unit
    if min_zero and not log_scale: defaults["min"] = 0
    if max_value is not None: defaults["max"] = max_value
    if no_value: defaults["noValue"] = no_value
    legend = {"showLegend": True, "displayMode": "table" if legend_right else "list",
              "placement": "right" if legend_right else "bottom",
              "calcs": ["mean", "lastNotNull"] if legend_right else []}
    return {"type": "timeseries", "title": title, "description": description, "id": layout.pid(), "datasource": DS,
            "gridPos": {"x": x, "y": layout.y, "w": w, "h": h}, "targets": targets,
            "fieldConfig": {"defaults": defaults,
                            "overrides": (colour_overrides() if per_backend else []) + (overrides or [])},
            "options": {"legend": legend, "tooltip": {"mode": "multi", "sort": "desc"}}}

def stat(layout, title, targets, x, w, h, unit=None, steps=None, description="", decimals=None, no_value="0"):
    defaults = {"color": {"mode": "thresholds"}, "noValue": no_value,
                "thresholds": {"mode": "absolute", "steps": steps or [{"color": "green", "value": None}]}}
    if unit: defaults["unit"] = unit
    if decimals is not None: defaults["decimals"] = decimals
    return {"type": "stat", "title": title, "description": description, "id": layout.pid(), "datasource": DS,
            "gridPos": {"x": x, "y": layout.y, "w": w, "h": h}, "targets": targets,
            "fieldConfig": {"defaults": defaults, "overrides": []},
            "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                        "colorMode": "background", "graphMode": "area", "textMode": "value",
                        "justifyMode": "center", "orientation": "auto", "wideLayout": True}}

def bargauge(layout, title, targets, x, w, h, unit=None, max_value=None, description=""):
    defaults = {"color": {"mode": "fixed", "fixedColor": "text"}, "min": 0,
                "thresholds": {"mode": "absolute", "steps": [{"color": "green", "value": None}]}}
    if unit: defaults["unit"] = unit
    if max_value is not None: defaults["max"] = max_value
    return {"type": "bargauge", "title": title, "description": description, "id": layout.pid(), "datasource": DS,
            "gridPos": {"x": x, "y": layout.y, "w": w, "h": h}, "targets": targets,
            "fieldConfig": {"defaults": defaults, "overrides": colour_overrides()},
            "options": {"orientation": "horizontal", "displayMode": "gradient", "showUnfilled": True,
                        "valueMode": "color", "namePlacement": "left", "sizing": "auto", "minVizHeight": 16,
                        "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False}}}

def health_timeline(layout, x, w, h):
    return {"type": "state-timeline", "title": "Backend health",
            "description": "Green while health checking keeps a backend in the pool, red while it is out.",
            "id": layout.pid(), "datasource": DS, "gridPos": {"x": x, "y": layout.y, "w": w, "h": h},
            "targets": [target("sum by (name) (" + by_name("lb_backend_healthy{" + SEL + "}") + ")", "{{name}}")],
            "fieldConfig": {"defaults": {
                "color": {"mode": "thresholds"},
                "thresholds": {"mode": "absolute", "steps": [{"color": "red", "value": None}, {"color": "green", "value": 1}]},
                "mappings": [{"type": "value", "options": {"0": {"text": "unhealthy", "color": "red", "index": 0},
                                                             "1": {"text": "healthy", "color": "green", "index": 1}}}],
                "custom": {"fillOpacity": 80, "lineWidth": 0}}, "overrides": []},
            "options": {"showValue": "never", "mergeValues": True, "rowHeight": 0.8, "alignValue": "left",
                        "legend": {"showLegend": False}, "tooltip": {"mode": "single"}}}

def variables(with_backend=True):
    window = {"name": "window", "label": "Window", "type": "custom", "query": "1m,5m,15m",
              "current": {"text": "1m", "value": "1m", "selected": True},
              "options": [{"text": v, "value": v, "selected": v == "1m"} for v in ("1m", "5m", "15m")],
              "hide": 0, "multi": False, "includeAll": False,
              "description": "How far back each rate and average looks. Longer windows smooth out noise."}
    backend = {"name": "backend", "label": "Backend", "type": "query", "datasource": DS,
               "definition": "label_values(lb_backend_healthy, backend)",
               "query": {"qryType": 1, "query": "label_values(lb_backend_healthy, backend)", "refId": "backend"},
               "multi": True, "includeAll": True, "allValue": ".*", "refresh": 2, "sort": 7, "hide": 0,
               "current": {"text": ["All"], "value": ["$__all"], "selected": True}, "options": []}
    return {"list": ([backend] if with_backend else []) + [window]}

ANNOTATIONS = {"list": [
    {"builtIn": 1, "datasource": {"type": "grafana", "uid": "-- Grafana --"}, "enable": True, "hide": True,
     "iconColor": "rgba(0, 211, 255, 1)", "name": "Annotations & Alerts", "type": "dashboard"},
    {"name": "Configuration reloaded", "datasource": DS, "enable": True, "iconColor": "blue",
     "expr": 'changes(lb_config_reloads_total{result="applied"}[20s]) > 0', "step": "10s",
     "titleFormat": "configuration reloaded", "textFormat": "SIGHUP applied", "useValueForTime": False},
    {"name": "Reload rejected", "datasource": DS, "enable": True, "iconColor": "red",
     "expr": 'changes(lb_config_reloads_total{result="rejected"}[20s]) > 0', "step": "10s",
     "titleFormat": "reload rejected", "textFormat": "the file was invalid; the running configuration was kept",
     "useValueForTime": False},
    # A region for as long as a fault injected through a mock's admin port is in force.
    {"name": "Fault in force", "datasource": DS, "enable": True, "iconColor": "orange",
     "expr": 'label_replace(mock_fault_in_force == 1, "name", "$1", "backend", "([^:]+).*")', "step": "5s",
     "titleFormat": "{{mode}} on {{name}}", "textFormat": "injected through the admin port",
     "tagKeys": "name,mode", "useValueForTime": False},
]}

LINKS = [{"type": "dashboards", "tags": ["ege-balancer"], "asDropdown": False, "includeVars": True,
          "keepTime": True, "title": "Ege-Balancer", "icon": "external link", "targetBlank": False}]

def dashboard(uid, title, description, panels, with_backend=True):
    return {"uid": uid, "title": title, "description": description, "tags": ["ege-balancer"],
            "timezone": "browser", "schemaVersion": 39, "refresh": "5s", "graphTooltip": 1,
            "time": {"from": "now-15m", "to": "now"}, "editable": True, "links": LINKS,
            "annotations": ANNOTATIONS, "templating": variables(with_backend), "panels": panels}

SEL = 'backend=~"$backend"'
W = "$window"

def overview():
    L, P = Layout(), []
    P.append(row(L, "Right now"))
    requests = f"sum(rate(lb_requests_total[{W}]))"
    refused = f'(sum(rate(lb_rejected_requests_total{{reason!="no_healthy_backend"}}[{W}])) or vector(0))'
    P += [
        stat(L, "Requests", [target(requests + " or vector(0)")], 0, 5, 4, "reqps", decimals=1,
             description="Requests the backends answered, per second."),
        stat(L, "Served without error", [target(f'sum(rate(lb_requests_total{{status!~"5.."}}[{W}])) / ({requests} + {refused})')],
             5, 5, 4, "percentunit", decimals=2, no_value="—",
             steps=[{"color": "red", "value": None}, {"color": "orange", "value": 0.95}, {"color": "green", "value": 0.99}],
             description="Share of requests answered without a 5xx and not refused by the balancer."),
        stat(L, "p99 latency", [target(f"histogram_quantile(0.99, sum by (le) (rate(lb_request_duration_seconds_bucket[{W}])))")],
             10, 5, 4, "s", no_value="—",
             steps=[{"color": "green", "value": None}, {"color": "orange", "value": 0.25}, {"color": "red", "value": 1}],
             description="One request in a hundred took longer than this."),
        stat(L, "Healthy backends", [target("sum(lb_backend_healthy)")], 15, 5, 4, decimals=0,
             steps=[{"color": "red", "value": None}, {"color": "orange", "value": 1}, {"color": "green", "value": 10}],
             description="Backends health checking keeps in the pool, out of ten."),
        stat(L, "Reloads applied", [target('sum(lb_config_reloads_total{result="applied"}) or vector(0)')], 20, 4, 4,
             decimals=0, steps=[{"color": "blue", "value": None}],
             description="Configurations applied through SIGHUP since the balancer started."),
    ]
    L.y += 4
    P.append(row(L, "Traffic"))
    P += [
        timeseries(L, "Request rate by backend",
                   [target(f"sum by (name) ({by_name(f'rate(lb_requests_total{{{SEL}}}[{W}])')})", "{{name}}")],
                   0, 14, 9, "reqps", per_backend=True, legend_right=True,
                   description="Requests each backend answered, per second. Each backend keeps its profile's colour."),
        bargauge(L, "Share of traffic, last 5 minutes",
                 [target(f"sort_desc(sum by (name) ({by_name(f'increase(lb_requests_total{{{SEL}}}[5m])')}) / scalar(sum(increase(lb_requests_total[5m]))))",
                         "{{name}}", instant=True)],
                 14, 10, 9, "percentunit", max_value=0.2,
                 description="With round robin every backend takes a tenth. Least connections moves traffic off slower backends."),
    ]
    L.y += 9
    P.append(row(L, "Latency and health"))
    quantile = lambda q: f"histogram_quantile({q}, sum by (le) (rate(lb_request_duration_seconds_bucket[{W}])))"
    P += [
        timeseries(L, "Latency", [target(quantile(0.5), "p50", "A"), target(quantile(0.95), "p95", "B"),
                                  target(quantile(0.99), "p99", "C")],
                   0, 14, 9, "s", overrides=[fixed("p50", "green"), fixed("p95", "orange"), fixed("p99", "red")],
                   description="Time to serve a request at the balancer, across all backends."),
        health_timeline(L, 14, 10, 9),
    ]
    L.y += 9
    return dashboard("ege-balancer", "Ege-Balancer — Overview",
                     "The state of the balancer at a glance: traffic, latency and backend health.", P)

def backends():
    L, P = Layout(), []
    P.append(row(L, "Summary"))
    summary = {
        "type": "table", "title": "Backends", "id": L.pid(), "datasource": DS,
        "description": "One row per backend. Sort by any column.",
        "gridPos": {"x": 0, "y": L.y, "w": 24, "h": 8},
        "targets": [
            target(f"sum by (name) ({by_name(f'rate(lb_requests_total{{{SEL}}}[{W}])')})", ref="A", instant=True, fmt="table"),
            target(f"sum by (name) ({by_name(f'increase(lb_requests_total{{{SEL}}}[5m])')}) / scalar(sum(increase(lb_requests_total[5m])))", ref="B", instant=True, fmt="table"),
            target(f"histogram_quantile(0.95, sum by (le, name) ({by_name(f'rate(lb_request_duration_seconds_bucket{{{SEL}}}[{W}])')}))", ref="C", instant=True, fmt="table"),
            target(f"sum by (name) ({by_name(f'avg_over_time(lb_backend_active_connections{{{SEL}}}[{W}])')})", ref="D", instant=True, fmt="table"),
            target(f"sum by (name) ({by_name(f'lb_backend_healthy{{{SEL}}}')})", ref="E", instant=True, fmt="table"),
        ],
        "transformations": [
            {"id": "merge", "options": {}},
            {"id": "organize", "options": {"excludeByName": {"Time": True},
                                           "indexByName": {"name": 0, "Value #E": 1, "Value #A": 2, "Value #B": 3, "Value #C": 4, "Value #D": 5},
                                           "renameByName": {"name": "Backend", "Value #A": "Requests", "Value #B": "Share, 5m",
                                                            "Value #C": "p95 latency", "Value #D": "In flight", "Value #E": "Health"}}},
            {"id": "sortBy", "options": {"sort": [{"field": "p95 latency", "desc": True}]}},
        ],
        "fieldConfig": {"defaults": {"custom": {"align": "auto", "cellOptions": {"type": "auto"}}}, "overrides": [
            {"matcher": {"id": "byName", "options": "Requests"}, "properties": [{"id": "unit", "value": "reqps"}, {"id": "decimals", "value": 2}]},
            {"matcher": {"id": "byName", "options": "Share, 5m"}, "properties": [
                {"id": "unit", "value": "percentunit"}, {"id": "decimals", "value": 1}, {"id": "min", "value": 0}, {"id": "max", "value": 0.2},
                {"id": "custom.cellOptions", "value": {"type": "gauge", "mode": "gradient"}},
                {"id": "color", "value": {"mode": "continuous-BlYlRd"}}]},
            {"matcher": {"id": "byName", "options": "p95 latency"}, "properties": [
                {"id": "unit", "value": "s"}, {"id": "custom.cellOptions", "value": {"type": "color-text"}},
                {"id": "thresholds", "value": {"mode": "absolute", "steps": [{"color": "green", "value": None}, {"color": "orange", "value": 0.1}, {"color": "red", "value": 0.3}]}}]},
            {"matcher": {"id": "byName", "options": "In flight"}, "properties": [{"id": "decimals", "value": 2}]},
            {"matcher": {"id": "byName", "options": "Health"}, "properties": [
                {"id": "mappings", "value": [{"type": "value", "options": {"0": {"text": "unhealthy", "color": "red"}, "1": {"text": "healthy", "color": "green"}}}]},
                {"id": "custom.cellOptions", "value": {"type": "color-background", "mode": "basic"}}]},
        ]},
        "options": {"showHeader": True, "cellHeight": "sm", "footer": {"show": False}},
    }
    P.append(summary)
    L.y += 8
    P.append(row(L, "Where the traffic goes"))
    P += [
        timeseries(L, "Share of traffic",
                   [target(f"sum by (name) ({by_name(f'rate(lb_requests_total{{{SEL}}}[{W}])')}) / scalar(sum(rate(lb_requests_total[{W}])))", "{{name}}")],
                   0, 12, 9, "percentunit", per_backend=True, legend_right=True, max_value=0.25,
                   description="Each backend's share of all requests. A tenth each under round robin."),
        timeseries(L, "Requests in flight, averaged",
                   [target(f"sum by (name) ({by_name(f'avg_over_time(lb_backend_active_connections{{{SEL}}}[{W}])')})", "{{name}}")],
                   12, 12, 9, per_backend=True, legend_right=True,
                   description="Average number of requests each backend was serving over the window. The raw gauge is sampled every 5s and mostly reads 0 or 1; the average shows which backends are actually busy."),
    ]
    L.y += 9
    P.append(row(L, "Latency by backend"))
    P += [
        timeseries(L, "p95 latency by backend",
                   [target(f"histogram_quantile(0.95, sum by (le, name) ({by_name(f'rate(lb_request_duration_seconds_bucket{{{SEL}}}[{W}])')}))", "{{name}}")],
                   0, 12, 9, "s", per_backend=True, legend_right=True,
                   description="Nineteen requests in twenty to each backend were faster than this."),
        {"type": "heatmap", "title": "Latency distribution", "id": L.pid(), "datasource": DS,
         "description": "How many requests fell into each latency bucket over time. The selected backends only.",
         "gridPos": {"x": 12, "y": L.y, "w": 12, "h": 9},
         "targets": [target(f'sum by (le) (increase(lb_request_duration_seconds_bucket{{{SEL}}}[$__rate_interval]))', "{{le}}", fmt="heatmap")],
         "fieldConfig": {"defaults": {"custom": {"hideFrom": {"legend": False, "tooltip": False, "viz": False},
                                                 "scaleDistribution": {"type": "linear"}}}, "overrides": []},
         "options": {"calculate": False, "cellGap": 1, "yAxis": {"axisPlacement": "left", "unit": "s", "reverse": False},
                     "rowsFrame": {"layout": "le"}, "color": {"mode": "scheme", "scheme": "Oranges", "steps": 64,
                                                               "fill": "dark-orange", "exponent": 0.5, "scale": "exponential"},
                     "legend": {"show": True}, "tooltip": {"mode": "single", "yHistogram": False, "showColorScale": True},
                     "showValue": "never", "exemplars": {"color": "rgba(255,0,255,0.7)"}}},
    ]
    L.y += 9
    P.append(row(L, "Failures and health"))
    P += [
        timeseries(L, "Failed attempts by backend",
                   [target(f"sum by (name) ({by_name(f'rate(lb_backend_failures_total{{{SEL}}}[{W}])')})", "{{name}}")],
                   0, 12, 8, "reqps", per_backend=True, legend_right=True, no_value="No failed attempts",
                   description="Attempts a backend could not serve: no connection, no answer in time, a connection or an answer broken off, and 5xx when retry_on_5xx is set. The Faults dashboard shows which."),
        health_timeline(L, 12, 12, 8),
    ]
    L.y += 8
    return dashboard("ege-balancer-backends", "Ege-Balancer — Backends",
                     "How each backend is doing and how the traffic is shared between them.", P)

def resilience():
    L, P = Layout(), []
    P.append(row(L, "Right now"))
    requests = f"sum(rate(lb_requests_total[{W}]))"
    P += [
        stat(L, "Refused by the balancer", [target(f'sum(rate(lb_rejected_requests_total{{reason!="no_healthy_backend"}}[{W}])) or vector(0)')],
             0, 6, 4, "reqps", decimals=2, steps=[{"color": "green", "value": None}, {"color": "orange", "value": 0.01}],
             description="Requests the balancer answered itself: rate limited, badly framed, too large, no backend, retry budget."),
        stat(L, "5xx answers", [target(f'sum(rate(lb_requests_total{{status=~"5.."}}[{W}])) or vector(0)')],
             6, 6, 4, "reqps", decimals=2, steps=[{"color": "green", "value": None}, {"color": "red", "value": 0.01}],
             description="5xx answers that came from a backend."),
        stat(L, "Retries per request", [target(f"(sum(rate(lb_retries_total[{W}])) or vector(0)) / {requests}")],
             12, 6, 4, "percentunit", decimals=1, no_value="—",
             steps=[{"color": "green", "value": None}, {"color": "orange", "value": 0.05}, {"color": "red", "value": 0.2}],
             description="Retries sent for every hundred requests. The retry budget keeps this under its share when a pool fails."),
        stat(L, "Reloads rejected", [target('sum(lb_config_reloads_total{result="rejected"}) or vector(0)')],
             18, 6, 4, decimals=0, steps=[{"color": "green", "value": None}, {"color": "red", "value": 1}],
             description="SIGHUP reloads refused because the file was invalid; the balancer kept its running configuration."),
    ]
    L.y += 4
    P.append(row(L, "What clients receive"))
    P += [
        timeseries(L, "Answers by status class",
                   [target(f'sum by (class) (label_replace(rate(lb_requests_total[{W}]), "class", "${{1}}xx", "status", "(.).."))', "{{class}}")],
                   0, 12, 9, "reqps", stack=True,
                   overrides=[fixed("2xx", "green"), fixed("3xx", "blue"), fixed("4xx", "orange"), fixed("5xx", "red")],
                   description="Answers that came from a backend, stacked by status class."),
        timeseries(L, "Refused by the balancer, by reason",
                   [target(f"sum by (reason) (rate(lb_rejected_requests_total[{W}]))", "{{reason}}", "A"),
                    target(f"sum(rate(lb_rejected_requests_total[{W}])) or vector(0)", "total", "B")],
                   12, 12, 9, "reqps",
                   overrides=[fixed("total", "text"), fixed("rate_limited", "orange"), fixed("retry_budget_exhausted", "purple"),
                              fixed("no_backend_available", "red"), fixed("no_healthy_backend", "yellow"),
                              fixed("bad_framing", "blue"), fixed("body_too_large", "light-blue")],
                   description="Requests the balancer answered itself. The total stays on the graph at zero when nothing is refused. no_healthy_backend is counted but not refused: the request is still offered to the pool."),
    ]
    L.y += 9
    P.append(row(L, "Retries and failures"))
    P += [
        timeseries(L, "Retries against the budget",
                   [target(f"sum(rate(lb_retries_total[{W}])) or vector(0)", "retries sent", "A"),
                    target(f'sum(rate(lb_rejected_requests_total{{reason="retry_budget_exhausted"}}[{W}])) or vector(0)', "refused by the budget", "B")],
                   0, 12, 8, "reqps", overrides=[fixed("retries sent", "blue"), fixed("refused by the budget", "purple")],
                   description="Retries the budget allowed, and retries it refused. Refusals appear only while many requests fail at once."),
        timeseries(L, "Failed attempts",
                   [target(f"sum(rate(lb_backend_failures_total[{W}])) or vector(0)", "failed attempts", "A")],
                   12, 12, 8, "reqps", overrides=[fixed("failed attempts", "red")],
                   description="Attempts a backend could not serve, across the pool. Each one feeds passive health checking and, under circuit_breaker, the backend's circuit."),
    ]
    L.y += 8
    P.append(row(L, "Health and configuration"))
    P += [
        health_timeline(L, 0, 16, 8),
        stat(L, "Reloads", [target('sum by (result) (lb_config_reloads_total)', "{{result}}")], 16, 8, 8, decimals=0,
             steps=[{"color": "blue", "value": None}], no_value="none yet",
             description="Configuration reloads since the balancer started, applied and rejected."),
    ]
    L.y += 8
    return dashboard("ege-balancer-resilience", "Ege-Balancer — Resilience",
                     "What happens when things fail: refusals, 5xx, retries and the retry budget, health and reloads.", P,
                     with_backend=True)

# Why the balancer counted an attempt as failed, in the order of the exchange.
REASON_COLOURS = [fixed("connect", "red"), fixed("timeout", "purple"), fixed("reset", "orange"),
                  fixed("cut_off", "dark-red"), fixed("5xx", "yellow"), fixed("other", "text")]

# What a mock backend did with a request. Answered is left out of the graphs: it dwarfs the rest.
OUTCOME_COLOURS = [fixed("hang", "purple"), fixed("reset", "dark-red"), fixed("drip", "orange"),
                   fixed("error", "red"), fixed("overloaded", "yellow"), fixed("abandoned", "blue")]

# The faults an admin port can inject, with the colour each is shown in. The injected-faults timeline
# encodes the mode in force on a backend as its position in this list, one based; 0 is none.
FAULT_MODES = [("hang", "purple"), ("reset", "dark-red"), ("drip", "orange"), ("error", "red"),
               ("slow", "yellow"), ("freeze", "light-blue")]

def faults():
    L, P = Layout(), []
    # One series per backend, always present, so that the timeline has rows before any fault.
    fault_in_force = "max by (name) (" + " or ".join(
        by_name("mock_fault_in_force{" + SEL + ', mode="' + mode + '"}') + f" * {code}"
        for code, (mode, _) in enumerate(FAULT_MODES, start=1)) + ")"
    P.append(row(L, "Right now"))
    refusals = f'sum(rate(lb_rejected_requests_total{{reason=~"not_retryable|no_backend_available|retry_budget_exhausted"}}[{W}]))'
    P += [
        stat(L, "Faults in force", [target(f"sum(mock_fault_in_force{{{SEL}}}) or vector(0)")], 0, 5, 4, decimals=0,
             steps=[{"color": "green", "value": None}, {"color": "orange", "value": 1}],
             description="Faults injected through the mock backends' admin ports that have not yet expired or been cleared. Background rates set by a profile are not counted."),
        stat(L, "Failed attempts", [target(f"sum(rate(lb_backend_failures_total{{{SEL}}}[{W}])) or vector(0)")],
             5, 5, 4, "reqps", decimals=2, steps=[{"color": "green", "value": None}, {"color": "red", "value": 0.01}],
             description="Attempts the balancer counted against a backend, for any reason."),
        stat(L, "Retries", [target(f"sum(rate(lb_retries_total[{W}])) or vector(0)")], 10, 5, 4, "reqps", decimals=2,
             steps=[{"color": "blue", "value": None}],
             description="Requests the balancer sent to another backend after a failed attempt."),
        stat(L, "Answers cut off", [target(f'sum(rate(lb_backend_failures_total{{{SEL}, reason="cut_off"}}[{W}])) or vector(0)')],
             15, 5, 4, "reqps", decimals=2, steps=[{"color": "green", "value": None}, {"color": "red", "value": 0.01}],
             description="Answers a backend broke off part way. The client's connection is closed before the whole answer arrives, often before any of it when the part sent fits in the balancer's buffer. Nothing can retry an answer already on its way."),
        stat(L, "Errors reaching clients",
             [target(f'(sum(rate(lb_requests_total{{status=~"5.."}}[{W}])) or vector(0)) + ({refusals} or vector(0))')],
             20, 4, 4, "reqps", decimals=2, steps=[{"color": "green", "value": None}, {"color": "red", "value": 0.01}],
             description="5xx answers from the backends, and 503s from the balancer when no backend served the request, a request could not safely be retried, or the retry budget was spent."),
    ]
    L.y += 4
    P.append(row(L, "Faults in force"))
    P += [
        {"type": "state-timeline", "title": "Injected faults",
         "description": "The fault injected into each mock backend, from the demo console's 8 or by hand, for as long as it was in force. A backend with two at once shows the later one in this list: hang, reset, drip, error, slow, freeze.",
         "id": L.pid(), "datasource": DS, "gridPos": {"x": 0, "y": L.y, "w": 18, "h": 8},
         "targets": [target(fault_in_force, "{{name}}")],
         # A fixed colour rather than thresholds: under thresholds the timeline groups values by
         # threshold range and ignores the mappings, which are what name each fault.
         "fieldConfig": {"defaults": {
             "color": {"mode": "fixed", "fixedColor": "transparent"},
             "mappings": [{"type": "value", "options": {
                 str(code): {"text": "" if code == 0 else FAULT_MODES[code - 1][0], "index": code,
                             "color": "transparent" if code == 0 else FAULT_MODES[code - 1][1]}
                 for code in range(len(FAULT_MODES) + 1)}}],
             "custom": {"fillOpacity": 80, "lineWidth": 0}}, "overrides": []},
         "options": {"showValue": "auto", "mergeValues": True, "rowHeight": 0.8, "alignValue": "left",
                     "legend": {"showLegend": False}, "tooltip": {"mode": "single"}}},
        stat(L, "Mock backends reachable", [target(f'sum(up{{job="mocks", {SEL}}})')], 18, 6, 7, decimals=0, no_value="—",
             steps=[{"color": "red", "value": None}, {"color": "orange", "value": 1}, {"color": "green", "value": 10}],
             description="Mock backends whose admin port Prometheus can reach. A frozen backend is still reachable; a stopped container is not."),
    ]
    L.y += 7
    P.append(row(L, "What the backends did"))
    not_answered = f'rate(mock_requests_total{{{SEL}, outcome!="answered"}}[{W}])'
    P += [
        timeseries(L, "Requests not answered normally, by outcome",
                   [target(f"sum by (outcome) ({not_answered})", "{{outcome}}")],
                   0, 12, 8, "reqps", overrides=OUTCOME_COLOURS, no_value="Every request answered normally",
                   description="What the mock backends did instead of answering normally: hung, dropped the connection part way (reset), dripped the answer out, answered 500 (error), refused with a full queue (overloaded). Abandoned requests are the ones the balancer gave up on first, at its response timeout or because its client left."),
        timeseries(L, "Requests not answered normally, by backend",
                   [target(f"sum by (name) ({by_name(not_answered)})", "{{name}}")],
                   12, 12, 8, "reqps", per_backend=True, legend_right=True, no_value="Every request answered normally",
                   description="The same requests, by the backend that did it. Each backend keeps its profile's colour."),
    ]
    L.y += 8
    P += [
        timeseries(L, "Workers busy",
                   [target(f"sum by (name) ({by_name(f'avg_over_time(mock_busy_workers{{{SEL}}}[{W}])')}) / sum by (name) ({by_name(f'mock_capacity{{{SEL}}} > 0')})", "{{name}}")],
                   0, 8, 8, "percentunit", per_backend=True, max_value=1,
                   description="Share of each backend's workers serving a request, averaged over the window. Hanging requests hold theirs until the balancer gives up on them."),
        timeseries(L, "Queued for a worker",
                   [target(f"sum by (name) ({by_name(f'avg_over_time(mock_waiting{{{SEL}}}[{W}])')})", "{{name}}")],
                   8, 8, 8, per_backend=True,
                   description="Requests waiting for a worker, averaged over the window. Beyond its queue a backend refuses with 503."),
        timeseries(L, "Cold start",
                   [target(f"sum by (name) ({by_name(f'mock_cold_factor{{{SEL}}}')})", "{{name}}")],
                   16, 8, 8, per_backend=True, min_zero=False,
                   description="How many times slower than normal each backend is while it warms up after starting; 1 once warm. Only the realistic overlay starts backends cold."),
    ]
    L.y += 8
    P.append(row(L, "What the balancer saw"))
    failures = f"lb_backend_failures_total{{{SEL}}}"
    P += [
        timeseries(L, "Failed attempts by reason",
                   [target(f"sum by (reason) (rate({failures}[{W}]))", "{{reason}}")],
                   0, 12, 8, "reqps", overrides=REASON_COLOURS, no_value="No failed attempts",
                   description="Why the balancer counted an attempt against a backend: no connection (connect), no answer within the response timeout (timeout), the connection closed before the answer (reset), the answer broken off part way (cut_off), or a 5xx under retry_on_5xx. A client that gives up is not counted."),
        {"type": "table", "title": "Failed attempts, last 5 minutes", "id": L.pid(), "datasource": DS,
         "description": "Failed attempts by backend and reason over the last five minutes.",
         "gridPos": {"x": 12, "y": L.y, "w": 12, "h": 8},
         "targets": [target(f"round(sum by (name, reason) ({by_name(f'increase({failures}[5m])')}) > 0)", instant=True, fmt="table")],
         "transformations": [
             {"id": "groupingToMatrix", "options": {"columnField": "reason", "rowField": "name", "valueField": "Value", "emptyValue": "zero"}},
             {"id": "organize", "options": {"renameByName": {"name\\reason": "Backend"}}},
         ],
         "fieldConfig": {"defaults": {"noValue": "0", "custom": {"align": "auto", "cellOptions": {"type": "auto"}}}, "overrides": []},
         "options": {"showHeader": True, "cellHeight": "sm", "footer": {"show": False}}},
    ]
    L.y += 8
    P += [
        health_timeline(L, 0, 12, 8),
        timeseries(L, "Retries, and requests not retried",
                   [target(f"sum(rate(lb_retries_total[{W}])) or vector(0)", "retries sent", "A"),
                    target(f'sum(rate(lb_rejected_requests_total{{reason="not_retryable"}}[{W}])) or vector(0)', "not retryable", "B"),
                    target(f'sum(rate(lb_rejected_requests_total{{reason="retry_budget_exhausted"}}[{W}])) or vector(0)', "refused by the budget", "C")],
                   12, 12, 8, "reqps",
                   overrides=[fixed("retries sent", "blue"), fixed("not retryable", "red"), fixed("refused by the budget", "purple")],
                   description="Retries the balancer sent, and failed requests it did not retry: a POST or PATCH a backend may already have carried out, or a retry the budget refused."),
    ]
    L.y += 8
    P.append(row(L, "What the clients got"))
    quantile = lambda q: f"histogram_quantile({q}, sum by (le) (rate(lb_request_duration_seconds_bucket[{W}])))"
    P += [
        timeseries(L, "Answers to clients",
                   [target(f'sum by (class) (label_replace(rate(lb_requests_total[{W}]), "class", "${{1}}xx", "status", "(.).."))', "{{class}}", "A"),
                    target(refusals, "503 from the balancer", "B"),
                    target(f'sum(rate(lb_backend_failures_total{{reason="cut_off"}}[{W}]))', "cut off", "C")],
                   0, 12, 8, "reqps", log_scale=True,
                   overrides=[fixed("2xx", "green"), fixed("3xx", "blue"), fixed("4xx", "orange"), fixed("5xx", "red"),
                              fixed("503 from the balancer", "purple"), fixed("cut off", "dark-red")],
                   description="Everything clients received: answers from the backends by status class, the balancer's own 503s, and answers cut off part way. The axis is logarithmic, so that a few failures a second show next to the answers that succeeded. Clients that gave up are not here."),
        timeseries(L, "Latency", [target(quantile(0.5), "p50", "A"), target(quantile(0.95), "p95", "B"),
                                  target(quantile(0.99), "p99", "C")],
                   12, 12, 8, "s", overrides=[fixed("p50", "green"), fixed("p95", "orange"), fixed("p99", "red")],
                   description="Time the answering attempt took, measured at the balancer. A drip or a slow fault shows here rather than as a failure. Time spent on failed attempts before it is not included: a hang the balancer gave up on and retried elsewhere shows as a retry, not here."),
    ]
    L.y += 8
    P.append(row(L, "What the backends remember"))
    hits = f'rate(mock_cache_requests_total{{{SEL}, result="hit"}}[{W}])'
    lookups = f"rate(mock_cache_requests_total{{{SEL}}}[{W}])"
    P += [
        timeseries(L, "Cache hit rate by backend",
                   [target(f"sum by (name) ({by_name(hits)}) / sum by (name) ({by_name(lookups)})", "{{name}}")],
                   0, 12, 8, "percentunit", per_backend=True, legend_right=True, max_value=1,
                   no_value="No sessions: requests need an X-Session header, as loadgen -keys sends",
                   description="Share of requests naming a session that the backend remembered. A miss costs the backend more time. Only the realistic overlay gives backends a memory."),
        timeseries(L, "Sessions remembered",
                   [target(f"sum by (name) ({by_name(f'mock_cache_keys{{{SEL}}}')})", "{{name}}")],
                   12, 12, 8, per_backend=True, legend_right=True,
                   description="Sessions each backend remembers. A backend that restarts, or whose cache is cleared, starts again from none."),
    ]
    L.y += 8
    return dashboard("ege-balancer-faults", "Ege-Balancer — Faults",
                     "A fault followed from the backend to the client: what the mock backends did, what the balancer counted, and what clients received.", P)

for name, board in [("ege-balancer.json", overview()), ("ege-balancer-backends.json", backends()),
                    ("ege-balancer-resilience.json", resilience()), ("ege-balancer-faults.json", faults())]:
    with open(os.path.join(OUT, name), "w") as f:
        json.dump(board, f, indent=2, ensure_ascii=False)
        f.write("\n")
    print(name, len(board["panels"]), "panels")
