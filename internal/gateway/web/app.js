"use strict";

// Everything shown here comes from workers and services, so it's only ever
// written as text. Nothing is assigned to innerHTML, and the page's content
// security policy would refuse to run markup even if it were.

const REFRESH_MS = 3000;
const PAGE_SIZE = 50;
const KEY_NAME = "fleet-dashboard-key";

const STATUS = {
  queued: { label: "Queued", tone: "neutral", icon: "circle" },
  assigned: { label: "Assigned", tone: "accent", icon: "play" },
  running: { label: "Running", tone: "accent", icon: "play" },
  lost: { label: "Lost", tone: "warning", icon: "alert" },
  succeeded: { label: "Succeeded", tone: "good", icon: "check" },
  failed: { label: "Failed", tone: "critical", icon: "cross" },
  timed_out: { label: "Timed out", tone: "serious", icon: "clock" },
  cancelled: { label: "Cancelled", tone: "neutral", icon: "slash" },
  online: { label: "Online", tone: "good", icon: "check" },
  offline: { label: "Offline", tone: "neutral", icon: "circle" },
};

const ICONS = {
  circle: "M8 2.5a5.5 5.5 0 1 0 0 11 5.5 5.5 0 0 0 0-11z",
  play: "M5 3l8 5-8 5z",
  alert: "M8 2l6.5 11.5h-13zM8 6.5v3.5M8 11.8v.2",
  check: "M2.5 8.5l3.5 3.5 7.5-8",
  cross: "M3.5 3.5l9 9M12.5 3.5l-9 9",
  clock: "M8 2.5a5.5 5.5 0 1 0 0 11 5.5 5.5 0 0 0 0-11zM8 5v3.2l2.2 1.3",
  slash: "M8 2.5a5.5 5.5 0 1 0 0 11 5.5 5.5 0 0 0 0-11zM4.2 4.2l7.6 7.6",
};

const $ = (id) => document.getElementById(id);

function el(tag, className, ...children) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  for (const child of children) {
    if (child !== null && child !== undefined) node.append(child);
  }
  return node;
}

function icon(name) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 16 16");
  svg.setAttribute("aria-hidden", "true");
  const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
  path.setAttribute("d", ICONS[name]);
  path.setAttribute("fill", name === "play" ? "currentColor" : "none");
  path.setAttribute("stroke", "currentColor");
  path.setAttribute("stroke-width", "1.75");
  path.setAttribute("stroke-linecap", "round");
  path.setAttribute("stroke-linejoin", "round");
  svg.append(path);
  return svg;
}

// A status always shows an icon and a word, so its colour is never the only cue.
function statusBadge(status) {
  const info = STATUS[status] || { label: status, tone: "neutral", icon: "circle" };
  return el("span", `status status-${info.tone}`, icon(info.icon), el("span", "status-text", info.label));
}

function meter(used, total, name, severity) {
  const share = total > 0 ? Math.min(used / total, 1) : 0;
  const bar = el("div", "meter");
  if (severity) {
    if (share >= 1) bar.classList.add("full");
    else if (share >= 0.9) bar.classList.add("warn");
  }
  bar.setAttribute("role", "img");
  bar.setAttribute("aria-label", `${name}: ${used} of ${total}`);
  const fill = el("span");
  fill.style.width = `${share * 100}%`;
  bar.append(fill);
  return bar;
}

function formatBytes(bytes) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${unit === 0 || value >= 100 ? Math.round(value) : value.toFixed(1)} ${units[unit]}`;
}

function formatDuration(ms) {
  if (ms < 1000) return "<1s";
  const seconds = Math.round(ms / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${seconds % 60}s`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h ${minutes % 60}m`;
  return `${Math.floor(hours / 24)}d ${hours % 24}h`;
}

function ago(iso) {
  const ms = Date.now() - new Date(iso).getTime();
  return ms < 5000 ? "just now" : `${formatDuration(ms)} ago`;
}

function exactTime(iso) {
  return new Date(iso).toLocaleString();
}

function timeText(iso, className) {
  const node = el("span", className, ago(iso));
  node.title = exactTime(iso);
  return node;
}

// --- Key handling. Storage can be unavailable, so memory is the fallback. ---

let memoryKey = "";

function loadKey() {
  try {
    return sessionStorage.getItem(KEY_NAME) || memoryKey;
  } catch {
    return memoryKey;
  }
}

function saveKey(key) {
  memoryKey = key;
  try {
    if (key) sessionStorage.setItem(KEY_NAME, key);
    else sessionStorage.removeItem(KEY_NAME);
  } catch {
    // The key just won't survive a reload.
  }
}

class AuthError extends Error {}

async function api(path, key = loadKey()) {
  const response = await fetch(`../v1/dashboard/${path}`, { headers: { Authorization: `Bearer ${key}` } });
  const body = await response.json().catch(() => null);
  if (response.status === 401 || response.status === 403) {
    throw new AuthError((body && body.error && body.error.message) || "That key wasn't accepted.");
  }
  if (!response.ok) {
    throw new Error((body && body.error && body.error.message) || `The gateway answered ${response.status}.`);
  }
  return body;
}

// --- State ---

const state = {
  jobs: [],
  next: "",
  overview: null,
  lastSuccess: 0,
  openJob: "",
  timer: 0,
  generation: 0, // Replies to a request made before a filter changed are ignored.
  extraPages: 0,
};

function showSignin(message) {
  stopRefresh();
  $("app").hidden = true;
  $("signin").hidden = false;
  if ($("detail").open) $("detail").close();
  $("signin-error").hidden = !message;
  $("signin-error").textContent = message || "";
  $("key").value = "";
  $("key").focus();
}

function showApp() {
  $("signin").hidden = true;
  $("app").hidden = false;
}

// --- Rendering ---

function renderTiles(overview) {
  const online = overview.nodes.filter((node) => node.online).length;
  const count = (group, ...names) => names.reduce((sum, name) => sum + (group[name] || 0), 0);
  const running = count(overview.active, "running", "assigned");
  const failed = count(overview.recent, "failed", "timed_out");
  const finished = count(overview.recent, "succeeded", "failed", "timed_out", "cancelled");
  const tile = (label, value, note, extra) =>
    el("div", "tile", el("dt", null, label), el("dd", null, String(value), note ? el("span", "note", note) : null, extra));

  const tiles = [
    tile("Machines online", `${online} of ${overview.nodes.length}`),
    tile("Running", running),
    tile("Queued", count(overview.active, "queued")),
  ];
  const lost = count(overview.active, "lost");
  if (lost > 0) tiles.push(tile("Lost", lost, "Waiting for their machine to return"));
  tiles.push(tile("Failed, last 24 hours", failed, `of ${finished} finished`));
  tiles.push(tile("Stored", formatBytes(overview.storage.used), `of ${formatBytes(overview.storage.total)}`,
    meter(overview.storage.used, overview.storage.total, "Storage used", true)));
  $("tiles").replaceChildren(...tiles);
}

function testNote(name, test) {
  if (test.status === "succeeded") return el("p", "test-note", `${name} test passed ${ago(test.at)}`);
  if (!test.finished) return el("p", "test-note", `${name} test is running`);
  const why = test.error_code ? ` (${test.error_code})` : "";
  return el("p", "not-ready-note", `${name} test failed ${ago(test.at)}${why}`);
}

function renderMachines(overview) {
  if (overview.nodes.length === 0) {
    $("machines").replaceChildren(el("p", "empty", "No machines yet. Add one on the gateway server with fleet-add-worker."));
    return;
  }
  $("machines").replaceChildren(...overview.nodes.map((node) => {
    const platform = [node.os, node.arch].filter(Boolean).join(" · ");
    let seen = "";
    if (!node.online) seen = node.last_seen_at ? `Last seen ${ago(node.last_seen_at)}` : "Has never connected";
    const meta = [platform, seen].filter(Boolean).join(" · ");
    const card = el("article", "machine",
      el("div", "machine-head", el("span", "machine-name", node.name), statusBadge(node.online ? "online" : "offline")),
      el("p", "machine-meta", meta));
    const usage = (label, use) => el("div", "use",
      el("span", "label", label), el("span", "value", `${use.used} of ${use.total}`), meter(use.used, use.total, label));
    card.append(usage("Jobs", node.jobs));
    for (const name of Object.keys(node.resources).sort()) card.append(usage(name, node.resources[name]));
    if (node.capabilities.length > 0) {
      const notReady = node.not_ready || {};
      card.append(el("ul", "chips", ...node.capabilities.map((name) => {
        if (!notReady[name]) return el("li", null, name);
        return el("li", "not-ready", el("span", "status status-warning", icon("alert")), name);
      })));
      // Spelled out, because a phone has no hover and an icon alone doesn't say why.
      for (const name of Object.keys(notReady).sort()) {
        card.append(el("p", "not-ready-note", `${name} isn't ready: ${notReady[name]}`));
      }
      const tests = node.last_tests || {};
      for (const name of Object.keys(tests).sort()) card.append(testNote(name, tests[name]));
    }
    return card;
  }));
}

function syncOptions(select, values) {
  const current = select.value;
  const wanted = ["", ...values];
  // A selected value stays available even once nothing offers it any more.
  if (current && !wanted.includes(current)) wanted.push(current);
  const existing = [...select.options].map((option) => option.value);
  if (existing.join("\0") === wanted.join("\0")) return;
  select.replaceChildren(...wanted.map((value) => {
    const option = el("option", null, value || "All");
    option.value = value;
    return option;
  }));
  select.value = current;
}

function progressCell(job) {
  const cell = el("td", "progress-cell");
  if (job.error) {
    cell.dataset.label = "Error";
    cell.append(el("span", "progress-text error-text", job.error.message || job.error.code));
    return cell;
  }
  cell.dataset.label = "Progress";
  if (job.terminal) return cell;
  if (!job.progress) {
    cell.append(el("span", "progress-text", job.status === "queued" ? "Waiting for a machine" : "Starting"));
    return cell;
  }
  const { percent, stage, message, current, total } = job.progress;
  const hasPercent = typeof percent === "number";
  const words = [hasPercent ? `${Math.round(percent)}%` : "", stage,
    message, current && total ? `${current} of ${total}` : current].filter(Boolean).join(" · ");
  // One wrapper, so the phone layout's label column sees a single value.
  cell.append(el("div", null, hasPercent ? meter(Math.round(percent), 100, "Progress") : null, el("span", "progress-text", words)));
  return cell;
}

function labelled(label, ...children) {
  const cell = el("td", null, ...children);
  cell.dataset.label = label;
  return cell;
}

function tookCell(job) {
  const cell = el("td", "num");
  cell.dataset.label = "Took";
  if (job.started_at) {
    const end = job.finished_at ? new Date(job.finished_at).getTime() : Date.now();
    cell.textContent = formatDuration(end - new Date(job.started_at).getTime());
  } else {
    cell.textContent = "–";
  }
  return cell;
}

function renderJobs() {
  const focused = document.activeElement && document.activeElement.dataset ? document.activeElement.dataset.job : "";
  $("job-rows").replaceChildren(...state.jobs.map((job) => {
    const open = el("button", "capability", job.test ? `${job.capability} (test)` : job.capability);
    open.type = "button";
    open.dataset.job = job.id;
    const row = el("tr", null,
      labelled("Status", statusBadge(job.status)),
      labelled("Capability", open),
      labelled("Machine", job.node || "–"),
      progressCell(job),
      labelled("Submitted by", job.submitted_by),
      labelled("Created", timeText(job.created_at, "when")),
      tookCell(job));
    row.dataset.job = job.id;
    return row;
  }));
  if (focused) {
    const again = $("job-rows").querySelector(`button[data-job="${CSS.escape(focused)}"]`);
    if (again) again.focus();
  }
  const filtered = $("filter-status").value || $("filter-capability").value || $("filter-node").value;
  $("jobs-empty").hidden = state.jobs.length > 0;
  $("jobs-empty").textContent = filtered ? "No jobs match these filters." : "No jobs yet.";
  $("load-more").hidden = !state.next;
}

function renderFreshness() {
  const line = $("freshness");
  const age = Date.now() - state.lastSuccess;
  if (state.lastSuccess && age > REFRESH_MS * 3) {
    line.textContent = `Can't reach the gateway. Showing data from ${formatDuration(age)} ago.`;
    line.classList.add("stale");
  } else if (state.lastSuccess) {
    line.textContent = "Up to date";
    line.classList.remove("stale");
  }
}

function eventText(event) {
  const payload = event.payload || {};
  switch (event.type) {
    case "progress": {
      const percent = typeof payload.percent === "number" ? `${Math.round(payload.percent)}%` : "";
      return ["Progress", percent, payload.stage, payload.message].filter(Boolean).join(" · ");
    }
    case "failed":
      return `Failed: ${payload.code || "unknown"}`;
    case "completed": {
      const files = (payload.artifacts || []).length;
      return files ? `Completed with ${files} file${files === 1 ? "" : "s"}` : "Completed";
    }
    case "started":
      return "Started";
    default:
      return event.type;
  }
}

function renderDetail(job) {
  $("detail-title").textContent = job.test ? `${job.capability} (test)` : job.capability;
  const facts = el("dl", "facts");
  const fact = (name, value) => {
    if (value === null || value === undefined || value === "") return;
    facts.append(el("dt", null, name), el("dd", null, value));
  };
  fact("Status", statusBadge(job.status));
  fact("Job", job.id);
  fact("Machine", job.node);
  fact("Submitted by", job.submitted_by);
  fact("Attempts", String(job.attempts));
  fact("Created", exactTime(job.created_at));
  if (job.started_at) fact("Started", exactTime(job.started_at));
  if (job.finished_at) fact("Finished", exactTime(job.finished_at));

  const parts = [facts];
  if (job.error) {
    parts.push(el("pre", "message", `${job.error.code}\n${job.error.message || ""}`));
  }
  if (job.artifacts.length > 0) {
    parts.push(el("h3", null, "Result files"));
    parts.push(el("ul", "files", ...job.artifacts.map((file) =>
      el("li", null, el("span", null, file.name), el("span", "size", formatBytes(file.size))))));
  }
  parts.push(el("h3", null, "History"));
  if (job.events.length === 0) {
    parts.push(el("p", "hint", "Nothing has happened yet."));
  } else {
    // Long jobs report often, so keep the start and the end of the story.
    const events = job.events.length > 40 ? [...job.events.slice(0, 5), null, ...job.events.slice(-30)] : job.events;
    parts.push(el("ul", "events", ...events.map((event) => {
      if (!event) return el("li", null, el("span", "text", `${job.events.length - 35} more events`), el("span"));
      const time = el("span", "time", new Date(event.at).toLocaleTimeString());
      time.title = exactTime(event.at);
      return el("li", null, el("span", "text", eventText(event)), time);
    })));
  }
  parts.push(el("p", "privacy", "Input and results aren't available here. Only the agent that submitted a job can read them."));
  $("detail-body").replaceChildren(...parts);
}

// --- Loading ---

function jobsQuery(limit, before) {
  const params = new URLSearchParams({ limit: String(limit) });
  const add = (name, value) => { if (value) params.set(name, value); };
  add("status", $("filter-status").value);
  add("capability", $("filter-capability").value);
  add("node", $("filter-node").value);
  add("before", before);
  return `jobs?${params}`;
}

async function refresh() {
  const generation = state.generation;
  // Keep however many rows the viewer has already asked to see.
  const limit = Math.min(PAGE_SIZE * (state.extraPages + 1), 200);
  const [overview, list] = await Promise.all([api("overview"), api(jobsQuery(limit))]);
  if (generation !== state.generation) return;
  state.overview = overview;
  state.jobs = list.jobs;
  state.next = list.next;
  state.lastSuccess = Date.now();
  renderTiles(overview);
  renderMachines(overview);
  syncOptions($("filter-capability"), overview.capabilities);
  syncOptions($("filter-node"), overview.nodes.map((node) => node.name));
  renderJobs();
  if (state.openJob && $("detail").open) {
    const job = await api(`jobs/${encodeURIComponent(state.openJob)}`);
    if (state.openJob === job.id && $("detail").open) renderDetail(job);
  }
}

function scheduleRefresh() {
  stopRefresh();
  state.timer = setTimeout(tick, REFRESH_MS);
}

function stopRefresh() {
  clearTimeout(state.timer);
  state.timer = 0;
}

async function tick() {
  if (document.hidden) {
    // Nothing is watching, so don't keep the gateway busy.
    return;
  }
  try {
    await refresh();
  } catch (error) {
    if (error instanceof AuthError) {
      saveKey("");
      showSignin("Your key is no longer accepted. Sign in again.");
      return;
    }
  }
  renderFreshness();
  scheduleRefresh();
}

async function openDetail(jobID) {
  state.openJob = jobID;
  try {
    renderDetail(await api(`jobs/${encodeURIComponent(jobID)}`));
    if (!$("detail").open) $("detail").showModal();
  } catch (error) {
    if (error instanceof AuthError) showSignin(error.message);
  }
}

// --- Events ---

$("signin-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const key = $("key").value.trim();
  try {
    await api("overview", key);
  } catch (error) {
    $("signin-error").hidden = false;
    $("signin-error").textContent = error instanceof AuthError ? error.message : "Can't reach the gateway. Try again.";
    return;
  }
  saveKey(key);
  showApp();
  tick();
});

$("signout").addEventListener("click", () => {
  saveKey("");
  state.jobs = [];
  state.overview = null;
  state.lastSuccess = 0;
  showSignin("");
});

for (const id of ["filter-status", "filter-capability", "filter-node"]) {
  $(id).addEventListener("change", () => {
    state.generation++;
    state.extraPages = 0;
    tick();
  });
}

$("load-more").addEventListener("click", async () => {
  const generation = state.generation;
  try {
    const list = await api(jobsQuery(PAGE_SIZE, state.next));
    if (generation !== state.generation) return;
    state.jobs = [...state.jobs, ...list.jobs];
    state.next = list.next;
    state.extraPages++;
    renderJobs();
  } catch (error) {
    if (error instanceof AuthError) showSignin(error.message);
  }
});

$("job-rows").addEventListener("click", (event) => {
  const row = event.target.closest("tr");
  if (row && row.dataset.job) openDetail(row.dataset.job);
});

$("detail-close").addEventListener("click", () => $("detail").close());
$("detail").addEventListener("close", () => { state.openJob = ""; });
$("detail").addEventListener("click", (event) => {
  if (event.target === $("detail")) $("detail").close(); // A click on the backdrop.
});

document.addEventListener("visibilitychange", () => {
  if (!document.hidden && !$("app").hidden) tick();
});

setInterval(() => { if (!$("app").hidden) renderFreshness(); }, 1000);

(async function start() {
  const key = loadKey();
  if (!key) {
    showSignin("");
    return;
  }
  try {
    await api("overview", key);
    showApp();
    tick();
  } catch (error) {
    if (error instanceof AuthError) {
      saveKey("");
      showSignin("Your key is no longer accepted. Sign in again.");
    } else {
      showSignin("Can't reach the gateway. Try again.");
    }
  }
})();
