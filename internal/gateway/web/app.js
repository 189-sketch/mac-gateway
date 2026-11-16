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
  copy: "M5.5 5.5h7v7h-7zM3.5 10.5v-7h7",
  eye: "M1.5 8s2.4-4.5 6.5-4.5 6.5 4.5 6.5 4.5-2.4 4.5-6.5 4.5S1.5 8 1.5 8zM8 6.2a1.8 1.8 0 1 0 0 3.6 1.8 1.8 0 0 0 0-3.6z",
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

class APIError extends Error {
  constructor(message, status, body) {
    super(message);
    this.status = status;
    this.body = body;
  }
}

async function api(path, key = loadKey(), options = {}) {
  const init = { method: options.method || "GET", headers: { Authorization: `Bearer ${key}` } };
  if (options.body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(options.body);
  }
  const response = await fetch(`../v1/dashboard/${path}`, init);
  const body = response.status === 204 ? {} : await response.json().catch(() => null);
  const message = body && body.error && body.error.message;
  // A 403 on an admin action means this key may only view, which isn't a reason to sign out.
  if (response.status === 401 || (response.status === 403 && init.method === "GET" && !path.startsWith("keys"))) {
    throw new AuthError(message || "That key wasn't accepted.");
  }
  if (!response.ok && !(options.accept || []).includes(response.status)) {
    throw new APIError(message || `The gateway answered ${response.status}.`, response.status, body);
  }
  return body;
}

function act(path, method, body, accept) {
  return api(path, loadKey(), { method, body, accept });
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
  admin: false,
  page: "overview",
  keys: null,
  pending: new Set(), // Machine actions waiting for the gateway, as "node" or "node/tool".
  note: null, // { text, tone, until }: what the last machine action did.
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

function actionButton(label, className, data, disabled, title) {
  const button = el("button", className, label);
  button.type = "button";
  Object.assign(button.dataset, data);
  button.disabled = Boolean(disabled);
  if (title) button.title = title;
  return button;
}

function toolChip(node, name, notReady, testable) {
  const warn = notReady ? el("span", "status status-warning", icon("alert")) : null;
  if (!state.admin || !node.online || !testable) return el("li", notReady ? "not-ready" : null, warn, name);
  const busy = state.pending.has(`${node.name}/${name}`) || state.pending.has(node.name);
  const button = actionButton(null, "chip-test", { node: node.name, tool: name }, busy || notReady,
    notReady ? `${name} isn't ready, so it can't be tested` : `Run ${name}'s test on ${node.name}`);
  button.append(el("span", "status status-accent", icon("play")), name);
  return el("li", "testable", button);
}

function machineActions(node) {
  if (!state.admin) return null;
  const actions = el("div", "machine-actions");
  if (node.online) {
    const testable = node.testable || [];
    const busy = state.pending.has(node.name);
    actions.append(actionButton(busy ? "Testing…" : "Test all tools", "quiet small", { testNode: node.name }, busy || testable.length === 0,
      testable.length === 0 ? "None of this machine's tools declares a test" : `Run the test of each of ${node.name}'s tools`));
  } else {
    actions.append(actionButton("Remove", "quiet small danger-text", { removeNode: node.name }, state.pending.has(node.name),
      `Remove ${node.name}; its token stops working`));
  }
  return actions;
}

function renderNote() {
  const line = $("action-note");
  if (!state.note || Date.now() > state.note.until) {
    line.hidden = true;
    return;
  }
  line.hidden = false;
  line.className = `action-note tone-${state.note.tone}`;
  line.textContent = state.note.text;
}

function setNote(text, tone = "good") {
  state.note = { text, tone, until: Date.now() + 15000 };
  renderNote();
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
      const testable = new Set(node.testable || []);
      card.append(el("ul", "chips", ...node.capabilities.map((name) => toolChip(node, name, notReady[name], testable.has(name)))));
      // Spelled out, because a phone has no hover and an icon alone doesn't say why.
      for (const name of Object.keys(notReady).sort()) {
        card.append(el("p", "not-ready-note", `${name} isn't ready: ${notReady[name]}`));
      }
      const tests = node.last_tests || {};
      for (const name of Object.keys(tests).sort()) {
        const note = testNote(name, tests[name]);
        note.dataset.job = tests[name].job_id;
        note.classList.add("opens");
        note.title = "Show this test";
        card.append(note);
      }
    }
    const actions = machineActions(node);
    if (actions) card.append(actions);
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
  if (job.test_output || typeof job.test_exit_code === "number") {
    parts.push(el("h3", null, typeof job.test_exit_code === "number" ? `Test output (exit code ${job.test_exit_code})` : "Test output"));
    parts.push(el("pre", "output", job.test_output || "(nothing printed)"));
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
  setViewer(overview.viewer);
  state.jobs = list.jobs;
  state.next = list.next;
  state.lastSuccess = Date.now();
  renderTiles(overview);
  renderMachines(overview);
  syncOptions($("filter-capability"), overview.capabilities);
  syncOptions($("filter-node"), overview.nodes.map((node) => node.name));
  renderJobs();
  renderNote();
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

// --- Signed-in viewer and pages ---

function setViewer(viewer) {
  state.admin = Boolean(viewer && viewer.admin);
  // Only the limited kind is called out; an admin key is what the page is built for.
  $("viewer").textContent = viewer && viewer.name ? `${viewer.name}${state.admin ? "" : " · view only"}` : "";
  $("viewer").title = viewer && viewer.name ? `Signed in with the ${state.admin ? "admin" : "view-only"} key "${viewer.name}"` : "";
  $("nav").hidden = !state.admin;
  if (!state.admin && state.page === "keys") route();
}

function route() {
  const wanted = location.hash === "#keys" && state.admin ? "keys" : "overview";
  state.page = wanted;
  $("page-overview").hidden = wanted !== "overview";
  $("page-keys").hidden = wanted !== "keys";
  for (const [id, page] of [["nav-overview", "overview"], ["nav-keys", "keys"]]) {
    if (page === wanted) $(id).setAttribute("aria-current", "page");
    else $(id).removeAttribute("aria-current");
  }
  if (wanted === "keys") loadKeys();
}

window.addEventListener("hashchange", () => { if (!$("app").hidden) route(); });

// --- Confirmation ---

let confirmAction = null;

function askConfirm(title, text, okLabel, action) {
  $("confirm-title").textContent = title;
  $("confirm-text").textContent = text;
  $("confirm-ok").textContent = okLabel;
  $("confirm-ok").disabled = false;
  $("confirm-error").hidden = true;
  confirmAction = action;
  $("confirm-dialog").showModal();
}

$("confirm-cancel").addEventListener("click", () => $("confirm-dialog").close());
$("confirm-ok").addEventListener("click", async () => {
  if (!confirmAction) return;
  $("confirm-ok").disabled = true;
  try {
    await confirmAction();
    $("confirm-dialog").close();
  } catch (error) {
    if (error instanceof AuthError) return showSignin(error.message);
    $("confirm-error").hidden = false;
    $("confirm-error").textContent = error.message;
    $("confirm-ok").disabled = false;
  }
});
$("confirm-dialog").addEventListener("close", () => { confirmAction = null; });

// --- Machine actions ---

async function testOnMachine(nodeName, tool) {
  const pendingKey = tool ? `${nodeName}/${tool}` : nodeName;
  state.pending.add(pendingKey);
  if (state.overview) renderMachines(state.overview);
  try {
    const body = await act(`nodes/${encodeURIComponent(nodeName)}/test`, "POST", tool ? { capability: tool } : {}, [422]);
    const started = body.tests || [];
    const skipped = body.skipped || [];
    const parts = [];
    if (started.length === 1) {
      parts.push(`${started[0].already ? "Already testing" : "Testing"} ${started[0].capability} on ${nodeName}.`);
    } else if (started.length > 1) {
      parts.push(`Testing ${started.length} tools on ${nodeName}.`);
    }
    for (const skip of skipped) parts.push(`${skip.capability}: ${skip.reason}.`);
    setNote(parts.join(" ") || "Nothing to test.", started.length ? "good" : "warning");
    if (started.length === 1) openDetail(started[0].job.id);
  } catch (error) {
    if (error instanceof AuthError) return showSignin(error.message);
    setNote(error.message, "critical");
  } finally {
    state.pending.delete(pendingKey);
    tick();
  }
}

function removeMachine(nodeName) {
  askConfirm(`Remove ${nodeName}?`,
    `${nodeName} is offline. Removing it stops its token from working, so it can't connect again, and its tools are no longer offered. Its past jobs stay in the list. To add the machine back, pair it again.`,
    "Remove machine",
    async () => {
      await act(`nodes/${encodeURIComponent(nodeName)}`, "DELETE");
      setNote(`Removed ${nodeName}.`);
      tick();
    });
}

// --- Copying ---

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Without a secure context or permission, fall back to a selection.
    const area = el("textarea", "copy-buffer");
    area.value = text;
    area.setAttribute("readonly", "");
    // Outside an open modal dialog, the page is inert and can't be selected.
    (document.querySelector("dialog[open]") || document.body).append(area);
    area.select();
    let ok = false;
    try { ok = document.execCommand("copy"); } catch { ok = false; }
    area.remove();
    return ok;
  }
}

function flash(button, text) {
  const label = button.querySelector(".label") || button;
  const before = label.textContent;
  label.textContent = text;
  button.classList.add("flashed");
  setTimeout(() => { label.textContent = before; button.classList.remove("flashed"); }, 1500);
}

function copyButton(label, getText, className = "quiet small") {
  const button = el("button", `${className} copy`, icon("copy"), el("span", "label", label));
  button.type = "button";
  button.addEventListener("click", async () => {
    try {
      flash(button, (await copyText(await getText())) ? "Copied" : "Copy failed");
    } catch (error) {
      if (error instanceof AuthError) return showSignin(error.message);
      flash(button, "Couldn't get key");
      $("keys-error").hidden = false;
      $("keys-error").textContent = error.message;
    }
  });
  return button;
}

// --- API keys ---

const secrets = new Map(); // Key ID to secret, for this page view only.

async function keySecret(id) {
  if (!secrets.has(id)) {
    const body = await act(`keys/${encodeURIComponent(id)}/secret`, "GET");
    secrets.set(id, body.secret);
  }
  return secrets.get(id);
}

async function loadKeys() {
  try {
    const body = await api("keys");
    state.keys = body;
    $("keys-error").hidden = true;
    renderKeys();
  } catch (error) {
    if (error instanceof AuthError) return showSignin(error.message);
    $("keys-error").hidden = false;
    $("keys-error").textContent = error.message;
  }
}

function patternList(patterns) {
  if (patterns.length === 1 && patterns[0] === "*") return el("span", "all", "All");
  return el("ul", "chips inline", ...patterns.map((p) => el("li", null, p)));
}

function renderKeys() {
  const { keys, self, mcp_url: mcpURL } = state.keys;
  $("mcp-url").textContent = mcpURL;
  const agents = keys.filter((key) => key.kind === "agent");
  $("key-rows").replaceChildren(...agents.map((key) => {
    const view = actionButton(null, "quiet small", { viewKey: key.id }, !key.viewable,
      key.viewable ? `Show ${key.name}'s key` : "This key was made before keys were kept, so it can't be shown. Make a new one to replace it.");
    view.append(icon("eye"), el("span", "label", "View"));
    const actions = el("div", "row-actions", view);
    if (key.viewable) actions.append(copyButton("Copy", () => keySecret(key.id)));
    actions.append(actionButton("Edit", "quiet small", { editKey: key.id }),
      actionButton("Revoke", "quiet small danger-text", { revokeKey: key.id }));
    const used = key.jobs ? `${key.jobs}` : "None";
    const usedCell = labelled("Jobs", used);
    if (key.last_job_at) usedCell.append(el("span", "when sub", ` · last ${ago(key.last_job_at)}`));
    return el("tr", null,
      labelled("Name", el("span", "key-name", key.name)),
      labelled("Tools", patternList(key.tools)),
      labelled("Machines", patternList(key.nodes)),
      usedCell,
      labelled("Created", key.created_at ? timeText(key.created_at, "when") : el("span", "when", "–")),
      labelled("", actions));
  }));
  $("keys-empty").hidden = agents.length > 0;

  const dashboards = keys.filter((key) => key.kind !== "agent");
  $("dash-key-rows").replaceChildren(...dashboards.map((key) => {
    const kind = key.kind === "admin" ? "Admin" : "View only";
    const row = el("li", null, el("span", "key-name", key.name), el("span", "kind", kind));
    if (key.id === self) row.append(el("span", "hint", "You're signed in with this key"));
    else row.append(actionButton("Revoke", "quiet small danger-text", { revokeKey: key.id }));
    return row;
  }));
  if (dashboards.length === 0) $("dash-key-rows").replaceChildren(el("li", "hint", "None."));
}

function keyByID(id) {
  return state.keys && state.keys.keys.find((key) => key.id === id);
}

function codeLine(label, text) {
  return el("div", "secret-line",
    el("div", "secret-label", el("span", null, label), copyButton("Copy", () => text)),
    el("pre", "secret-text", text));
}

function showSecret(title, name, secret, mcpURL, fresh) {
  $("secret-title").textContent = title;
  const parts = [];
  if (fresh) parts.push(el("p", "hint", `Give this to ${name}. You can see it again here at any time while the key exists.`));
  parts.push(codeLine("API key", secret));
  parts.push(codeLine("MCP address", mcpURL));
  parts.push(codeLine("Claude Code", `claude mcp add --transport http fleet ${mcpURL} --header "Authorization: Bearer ${secret}"`));
  parts.push(codeLine(".mcp.json", JSON.stringify({ mcpServers: { fleet: { type: "http", url: mcpURL, headers: { Authorization: `Bearer ${secret}` } } } }, null, 2)));
  parts.push(el("p", "privacy", "Anyone with this key can use the tools it allows. If it gets out, revoke it and make a new one."));
  $("secret-body").replaceChildren(...parts);
  if (!$("secret-dialog").open) $("secret-dialog").showModal();
}

async function viewKey(id) {
  const key = keyByID(id);
  try {
    const secret = await keySecret(id);
    showSecret(key ? key.name : "API key", key ? key.name : "", secret, state.keys.mcp_url, false);
  } catch (error) {
    if (error instanceof AuthError) return showSignin(error.message);
    $("keys-error").hidden = false;
    $("keys-error").textContent = error.message;
  }
}

// The key form both makes keys and changes them. editing holds the key being changed.
let editing = null;

function splitPatterns(text) {
  return text.split(",").map((p) => p.trim()).filter(Boolean);
}

function renderPick(inputID, pickID, names) {
  const input = $(inputID);
  const chosen = new Set(splitPatterns(input.value));
  $(pickID).replaceChildren(...["*", ...names].map((name) => {
    const button = el("button", "pick-chip", name === "*" ? "All" : name);
    button.type = "button";
    button.dataset.value = name;
    button.setAttribute("aria-pressed", String(chosen.has(name)));
    return button;
  }));
  if (names.length === 0) $(pickID).append(el("span", "hint", "Nothing is connected yet."));
}

function togglePick(inputID, value) {
  let chosen = splitPatterns($(inputID).value);
  if (chosen.includes(value)) chosen = chosen.filter((p) => p !== value);
  else if (value === "*") chosen = ["*"];
  else chosen = [...chosen.filter((p) => p !== "*"), value];
  $(inputID).value = chosen.join(", ");
}

function syncPicks() {
  const overview = state.overview || { capabilities: [], nodes: [] };
  renderPick("key-tools", "key-tools-pick", overview.capabilities);
  renderPick("key-nodes", "key-nodes-pick", overview.nodes.map((node) => node.name));
}

function openKeyForm(key) {
  editing = key || null;
  $("key-form-title").textContent = key ? `Change what ${key.name} can use` : "New API key";
  $("key-form-submit").textContent = key ? "Save" : "Create key";
  $("key-name").value = key ? key.name : "";
  $("key-name").disabled = Boolean(key);
  $("key-tools").value = key ? key.tools.join(", ") : "*";
  $("key-nodes").value = key ? key.nodes.join(", ") : "*";
  $("key-form-error").hidden = true;
  $("key-form-submit").disabled = false;
  syncPicks();
  $("key-form-dialog").showModal();
  (key ? $("key-tools") : $("key-name")).focus();
}

$("key-new").addEventListener("click", () => openKeyForm(null));
$("key-form-close").addEventListener("click", () => $("key-form-dialog").close());
for (const [input, pick] of [["key-tools", "key-tools-pick"], ["key-nodes", "key-nodes-pick"]]) {
  $(pick).addEventListener("click", (event) => {
    const button = event.target.closest("button[data-value]");
    if (!button) return;
    togglePick(input, button.dataset.value);
    syncPicks();
  });
  $(input).addEventListener("input", syncPicks);
}

$("key-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const tools = splitPatterns($("key-tools").value);
  const nodes = splitPatterns($("key-nodes").value);
  $("key-form-submit").disabled = true;
  try {
    if (editing) {
      await act(`keys/${encodeURIComponent(editing.id)}`, "PUT", { tools, nodes });
      $("key-form-dialog").close();
    } else {
      const name = $("key-name").value.trim();
      const body = await act("keys", "POST", { name, tools, nodes });
      secrets.set(body.id, body.secret);
      $("key-form-dialog").close();
      showSecret(`Key for ${body.name}`, body.name, body.secret, body.mcp_url, true);
    }
    loadKeys();
  } catch (error) {
    if (error instanceof AuthError) return showSignin(error.message);
    $("key-form-error").hidden = false;
    $("key-form-error").textContent = error.message;
    $("key-form-submit").disabled = false;
  }
});

function revokeKey(id) {
  const key = keyByID(id);
  if (!key) return;
  const what = key.kind === "agent" ? "Agents using it lose access straight away. Jobs it already started keep running." : "It can't sign in to this page any more.";
  askConfirm(`Revoke ${key.name}'s key?`, `${what} This can't be undone, and the name can't be used for a new key.`, "Revoke key", async () => {
    await act(`keys/${encodeURIComponent(id)}`, "DELETE");
    secrets.delete(id);
    loadKeys();
  });
}

$("page-keys").addEventListener("click", (event) => {
  const button = event.target.closest("button");
  if (!button) return;
  if (button.dataset.viewKey) viewKey(button.dataset.viewKey);
  else if (button.dataset.editKey) openKeyForm(keyByID(button.dataset.editKey));
  else if (button.dataset.revokeKey) revokeKey(button.dataset.revokeKey);
});

$("secret-close").addEventListener("click", () => $("secret-dialog").close());
for (const id of ["secret-dialog", "key-form-dialog", "confirm-dialog"]) {
  $(id).addEventListener("click", (event) => {
    if (event.target === $(id)) $(id).close(); // A click on the backdrop.
  });
}

// --- Events ---

$("signin-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const key = $("key").value.trim();
  try {
    setViewer((await api("overview", key)).viewer);
  } catch (error) {
    $("signin-error").hidden = false;
    $("signin-error").textContent = error instanceof AuthError ? error.message : "Can't reach the gateway. Try again.";
    return;
  }
  saveKey(key);
  showApp();
  route();
  tick();
});

$("signout").addEventListener("click", () => {
  saveKey("");
  state.jobs = [];
  state.overview = null;
  state.keys = null;
  state.admin = false;
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

$("machines").addEventListener("click", (event) => {
  const target = event.target.closest("button, [data-job]");
  if (!target) return;
  if (target.dataset.tool) testOnMachine(target.dataset.node, target.dataset.tool);
  else if (target.dataset.testNode) testOnMachine(target.dataset.testNode, "");
  else if (target.dataset.removeNode) removeMachine(target.dataset.removeNode);
  else if (target.dataset.job) openDetail(target.dataset.job);
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
    setViewer((await api("overview", key)).viewer);
    showApp();
    route();
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
