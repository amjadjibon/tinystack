"use strict";

// The dashboard only reads TinyStack's own JSON endpoints. Every value is
// written with textContent: request bodies and resource names come from
// clients and must never be parsed as HTML.

const time = (v) => new Date(v).toLocaleTimeString();
const statusClass = (ok) => (ok ? "ok" : "bad");

// parseMetrics turns the Prometheus text format into one row per series.
function parseMetrics(text) {
  const series = new Map();
  for (const line of text.split("\n")) {
    const m = line.match(/^(tinystack_\w+)\{service="([^"]*)",operation="([^"]*)",status="(\d+)"\} (\S+)$/);
    if (!m) continue;
    const key = m[2] + " " + m[3] + " " + m[4];
    const row = series.get(key) || { service: m[2], operation: m[3], status: Number(m[4]), count: 0, seconds: 0 };
    if (m[1] === "tinystack_requests_total") row.count = Number(m[5]);
    if (m[1] === "tinystack_request_duration_seconds_sum") row.seconds = Number(m[5]);
    series.set(key, row);
  }
  return [...series.values()];
}

const tabs = {
  requests: {
    url: "/_tinystack/requests",
    clearable: true,
    rows: (data) => data.requests,
    columns: [
      ["Time", (r) => time(r.time)],
      ["Service", (r) => r.service],
      ["Operation", (r) => r.operation || r.method + " " + r.path],
      ["Status", (r) => r.status, (r) => statusClass(r.status < 400)],
      ["Error", (r) => r.error_code, () => "bad"],
      ["Account", (r) => r.account],
      ["Region", (r) => r.region],
      ["ms", (r) => r.duration_ms.toFixed(1), () => "num"],
    ],
    detail: (r) => [
      ["Request", `${r.method} ${r.path}\nrequest id ${r.request_id}\nprotocol ${r.protocol || "-"}`],
      ["Error", r.error_code ? `${r.error_code}: ${r.error_message}` : ""],
      [`Request body (${r.request_size} bytes)`, r.request_body],
      [`Response body (${r.response_size} bytes)`, r.response_body],
    ],
  },
  deliveries: {
    url: "/_tinystack/deliveries",
    clearable: true,
    rows: (data) => data.deliveries,
    columns: [
      ["Time", (r) => time(r.time)],
      ["Via", (r) => r.via],
      ["Source", (r) => r.source],
      ["Target", (r) => r.target],
      ["Status", (r) => r.status, (r) => statusClass(r.status === "delivered")],
      ["Error", (r) => r.error, () => "bad"],
    ],
    detail: (r) => [
      ["Route", `${r.via}\n${r.source || "(direct invocation)"}\n→ ${r.target}`],
      ["Error", r.error],
      [`Payload (${r.size} bytes)`, r.payload],
    ],
  },
  resources: {
    url: "/_tinystack/resources",
    rows: (data) => data.resources,
    columns: [
      ["Service", (r) => r.service],
      ["Type", (r) => r.type],
      ["Name", (r) => r.name],
      ["Account", (r) => r.account],
      ["Region", (r) => r.region],
      ["ARN", (r) => r.arn],
    ],
    detail: (r) => [
      ["ARN", r.arn],
      ["Tags", r.tags ? JSON.stringify(r.tags, null, 2) : ""],
      ["Details", JSON.stringify(r.details, null, 2)],
    ],
  },
  metrics: {
    url: "/_tinystack/metrics",
    text: true,
    rows: parseMetrics,
    columns: [
      ["Service", (r) => r.service],
      ["Operation", (r) => r.operation || "(unknown)"],
      ["Status", (r) => r.status, (r) => statusClass(r.status < 400)],
      ["Count", (r) => r.count, () => "num"],
      ["Avg ms", (r) => ((r.seconds / r.count) * 1000).toFixed(1), () => "num"],
    ],
    detail: (r) => [["Series", JSON.stringify(r, null, 2)]],
  },
};

const $ = (id) => document.getElementById(id);
let current = "requests";
let rows = [];

// pretty formats JSON bodies for reading and leaves anything else as is.
function pretty(text) {
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return text;
  }
}

function cell(tag, text, cls) {
  const el = document.createElement(tag);
  el.textContent = text ?? "";
  if (cls) el.className = cls;
  return el;
}

function render() {
  const tab = tabs[current];
  const filter = $("filter").value.toLowerCase();
  const head = document.createElement("tr");
  for (const [name] of tab.columns) head.append(cell("th", name));
  $("head").replaceChildren(head);

  const body = [];
  for (const row of rows) {
    const values = tab.columns.map(([, value]) => String(value(row) ?? ""));
    if (filter && !values.join(" ").toLowerCase().includes(filter)) continue;
    const tr = document.createElement("tr");
    tab.columns.forEach(([, , cls], i) => tr.append(cell("td", values[i], values[i] && cls ? cls(row) : "")));
    tr.addEventListener("click", () => show(tab, row));
    body.push(tr);
  }
  $("rows").replaceChildren(...body);
  $("empty").hidden = body.length > 0;
  $("clear").hidden = !tab.clearable;
}

function show(tab, row) {
  const parts = [];
  for (const [title, text] of tab.detail(row)) {
    if (!text) continue;
    parts.push(cell("h2", title), cell("pre", pretty(text)));
  }
  $("detail-body").replaceChildren(...parts);
  $("detail").hidden = false;
}

async function load() {
  const tab = tabs[current];
  const response = await fetch(tab.url);
  if (!response.ok) return;
  rows = tab.rows(tab.text ? await response.text() : await response.json()) || [];
  render();
}

async function services() {
  const response = await fetch("/_tinystack/health");
  if (response.ok) $("services").textContent = (await response.json()).services.join(" · ");
}

for (const button of document.querySelectorAll("nav [data-tab]")) {
  button.addEventListener("click", () => {
    document.querySelector("nav .active").classList.remove("active");
    button.classList.add("active");
    current = button.dataset.tab;
    rows = [];
    render();
    load();
  });
}

$("filter").addEventListener("input", render);
$("close").addEventListener("click", () => ($("detail").hidden = true));
$("clear").addEventListener("click", async () => {
  await fetch(tabs[current].url, { method: "DELETE" });
  load();
});

setInterval(() => $("live").checked && load(), 2000);
services();
load();
