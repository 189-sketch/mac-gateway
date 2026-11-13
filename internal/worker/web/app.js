"use strict";

// Tool files, logs and command output are only ever written as text. Nothing is
// assigned to innerHTML, and the page's content security policy would refuse to
// run markup even if it were.

const TOKEN_KEY = "fleet-worker-ui";
const STATUS_MS = 3000;
const LOG_MS = 2000;
const TRY_MS = 700;
const CHECK_DELAY_MS = 700;
const MAX_LOG_LINES = 2000;

const ICONS = {
  circle: "M8 2.5a5.5 5.5 0 1 0 0 11 5.5 5.5 0 0 0 0-11z",
  play: "M5 3l8 5-8 5z",
  alert: "M8 2l6.5 11.5h-13zM8 6.5v3.5M8 11.8v.2",
  check: "M2.5 8.5l3.5 3.5 7.5-8",
  cross: "M3.5 3.5l9 9M12.5 3.5l-9 9",
  slash: "M8 2.5a5.5 5.5 0 1 0 0 11 5.5 5.5 0 0 0 0-11zM4.2 4.2l7.6 7.6",
};

const STATES = {
  ready: { label: "Ready", tone: "good", icon: "check" },
  not_ready: { label: "Not ready", tone: "warning", icon: "alert" },
  disabled: { label: "Off", tone: "neutral", icon: "slash" },
  invalid: { label: "Has problems", tone: "critical", icon: "cross" },
  running: { label: "Running", tone: "accent", icon: "play" },
  succeeded: { label: "Succeeded", tone: "good", icon: "check" },
  failed: { label: "Failed", tone: "critical", icon: "cross" },
  connected: { label: "Connected to the gateway", tone: "good", icon: "check" },
  disconnected: { label: "Not connected to the gateway", tone: "warning", icon: "alert" },
  unpaired: { label: "Not paired with a gateway yet", tone: "warning", icon: "alert" },
  rejected: { label: "The gateway no longer knows this machine", tone: "critical", icon: "cross" },
  ok: { label: "Worked", tone: "good", icon: "check" },
  skipped: { label: "Skipped", tone: "neutral", icon: "slash" },
};

const KINDS = { cli: "Program", http: "HTTP API", http_async: "HTTP job queue" };

const PARAM_TYPES = [
  ["string", "Text"],
  ["integer", "Whole number"],
  ["number", "Number"],
  ["boolean", "Yes / no"],
  ["array", "List of text"],
];

const ARG_KINDS = [
  ["always", "Always"],
  ["value", "If given"],
  ["when", "If true"],
  ["each", "For each item"],
];

const MEDIA = {
  png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", gif: "image/gif", webp: "image/webp",
  wav: "audio/wav", mp3: "audio/mpeg", m4a: "audio/mp4", ogg: "audio/ogg", flac: "audio/flac", aac: "audio/aac",
  mp4: "video/mp4", mov: "video/quicktime", webm: "video/webm",
};

const $ = (id) => document.getElementById(id);

let token = null;
let status = null;
let view = "tools";
let statusTimer = null;
let logTimer = null;
let logAfter = 0;
let detected = null;
let idCounter = 0;

// The tool being edited.
const ed = {
  file: "",       // Its file in the tools folder, or "" for a tool in worker.yaml.
  builtin: "",    // The name of a tool in worker.yaml.
  version: "",
  yaml: "",
  tool: null,
  mode: "form",
  dirty: false,
  checkTimer: null,
  checkSeq: 0,
  tryID: "",
  tryTimer: null,
  mediaURLs: [],
};

// --- Basics ---

function el(tag, className, ...children) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  for (const child of children) {
    if (child !== null && child !== undefined && child !== false) node.append(child);
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

// A state always shows an icon and a word, so colour is never the only cue.
function badge(state, label) {
  const info = STATES[state] || { label: state, tone: "neutral", icon: "circle" };
  return el("span", `status status-${info.tone}`, icon(info.icon), el("span", "status-text", label || info.label));
}

function button(text, className, onClick, attrs = {}) {
  const b = el("button", className, text);
  b.type = "button";
  for (const [k, v] of Object.entries(attrs)) b.setAttribute(k, v);
  b.addEventListener("click", onClick);
  return b;
}

function nextID() {
  idCounter += 1;
  return `f${idCounter}`;
}

let toastTimer = null;
function toast(text, ms = 3500) {
  const t = $("toast");
  t.textContent = text;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, ms);
}

function ago(iso) {
  if (!iso) return "";
  const seconds = Math.max(0, (Date.now() - Date.parse(iso)) / 1000);
  if (seconds < 60) return `${Math.round(seconds)}s ago`;
  if (seconds < 3600) return `${Math.round(seconds / 60)} min ago`;
  if (seconds < 86400) return `${Math.round(seconds / 3600)} h ago`;
  return new Date(iso).toLocaleString();
}

function size(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  return `${(bytes / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

// Shows a command the way a shell would need it typed, so it can be read and
// copied. The worker itself never uses a shell.
function shellQuote(arg) {
  if (arg !== "" && /^[A-Za-z0-9_./:=@%+,-]+$/.test(arg)) return arg;
  return `'${arg.replace(/'/g, `'\\''`)}'`;
}

// --- Talking to the worker ---

class APIError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

async function api(method, path, body) {
  const headers = { Authorization: `Bearer ${token}` };
  const init = { method, headers, cache: "no-store" };
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  let resp;
  try {
    resp = await fetch(path, init);
  } catch {
    throw new APIError(0, "The worker didn't answer. It may have stopped.");
  }
  if (resp.status === 401) {
    signedOut("You've been signed out, probably because the worker restarted. Run this again to open the page:");
    throw new APIError(401, "signed out");
  }
  let data = null;
  try { data = await resp.json(); } catch { /* No body. */ }
  if (!resp.ok) throw new APIError(resp.status, (data && data.error) || resp.statusText);
  return data;
}

async function fetchBlob(path) {
  const resp = await fetch(path, { headers: { Authorization: `Bearer ${token}` }, cache: "no-store" });
  if (!resp.ok) throw new APIError(resp.status, "The file couldn't be fetched.");
  return resp.blob();
}

// --- Signing in ---

async function start() {
  const hash = new URLSearchParams(location.hash.slice(1));
  const code = hash.get("code");
  if (code) {
    // The code works once. Take it out of the address so it isn't kept in history.
    history.replaceState(null, "", location.pathname);
    try {
      const resp = await fetch("api/sign-in", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ code }),
      });
      const data = await resp.json();
      if (!resp.ok) throw new Error(data.error || resp.statusText);
      sessionStorage.setItem(TOKEN_KEY, data.token);
    } catch (err) {
      signedOut("That sign-in link has expired or was already used. Run this to get a new one:");
      return;
    }
  }
  try { token = sessionStorage.getItem(TOKEN_KEY); } catch { token = null; }
  if (!token) {
    signedOut();
    return;
  }
  $("signin").hidden = true;
  $("app").hidden = false;
  await refreshStatus();
  statusTimer = setInterval(refreshStatus, STATUS_MS);
}

function signedOut(message) {
  try { sessionStorage.removeItem(TOKEN_KEY); } catch { /* Storage may be off. */ }
  token = null;
  clearInterval(statusTimer);
  clearInterval(logTimer);
  clearTimeout(ed.tryTimer);
  if (message) $("signin-message").textContent = message;
  $("app").hidden = true;
  $("signin").hidden = false;
}

// --- Views ---

function show(next) {
  view = next;
  for (const name of ["tools", "editor", "connection", "activity", "machine"]) {
    $(`view-${name}`).hidden = name !== next;
  }
  for (const tab of document.querySelectorAll(".tab")) {
    const current = tab.dataset.view === next || (next === "editor" && tab.dataset.view === "tools");
    if (current) tab.setAttribute("aria-current", "page");
    else tab.removeAttribute("aria-current");
  }
  clearInterval(logTimer);
  if (next === "activity") {
    refreshLogs();
    logTimer = setInterval(refreshLogs, LOG_MS);
  }
  if (next === "machine") renderMachine();
  if (next === "connection") renderConnection();
  window.scrollTo(0, 0);
}

function leaveEditor() {
  if (ed.dirty && !confirm("Leave without saving your changes?")) return false;
  ed.dirty = false;
  return true;
}

for (const tab of document.querySelectorAll(".tab")) {
  tab.addEventListener("click", () => {
    if (view === "editor" && !leaveEditor()) return;
    show(tab.dataset.view);
  });
}

window.addEventListener("beforeunload", (event) => {
  if (ed.dirty) event.preventDefault();
});

// --- Status and the tools list ---

let firstStatus = true;

function connectionState() {
  if (!status.paired) return "unpaired";
  if (status.connected) return "connected";
  return status.rejected ? "rejected" : "disconnected";
}

function gatewayHost() {
  try { return new URL(status.gateway).host; } catch { return status.gateway; }
}

async function refreshStatus() {
  try {
    status = await api("GET", "api/status");
  } catch (err) {
    if (err.status !== 401) {
      $("connection").replaceChildren(badge("disconnected", "The worker isn't answering"));
    }
    return;
  }
  $("node-name").textContent = status.node;
  document.title = `${status.node} · Worker`;
  const state = connectionState();
  const conn = badge(state, state === "connected" ? `Connected to ${gatewayHost()}`
    : state === "disconnected" ? `Not connected to ${gatewayHost()}` : undefined);
  conn.title = state === "connected" ? `Since ${new Date(status.connected_since).toLocaleString()}` : "Open the Connection tab.";
  $("connection").replaceChildren(conn);
  // A machine that isn't paired can't do anything yet, so start where that's fixed.
  if (firstStatus) {
    firstStatus = false;
    if (state === "unpaired" || state === "rejected") show("connection");
  }
  if (view === "connection") renderConnection();
  const activityTab = document.querySelector('.tab[data-view="activity"]');
  activityTab.textContent = status.running.length ? `Activity (${status.running.length})` : "Activity";
  renderTools();
  if (view === "activity") renderRunning();
  if (view === "machine") renderFacts();
}

function renderTools() {
  const rows = status.tools.map((t) => {
    const name = button(t.name, "link tool-name", () => openTool(t));
    const where = t.file ? `tools/${t.file}` : "worker.yaml";
    const toolCell = el("td", "", name,
      t.description ? el("div", "tool-desc", t.description) : null,
      el("div", "tool-file", where));

    const stateCell = el("td", "", badge(t.state));
    const note = t.state === "not_ready" ? [t.reason, t.detail].filter(Boolean).join(": ") : t.error;
    if (note) stateCell.append(el("div", "state-note", note));

    let toggle = el("span", "hint", "—");
    if (t.file && !(t.state === "invalid" && !t.adapter)) {
      const box = el("input");
      box.type = "checkbox";
      box.checked = t.state !== "disabled";
      box.setAttribute("aria-label", `Offer ${t.name}`);
      box.addEventListener("change", () => setEnabled(t.file, box.checked, box));
      toggle = box;
    } else if (!t.file) {
      toggle.title = "Declared in worker.yaml";
    }
    return el("tr", "", toolCell, stateCell, el("td", "", KINDS[t.adapter] || t.adapter || ""), el("td", "", toggle));
  });
  $("tool-rows").replaceChildren(...rows);
  $("no-tools").hidden = rows.length > 0;
}

async function setEnabled(file, enabled, box) {
  box.disabled = true;
  try {
    await api("POST", `api/files/${encodeURIComponent(file)}/enabled`, { enabled });
    toast(enabled ? "Turned on. The gateway hears about it within a few seconds." : "Turned off.");
    setTimeout(refreshStatus, 600);
  } catch (err) {
    box.checked = !enabled;
    if (err.status !== 401) toast(err.message, 8000);
  } finally {
    box.disabled = false;
  }
}

// --- New tool ---

$("new-tool").addEventListener("click", async () => {
  let templates;
  try {
    templates = await api("GET", "api/templates");
  } catch (err) {
    toast(err.message);
    return;
  }
  const list = templates.map((t, i) => {
    const radio = el("input");
    radio.type = "radio";
    radio.name = "template";
    radio.value = t.name;
    radio.checked = i === 0;
    return el("label", "template", radio, el("strong", "", t.title), el("span", "", t.description));
  });
  $("template-list").replaceChildren(...list);
  $("new-name").value = "";
  $("new-error").hidden = true;
  $("new-dialog").showModal();
  $("new-name").focus();
});

$("new-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const template = new FormData($("new-form")).get("template");
  const name = $("new-name").value.trim();
  try {
    const file = await api("POST", "api/files", { template, name });
    $("new-dialog").close();
    loadEditor(file, "");
    setTimeout(refreshStatus, 600);
  } catch (err) {
    $("new-error").textContent = err.message;
    $("new-error").hidden = false;
  }
});

for (const close of document.querySelectorAll("[data-close]")) {
  close.addEventListener("click", () => close.closest("dialog").close());
}

// --- Editor ---

async function openTool(t) {
  try {
    if (t.file) {
      loadEditor(await api("GET", `api/files/${encodeURIComponent(t.file)}`), "");
    } else {
      loadEditor(await api("GET", `api/builtin/${encodeURIComponent(t.name)}`), t.name);
    }
  } catch (err) {
    if (err.status !== 401) toast(err.message, 6000);
  }
}

function loadEditor(file, builtin) {
  ed.file = builtin ? "" : file.file;
  ed.builtin = builtin;
  ed.version = file.version;
  ed.yaml = file.yaml;
  ed.tool = file.tool || null;
  ed.dirty = false;
  stopTry();
  $("try-result").hidden = true;
  $("try-call").hidden = true;
  $("try-problem").hidden = true;
  $("try-input").value = JSON.stringify(defaultInput(ed.tool), null, 2);

  const readOnly = Boolean(builtin);
  $("editor-readonly").hidden = !readOnly;
  $("editor-save").hidden = readOnly;
  $("editor-delete").hidden = readOnly;
  $("yaml").readOnly = readOnly;
  $("editor-file").textContent = readOnly ? "worker.yaml" : `tools/${ed.file}`;
  showProblem(file.problem);
  setMode(formSupported() ? "form" : "yaml", false);
  updateTitle();
  show("editor");
}

function updateTitle() {
  const name = (ed.tool && ed.tool.name) || ed.builtin || ed.file;
  $("editor-title").textContent = ed.dirty ? `${name} (not saved)` : name;
}

function formSupported() {
  return Boolean(ed.tool) && (ed.tool.adapter === "cli" || !ed.tool.adapter) && !ed.builtin;
}

function showProblem(problem) {
  const box = $("editor-problem");
  if (!problem) {
    box.hidden = true;
    return;
  }
  const off = ed.tool && ed.tool.enabled === false;
  box.className = off ? "problem warning" : "problem";
  box.textContent = off ? `To turn this tool on, first fix: ${problem}` : problem;
  box.hidden = false;
}

function setMode(mode, convert = true) {
  const apply = () => {
    ed.mode = mode;
    $("mode-form").setAttribute("aria-selected", String(mode === "form"));
    $("mode-yaml").setAttribute("aria-selected", String(mode === "yaml"));
    $("mode-form").disabled = !formSupported() && mode !== "form";
    $("form").hidden = mode !== "form";
    $("yaml-pane").hidden = mode !== "yaml";
    const note = $("form-note");
    if (mode === "form") {
      note.textContent = "Saving from the form rewrites the file in a standard layout, without its comments. Use YAML to keep them.";
      note.hidden = false;
      renderForm();
    } else {
      $("yaml").value = ed.yaml;
      note.hidden = !(ed.tool && ed.tool.adapter && ed.tool.adapter !== "cli");
      note.textContent = "HTTP tools are edited as YAML. The templates show each setting.";
    }
  };
  if (!convert || mode === ed.mode) {
    apply();
    return;
  }
  // Carry unsaved changes across, through the worker's own reading of them.
  if (mode === "yaml") {
    api("POST", "api/convert", { tool: ed.tool }).then((r) => {
      if (ed.dirty) ed.yaml = r.yaml;
      apply();
    }).catch((err) => showProblem(err.message));
  } else {
    api("POST", "api/convert", { yaml: ed.yaml }).then((r) => {
      ed.tool = r.tool;
      if (!formSupported()) {
        showProblem("The form only handles command-line tools.");
        return;
      }
      apply();
    }).catch((err) => showProblem(`The YAML can't be read: ${err.message}`));
  }
}

$("mode-form").addEventListener("click", () => setMode("form"));
$("mode-yaml").addEventListener("click", () => setMode("yaml"));
$("yaml").addEventListener("input", () => {
  ed.yaml = $("yaml").value;
  changed();
});

$("editor-back").addEventListener("click", () => {
  if (!leaveEditor()) return;
  stopTry();
  show("tools");
  refreshStatus();
});

function changed() {
  ed.dirty = true;
  updateTitle();
  clearTimeout(ed.checkTimer);
  ed.checkTimer = setTimeout(checkTool, CHECK_DELAY_MS);
}

// The YAML the editor holds now, saved or not.
async function currentYAML() {
  if (ed.mode === "yaml") return ed.yaml;
  const r = await api("POST", "api/convert", { tool: ed.tool });
  return r.yaml;
}

async function checkTool() {
  const seq = ++ed.checkSeq;
  let problem = "";
  try {
    const yaml = await currentYAML();
    const r = await api("POST", "api/check", { yaml, file: ed.file });
    problem = r.problem || "";
  } catch (err) {
    problem = err.message;
  }
  if (seq === ed.checkSeq) showProblem(problem);
}

$("editor-save").addEventListener("click", async () => {
  if (!ed.dirty) {
    toast("There are no changes to save.");
    return;
  }
  const body = { version: ed.version };
  if (ed.mode === "form") body.tool = ed.tool;
  else body.yaml = ed.yaml;
  const save = $("editor-save");
  save.disabled = true;
  try {
    const file = await api("PUT", `api/files/${encodeURIComponent(ed.file)}`, body);
    ed.version = file.version;
    ed.yaml = file.yaml;
    ed.tool = file.tool || ed.tool;
    ed.dirty = false;
    updateTitle();
    showProblem(file.problem);
    if (ed.mode === "yaml") $("yaml").value = ed.yaml;
    toast(ed.tool && ed.tool.enabled === false ? "Saved. It's off, so the gateway doesn't see it yet." : "Saved. The worker uses it within a few seconds.");
    setTimeout(refreshStatus, 600);
  } catch (err) {
    if (err.status !== 401) showProblem(err.message);
  } finally {
    save.disabled = false;
  }
});

$("editor-delete").addEventListener("click", async () => {
  if (!confirm(`Delete tools/${ed.file}? This can't be undone.`)) return;
  try {
    await api("DELETE", `api/files/${encodeURIComponent(ed.file)}`);
    ed.dirty = false;
    stopTry();
    toast("Deleted.");
    show("tools");
    refreshStatus();
  } catch (err) {
    if (err.status !== 401) showProblem(err.message);
  }
});

// --- The form ---
// It edits ed.tool, a plain copy of the tool file. Settings the form doesn't
// show are kept as they are.

function setField(obj, key, value) {
  const empty = value === undefined || value === "" || value === null ||
    (Array.isArray(value) && value.length === 0) ||
    (typeof value === "object" && !Array.isArray(value) && Object.keys(value).length === 0);
  if (empty) delete obj[key];
  else obj[key] = value;
}

function control(tag, value, onInput, attrs = {}) {
  const node = el(tag);
  if (tag === "input" && !attrs.type) node.type = "text";
  for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
  node.value = value === undefined || value === null ? "" : String(value);
  node.spellcheck = false;
  node.addEventListener("input", () => { onInput(node.value); changed(); });
  return node;
}

function checkbox(checked, onChange, text) {
  const box = el("input");
  box.type = "checkbox";
  box.checked = Boolean(checked);
  box.addEventListener("change", () => { onChange(box.checked); changed(); });
  return el("label", "inline", box, text);
}

function choice(options, value, onChange) {
  const node = el("select");
  for (const [v, label] of options) {
    const option = el("option", "", label);
    option.value = v;
    node.append(option);
  }
  node.value = value;
  node.addEventListener("change", () => { onChange(node.value); changed(); });
  return node;
}

function field(label, input, hint) {
  const id = nextID();
  const target = input.matches("input, select, textarea") ? input : input.querySelector("input, select, textarea");
  if (target) target.id = id;
  const l = el("label", "", label);
  l.htmlFor = id;
  return el("div", "field", l, input, hint ? el("p", "hint", hint) : null);
}

function section(title, ...children) {
  return el("fieldset", "", el("legend", "", title), ...children);
}

function lines(text) {
  return text.split("\n").map((s) => s.trim()).filter((s) => s !== "");
}

function paramNames() {
  const props = (ed.tool.input_schema && ed.tool.input_schema.properties) || {};
  return Object.keys(props);
}

function renderForm() {
  const t = ed.tool;
  if (!t) return;
  $("form").replaceChildren(
    basicsSection(t),
    programSection(t),
    paramsSection(t),
    argsSection(t),
    resultSection(t),
    outputsSection(t),
    runningSection(t),
    testSection(t),
  );
}

function basicsSection(t) {
  return section("About",
    field("Name", control("input", t.name, (v) => { t.name = v.trim(); updateTitle(); }), "Agents see this name. Lowercase letters, digits, dots, dashes and underscores."),
    field("Description", control("textarea", t.description, (v) => setField(t, "description", v), { rows: 3 }),
      "What it does and when to use it. Agents read this to choose a tool, so be specific."),
    el("div", "field", checkbox(t.enabled !== false, (on) => { t.enabled = on; }, "Offer this tool to the gateway")),
  );
}

function programSection(t) {
  if (!Array.isArray(t.command)) t.command = [""];
  const program = control("input", t.command[0], (v) => { t.command[0] = v.trim(); }, { list: "programs", placeholder: "/full/path/to/program" });
  const pick = button("Browse…", "quiet", () => browse("file", program.value, (path) => {
    program.value = path;
    t.command[0] = path;
    changed();
  }));
  loadProgramList();

  const cwd = control("input", t.cwd, (v) => setField(t, "cwd", v.trim()), { placeholder: "The job's own empty folder" });
  const pickCwd = button("Browse…", "quiet", () => browse("folder", cwd.value, (path) => {
    cwd.value = path;
    setField(t, "cwd", path);
    changed();
  }));

  return section("Program",
    field("Program", el("div", "with-button", program, pick), "The full path is best. There's no shell, so pipes and wildcards don't work; to use them, run a script."),
    field("Fixed arguments", control("textarea", t.command.slice(1).join("\n"), (v) => {
      t.command = [t.command[0] || "", ...lines(v)];
    }, { rows: 3, class: "code", placeholder: "One per line, such as\n-p\n{{input.prompt}}" }), "One argument per line, passed exactly as written. Use {{input.name}} for a parameter that's always given, and {{workdir}} for the job's folder."),
    field("Standard input", control("textarea", t.stdin, (v) => setField(t, "stdin", v), { rows: 2, class: "code", placeholder: "{{input.prompt}}" }), "Sent to the program as it starts. Best for long or free text."),
    field("Working folder", el("div", "with-button", cwd, pickCwd), "Where the program runs. It must exist. Output files are still collected from the job's folder."),
    el("div", "field", el("span", "label", "Environment"), kvRows(t, "env", "env", "NAME", "value or {{env.NAME}}"),
      el("p", "hint", "Programs get only PATH, HOME, LANG, LC_ALL, TMPDIR and these. {{env.NAME}} copies a value from the worker's own environment.")),
  );
}

function kvRows(t, key, kind, keyHint, valueHint, numeric = false) {
  const wrap = el("div", "rows");
  const render = () => {
    const obj = t[key] || {};
    const rows = Object.entries(obj).map(([k, v]) => {
      const name = control("input", k, () => {}, { placeholder: keyHint, "aria-label": "Name" });
      name.addEventListener("change", () => {
        const next = {};
        for (const [k2, v2] of Object.entries(t[key] || {})) next[k2 === k ? name.value.trim() : k2] = v2;
        setField(t, key, next);
        render();
      });
      const value = control("input", v, (nv) => {
        const current = t[key] || {};
        current[name.value.trim() || k] = numeric ? Number(nv) : nv;
        t[key] = current;
      }, { placeholder: valueHint, "aria-label": "Value", type: numeric ? "number" : "text" });
      const remove = button("×", "remove", () => {
        const current = t[key] || {};
        delete current[k];
        setField(t, key, current);
        changed();
        render();
      }, { "aria-label": `Remove ${k}` });
      return el("div", `rowline ${kind}`, name, value, remove);
    });
    const add = button("Add", "quiet add", () => {
      const current = t[key] || {};
      let n = 1;
      while (`NAME${n}` in current) n++;
      current[numeric ? `resource${n}` : `NAME${n}`] = numeric ? 1 : "";
      t[key] = current;
      changed();
      render();
    });
    wrap.replaceChildren(...rows, el("div", "", add));
  };
  render();
  return wrap;
}

function paramsSection(t) {
  const schema = t.input_schema || (t.input_schema = { type: "object", properties: {} });
  const wrap = el("div");
  let asJSON = false;
  const toggle = button("Edit as JSON schema", "quiet", () => {
    asJSON = !asJSON;
    toggle.textContent = asJSON ? "Edit as a table" : "Edit as JSON schema";
    render();
  });

  const render = () => {
    if (asJSON) {
      const area = control("textarea", JSON.stringify(t.input_schema, null, 2), (v) => {
        try {
          t.input_schema = JSON.parse(v);
          area.setCustomValidity("");
        } catch {
          area.setCustomValidity("Not valid JSON");
        }
      }, { rows: 12, class: "code" });
      wrap.replaceChildren(area);
      return;
    }
    const s = t.input_schema;
    if (!s.properties) s.properties = {};
    const required = new Set(s.required || []);
    const head = el("div", "rowline params head", el("span", "", "Name"), el("span", "", "Type"), el("span", "", "Required"), el("span", "", "Default"), el("span", "", "Description"), el("span"));
    const rows = Object.entries(s.properties).map(([name, prop]) => {
      const nameInput = control("input", name, () => {}, { "aria-label": "Parameter name" });
      nameInput.addEventListener("change", () => {
        const next = nameInput.value.trim();
        if (!next || next === name || next in s.properties) {
          nameInput.value = name;
          return;
        }
        const props = {};
        for (const [k, v] of Object.entries(s.properties)) props[k === name ? next : k] = v;
        s.properties = props;
        if (required.has(name)) setField(s, "required", (s.required || []).map((r) => (r === name ? next : r)));
        changed();
        renderForm();
      });
      const known = PARAM_TYPES.some(([v]) => v === prop.type);
      const typeSelect = choice(known ? PARAM_TYPES : [...PARAM_TYPES, [String(prop.type), `Other (${JSON.stringify(prop.type)})`]], String(prop.type), (v) => {
        prop.type = v;
        if (v === "array") prop.items = prop.items || { type: "string" };
        else delete prop.items;
        delete prop.default;
        render();
        renderForm();
      });
      typeSelect.setAttribute("aria-label", "Type");
      const req = el("input");
      req.type = "checkbox";
      req.checked = required.has(name);
      req.setAttribute("aria-label", "Required");
      req.addEventListener("change", () => {
        const list = (s.required || []).filter((r) => r !== name);
        if (req.checked) list.push(name);
        setField(s, "required", list);
        changed();
      });
      const def = defaultControl(prop);
      const desc = control("input", prop.description, (v) => setField(prop, "description", v), { "aria-label": "Description", placeholder: "Shown to agents" });
      const remove = button("×", "remove", () => {
        delete s.properties[name];
        setField(s, "required", (s.required || []).filter((r) => r !== name));
        changed();
        renderForm();
      }, { "aria-label": `Remove ${name}` });
      return el("div", "rowline params", nameInput, typeSelect, el("span", "", req), def, desc, remove);
    });
    const add = button("Add parameter", "quiet add", () => {
      let n = 1;
      while (`param${n}` in s.properties) n++;
      s.properties[`param${n}`] = { type: "string" };
      changed();
      renderForm();
    });
    const extra = Object.keys(s).filter((k) => !["type", "properties", "required", "additionalProperties"].includes(k));
    wrap.replaceChildren(el("div", "rows", head, ...rows), add,
      extra.length ? el("p", "hint", `This schema also uses ${extra.join(", ")}, which is kept. Edit it as JSON to change that.`) : null);
  };
  render();

  const strict = checkbox(schema.additionalProperties === false, (on) => {
    if (on) t.input_schema.additionalProperties = false;
    else delete t.input_schema.additionalProperties;
  }, "Refuse parameters not listed here");

  return section("Parameters",
    el("p", "hint", "What callers pass in. A parameter used in the program's fixed arguments, standard input or environment must be required or have a default. Rules such as enum or minLength are kept; edit them as JSON."),
    wrap,
    el("div", "row", toggle, strict),
    field("Input files", control("input", (t.input_files || []).join(", "), (v) => setField(t, "input_files", v.split(",").map((s) => s.trim()).filter(Boolean)), { placeholder: "audio, reference" }),
      "Names of files every job must send. Use {{files.name}} for each one's local path."),
  );
}

function defaultControl(prop) {
  const label = { "aria-label": "Default" };
  switch (prop.type) {
    case "boolean":
      return choice([["", "None"], ["true", "Yes"], ["false", "No"]], prop.default === undefined ? "" : String(prop.default), (v) => {
        if (v === "") delete prop.default;
        else prop.default = v === "true";
      });
    case "integer":
    case "number":
      return control("input", prop.default, (v) => {
        if (v.trim() === "" || Number.isNaN(Number(v))) delete prop.default;
        else prop.default = Number(v);
      }, { ...label, type: "number", step: prop.type === "integer" ? "1" : "any" });
    case "string":
      return control("input", prop.default, (v) => setField(prop, "default", v), label);
    default:
      return control("input", prop.default === undefined ? "" : JSON.stringify(prop.default), (v) => {
        try { setField(prop, "default", v.trim() ? JSON.parse(v) : undefined); } catch { /* Kept until it's valid. */ }
      }, { ...label, placeholder: "JSON" });
  }
}

function argKind(item) {
  if (typeof item === "string") return "always";
  if (item.when) return "when";
  if (item.each) return "each";
  return "value";
}

function argsSection(t) {
  const wrap = el("div", "rows");
  const fieldOptions = (type) => {
    const props = (t.input_schema && t.input_schema.properties) || {};
    const names = Object.entries(props).filter(([, p]) => !type || p.type === type).map(([n]) => [`input.${n}`, n]);
    return [["", "Choose a parameter"], ...names];
  };
  const render = () => {
    const items = t.args || [];
    const head = el("div", "rowline args head", el("span", "", "Passed"), el("span", "", "Flag"), el("span", "", "Value"), el("span"));
    const rows = items.map((item, i) => {
      const kind = argKind(item);
      const kindSelect = choice(ARG_KINDS, kind, (v) => {
        const flag = typeof item === "string" ? "" : item.flag || "";
        if (v === "always") items[i] = typeof item === "string" ? item : item.value || "";
        else if (v === "value") items[i] = { flag, value: typeof item === "string" ? item : item.value || "" };
        else if (v === "when") items[i] = { flag, when: "" };
        else items[i] = { flag, each: "" };
        render();
      });
      kindSelect.setAttribute("aria-label", "When it's passed");
      let flagInput = el("span", "hint", "");
      let valueInput;
      if (kind === "always") {
        valueInput = control("input", item, (v) => { items[i] = v; }, { placeholder: "{{input.name}} or text", "aria-label": "Argument" });
      } else {
        flagInput = control("input", item.flag, (v) => setField(item, "flag", v.trim()), { placeholder: "--flag", "aria-label": "Flag" });
        if (kind === "value") {
          valueInput = control("input", item.value, (v) => setField(item, "value", v), { placeholder: "{{input.name}}", "aria-label": "Value" });
        } else if (kind === "when") {
          valueInput = choice(fieldOptions("boolean"), item.when, (v) => { item.when = v; });
          valueInput.setAttribute("aria-label", "Parameter");
        } else {
          const list = choice(fieldOptions("array"), item.each, (v) => { item.each = v; });
          list.setAttribute("aria-label", "List parameter");
          const each = control("input", item.value, (v) => setField(item, "value", v), { placeholder: "{{item}}", "aria-label": "Value for each item" });
          valueInput = el("div", "with-button", list, each);
        }
      }
      const remove = button("×", "remove", () => {
        items.splice(i, 1);
        setField(t, "args", items);
        changed();
        render();
      }, { "aria-label": "Remove argument" });
      return el("div", "rowline args", kindSelect, flagInput, valueInput, remove);
    });
    const add = button("Add argument", "quiet add", () => {
      t.args = t.args || [];
      t.args.push({ flag: "", value: "" });
      changed();
      render();
    });
    wrap.replaceChildren(...(rows.length ? [head, ...rows] : []), el("div", "", add));
  };
  render();
  return section("More arguments",
    el("p", "hint", "Added after the fixed arguments, in this order. Optional parameters go here: “If given” is left out when the caller doesn't send a value, “If true” adds just the flag, and “For each item” repeats for every item in a list."),
    wrap);
}

function resultSection(t) {
  const result = t.result || {};
  const from = t.result ? (result.from || "stdout") : "";
  const details = el("div");
  const render = () => {
    if (!t.result) {
      details.replaceChildren();
      return;
    }
    const r = t.result;
    const format = choice([["text", "Text"], ["json", "JSON"]], r.format || "text", (v) => { r.format = v; render(); });
    const parts = [el("div", "pair",
      field("Format", format),
      r.format === "json"
        ? field("JSON field", control("input", r.field, (v) => setField(r, "field", v.trim()), { placeholder: "result" }), "Return just this field. Leave empty for all of it.")
        : field("Keep the last lines", control("input", r.tail_lines, (v) => setField(r, "tail_lines", v ? Number(v) : undefined), { type: "number", min: "0", placeholder: "All" }), "Useful for long build and test logs."),
    )];
    if (r.from === "file") {
      parts.unshift(field("File", control("input", r.path, (v) => setField(r, "path", v.trim()), { placeholder: "result.json" }), "A path inside the job's folder ({{workdir}})."));
    }
    details.replaceChildren(...parts);
  };
  const fromSelect = choice([["", "Nothing"], ["stdout", "What it prints"], ["file", "A file it writes"]], from, (v) => {
    if (!v) delete t.result;
    else {
      t.result = t.result || {};
      t.result.from = v;
      if (v !== "file") delete t.result.path;
    }
    render();
  });
  render();
  return section("Result",
    field("Return", fromSelect, "What the agent gets back. Either way, it also gets the exit code and the last 64 KB of output."),
    details,
    field("Exit codes that mean it worked", control("input", (t.success_exit_codes || []).join(", "), (v) => {
      setField(t, "success_exit_codes", v.split(",").map((s) => s.trim()).filter(Boolean).map(Number).filter(Number.isInteger));
    }, { placeholder: "0" }), "Separated by commas. Any other code fails the job. xcodebuild, for one, exits with 65 when tests fail."),
  );
}

function outputsSection(t) {
  const wrap = el("div", "rows");
  const render = () => {
    const outputs = t.outputs || {};
    const head = el("div", "rowline outputs head", el("span", "", "Name"), el("span", "", "Path in the job's folder"), el("span", "", "Folder"), el("span", "", "Optional"), el("span"));
    const rows = Object.entries(outputs).map(([name, spec]) => {
      const obj = typeof spec === "string" ? { path: spec } : { ...spec };
      const save = () => {
        const current = t.outputs || {};
        current[name] = !obj.archive && !obj.optional ? obj.path : Object.fromEntries(Object.entries(obj).filter(([, v]) => v !== "" && v !== false && v !== undefined));
        t.outputs = current;
      };
      const nameInput = control("input", name, () => {}, { "aria-label": "Output name" });
      nameInput.addEventListener("change", () => {
        const next = nameInput.value.trim();
        if (!next || next === name || next in (t.outputs || {})) {
          nameInput.value = name;
          return;
        }
        const renamed = {};
        for (const [k, v] of Object.entries(t.outputs)) renamed[k === name ? next : k] = v;
        t.outputs = renamed;
        changed();
        render();
      });
      const path = control("input", obj.path, (v) => { obj.path = v.trim(); save(); }, { "aria-label": "Path", placeholder: "out.wav" });
      const zip = el("input");
      zip.type = "checkbox";
      zip.checked = obj.archive === "zip";
      zip.title = "It's a folder: send it as a zip";
      zip.setAttribute("aria-label", "Send a folder as zip");
      zip.addEventListener("change", () => { obj.archive = zip.checked ? "zip" : ""; save(); changed(); });
      const optional = el("input");
      optional.type = "checkbox";
      optional.checked = Boolean(obj.optional);
      optional.title = "Don't fail if the program didn't make it";
      optional.setAttribute("aria-label", "Optional");
      optional.addEventListener("change", () => { obj.optional = optional.checked; save(); changed(); });
      const remove = button("×", "remove", () => {
        delete t.outputs[name];
        setField(t, "outputs", t.outputs);
        changed();
        render();
      }, { "aria-label": `Remove ${name}` });
      return el("div", "rowline outputs", nameInput, path, el("span", "", zip), el("span", "", optional), remove);
    });
    const add = button("Add file", "quiet add", () => {
      t.outputs = t.outputs || {};
      let n = 1;
      while (`file${n}` in t.outputs) n++;
      t.outputs[`file${n}`] = "";
      changed();
      render();
    });
    wrap.replaceChildren(...(rows.length ? [head, ...rows] : []), el("div", "", add));
  };
  render();
  return section("Files it makes",
    el("p", "hint", "Files the program writes in the job's folder ({{workdir}}), sent back to the agent. Tick Folder to send a whole folder as a zip."),
    wrap);
}

function runningSection(t) {
  const check = t.check || {};
  const setCheck = (key, value) => {
    t.check = t.check || {};
    setField(t.check, key, value);
    setField(t, "check", t.check);
  };
  return section("Running",
    el("div", "pair",
      field("Time limit", control("input", t.timeout, (v) => setField(t, "timeout", v.trim()), { placeholder: "30m" }), "Such as 90s, 10m or 2h."),
      field("Progress", control("input", t.progress && t.progress.regex, (v) => setField(t, "progress", v ? { regex: v } : undefined), { placeholder: "(?P<percent>\\d+)%", class: "code" }), "A regular expression for progress lines, with groups named percent, current, total or message."),
    ),
    el("div", "field", checkbox(t.retry_if_lost, (on) => setField(t, "retry_if_lost", on || undefined), "Run it again elsewhere if this machine drops out mid-job"),
      el("p", "hint", "Only for tools that are safe to run twice.")),
    el("div", "field", el("span", "label", "Uses"), kvRows(t, "requires", "kv", "resource", "units", true),
      el("p", "hint", "Units of this machine's resources (node.resources in worker.yaml) a job holds, so heavy jobs don't overlap.")),
    el("div", "pair",
      field("Ready check", control("textarea", (check.command || []).join("\n"), (v) => setCheck("command", lines(v)), { rows: 2, class: "code", placeholder: "/path/to/program\n--version" }), "A command that exits with 0 when the tool can work, one argument per line. Without one, the tool is ready if its program exists."),
      field("Check every", control("input", check.every, (v) => setCheck("every", v.trim()), { placeholder: "30s" })),
    ),
  );
}

function testSection(t) {
  const text = t.test && t.test.input !== undefined ? JSON.stringify(t.test.input, null, 2) : "";
  const area = control("textarea", text, (v) => {
    if (!v.trim()) {
      delete t.test;
      area.setCustomValidity("");
      return;
    }
    try {
      t.test = { input: JSON.parse(v) };
      area.setCustomValidity("");
    } catch {
      area.setCustomValidity("Not valid JSON");
    }
  }, { rows: 4, class: "code", placeholder: '{ "text": "hello" }' });
  return section("Test",
    field("Test input", area, "Input that's safe to run again and again. The gateway's “fleet test” runs the tool with it, as a real job."),
    el("div", "row", button("Use in Try it", "quiet", () => {
      if (t.test) $("try-input").value = JSON.stringify(t.test.input, null, 2);
    })),
  );
}

function defaultInput(tool) {
  if (!tool) return {};
  if (tool.test && tool.test.input !== undefined) return tool.test.input;
  const out = {};
  const schema = tool.input_schema || {};
  for (const name of schema.required || []) {
    const prop = (schema.properties || {})[name] || {};
    out[name] = { string: "", integer: 0, number: 0, boolean: false, array: [] }[prop.type] ?? "";
  }
  return out;
}

// --- Finding programs and folders ---

async function loadProgramList() {
  if (detected) return;
  detected = { programs: [] };
  try {
    detected = await api("GET", "api/detect");
  } catch {
    return;
  }
  let list = $("programs");
  if (!list) {
    list = el("datalist");
    list.id = "programs";
    document.body.append(list);
  }
  list.replaceChildren(...detected.programs.map((p) => {
    const option = el("option");
    option.value = p.path;
    option.label = p.name;
    return option;
  }));
}

const browser = { mode: "file", path: "", onPick: null };

function browse(mode, start, onPick) {
  browser.mode = mode;
  browser.onPick = onPick;
  $("browse-title").textContent = mode === "file" ? "Choose a program" : "Choose a folder";
  $("browse-pick-folder").hidden = mode !== "folder";
  $("browse-error").hidden = true;
  let dir = start || "";
  if (mode === "file" && dir) dir = dir.replace(/[\\/][^\\/]*$/, "") || "";
  $("browse-dialog").showModal();
  listFolder(dir);
}

async function listFolder(path) {
  const query = new URLSearchParams({ path, hidden: $("browse-hidden").checked ? "1" : "0" });
  let r;
  try {
    r = await api("GET", `api/browse?${query}`);
  } catch (err) {
    $("browse-error").textContent = err.message;
    $("browse-error").hidden = false;
    return;
  }
  $("browse-error").hidden = true;
  browser.path = r.path;
  browser.parent = r.parent;
  browser.sep = r.separator;
  $("browse-path").value = r.path;
  $("browse-up").disabled = !r.parent;
  const join = (name) => (r.path.endsWith(r.separator) ? r.path + name : r.path + r.separator + name);
  $("browse-list").replaceChildren(...r.entries.map((e) => {
    const cls = e.dir ? "dir" : e.exec ? "exe" : "plain";
    const b = button(e.name, cls, () => {
      if (e.dir) listFolder(join(e.name));
      else if (browser.mode === "file") {
        $("browse-dialog").close();
        browser.onPick(join(e.name));
      }
    });
    if (!e.dir && browser.mode === "folder") b.disabled = true;
    return el("li", "", b);
  }));
}

$("browse-go").addEventListener("submit", (event) => {
  event.preventDefault();
  listFolder($("browse-path").value.trim());
});
$("browse-up").addEventListener("click", () => listFolder(browser.parent));
$("browse-hidden").addEventListener("change", () => listFolder(browser.path));
$("browse-pick-folder").addEventListener("click", () => {
  $("browse-dialog").close();
  browser.onPick(browser.path);
});

// --- Try it ---

function tryInput() {
  const text = $("try-input").value.trim();
  if (!text) return {};
  return JSON.parse(text);
}

function showTryProblem(message) {
  $("try-problem").textContent = message;
  $("try-problem").hidden = !message;
}

async function tryRequest() {
  let input;
  try {
    input = tryInput();
  } catch {
    throw new Error("The input isn't valid JSON.");
  }
  return { yaml: await currentYAML(), file: ed.file, input };
}

$("try-preview").addEventListener("click", async () => {
  showTryProblem("");
  try {
    const r = await api("POST", "api/preview", await tryRequest());
    if (r.problem) {
      showTryProblem(r.problem);
      $("try-call").hidden = true;
      return;
    }
    $("try-call").replaceChildren(el("h4", "", "It would run"), ...callView(r.call));
    $("try-call").hidden = false;
  } catch (err) {
    if (err.status !== 401) showTryProblem(err.message);
  }
});

function callView(call) {
  const parts = [];
  if (call.argv) {
    parts.push(el("pre", "out", call.argv.map(shellQuote).join(" ")));
    const facts = el("dl", "facts");
    const add = (k, v) => { if (v) facts.append(el("dt", "", k), el("dd", "", v)); };
    if (call.program && call.program !== call.argv[0]) add("Program", call.program);
    add("In folder", call.cwd);
    if (call.env) add("Environment", Object.keys(call.env).join(", "));
    if (facts.childElementCount) parts.push(facts);
    if (call.stdin) parts.push(el("h4", "", "Standard input"), el("pre", "out", call.stdin));
  } else if (call.url) {
    parts.push(el("pre", "out", `${call.method || "POST"} ${call.url}`));
    if (call.body) parts.push(el("h4", "", "Body"), el("pre", "out", call.body));
  }
  return parts;
}

$("try-run").addEventListener("click", async () => {
  showTryProblem("");
  stopTry();
  let req;
  try {
    req = await tryRequest();
    const r = await api("POST", "api/try", req);
    ed.tryID = r.id;
  } catch (err) {
    if (err.status !== 401) showTryProblem(err.message);
    return;
  }
  $("try-call").hidden = true;
  $("try-run").disabled = true;
  $("try-cancel").hidden = false;
  pollTry();
});

$("try-cancel").addEventListener("click", async () => {
  if (!ed.tryID) return;
  try { await api("DELETE", `api/try/${ed.tryID}`); } catch { /* It may have finished. */ }
});

function stopTry() {
  clearTimeout(ed.tryTimer);
  ed.tryID = "";
  $("try-run").disabled = false;
  $("try-cancel").hidden = true;
}

async function pollTry() {
  const id = ed.tryID;
  if (!id) return;
  let r;
  try {
    r = await api("GET", `api/try/${id}`);
  } catch (err) {
    if (err.status !== 401) showTryProblem(err.message);
    stopTry();
    return;
  }
  if (id !== ed.tryID) return;
  renderTry(r);
  if (r.state === "running") {
    ed.tryTimer = setTimeout(pollTry, TRY_MS);
  } else {
    $("try-run").disabled = false;
    $("try-cancel").hidden = true;
  }
}

function renderTry(r) {
  const box = $("try-result");
  const outputPre = box.querySelector(".try-output");
  const atBottom = !outputPre || outputPre.scrollTop + outputPre.clientHeight >= outputPre.scrollHeight - 8;
  const parts = [el("div", "try-head", badge(r.state), el("span", "hint", `${r.seconds.toFixed(1)}s`))];
  if (r.progress) {
    const p = r.progress;
    if (typeof p.percent === "number") {
      const bar = el("div", "meter");
      const fill = el("span");
      fill.style.width = `${Math.min(100, Math.max(0, p.percent))}%`;
      bar.append(fill);
      bar.setAttribute("role", "img");
      bar.setAttribute("aria-label", `${Math.round(p.percent)}%`);
      parts.push(bar);
    }
    const text = [p.stage !== "running" ? p.stage : "", p.current && p.total ? `${p.current} of ${p.total}` : p.current, p.message].filter(Boolean).join(" · ");
    if (text) parts.push(el("p", "hint", text));
  }
  if (r.failure) parts.push(el("p", "problem", `${r.failure.code}: ${r.failure.message}`));
  if (r.exit_code !== undefined && r.exit_code !== null) parts.push(el("p", "hint", `Exit code ${r.exit_code}`));
  if (r.result !== undefined && r.result !== null) {
    const text = typeof r.result === "string" ? r.result : JSON.stringify(r.result, null, 2);
    parts.push(el("h4", "", "Result"), el("pre", "out", text));
  }
  // Files are listed once, when the try ends, so previews load only once.
  const shown = box.dataset.files === r.id;
  if (r.files.length && r.state !== "running") {
    parts.push(el("h4", "", "Files"), shown ? box.querySelector(".files") : filesView(r));
    box.dataset.files = r.id;
  }
  parts.push(el("h4", "", "Output"));
  const out = el("pre", "out try-output", r.output || (r.state === "running" ? "Waiting for output…" : "It printed nothing."));
  parts.push(out);
  if (r.call && (r.call.argv || r.call.url)) parts.push(el("h4", "", "Command"), ...callView(r.call));
  box.replaceChildren(...parts);
  box.hidden = false;
  if (atBottom) out.scrollTop = out.scrollHeight;
}

function filesView(r) {
  for (const url of ed.mediaURLs) URL.revokeObjectURL(url);
  ed.mediaURLs = [];
  const list = el("ul", "files");
  for (const f of r.files) {
    const path = `api/try/${r.id}/files/${encodeURIComponent(f.name)}`;
    const ext = f.name.split(".").pop().toLowerCase();
    const type = MEDIA[ext];
    const item = el("li", "", el("div", "file-line",
      el("code", "", f.name),
      el("span", "size", size(f.size)),
      button("Download", "quiet", () => download(path, f.name))));
    if (type && f.size <= 200 * 1024 * 1024) {
      fetchBlob(path).then((blob) => {
        const url = URL.createObjectURL(new Blob([blob], { type }));
        ed.mediaURLs.push(url);
        let media;
        if (type.startsWith("image/")) {
          media = el("img");
          media.alt = f.name;
        } else {
          media = el(type.startsWith("audio/") ? "audio" : "video");
          media.controls = true;
        }
        media.src = url;
        item.append(media);
      }).catch(() => {});
    }
    list.append(item);
  }
  return list;
}

async function download(path, name) {
  try {
    const blob = await fetchBlob(path);
    const url = URL.createObjectURL(blob);
    const a = el("a");
    a.href = url;
    a.download = name;
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 60000);
  } catch (err) {
    toast(err.message);
  }
}

// --- Activity ---

function renderRunning() {
  const wrap = $("running");
  if (!status.running.length) {
    wrap.replaceChildren(el("p", "empty", "Nothing is running."));
    return;
  }
  const rows = status.running.map((r) => el("tr", "",
    el("td", "", el("span", "tool-name", r.capability)),
    el("td", "", el("code", "", r.job)),
    el("td", "", ago(r.started))));
  const head = el("thead", "", el("tr", "", el("th", "", "Tool"), el("th", "", "Job"), el("th", "", "Started")));
  wrap.replaceChildren(el("table", "list", head, el("tbody", "", ...rows)));
}

async function refreshLogs() {
  if (status) renderRunning();
  let r;
  try {
    r = await api("GET", `api/logs?after=${logAfter}`);
  } catch {
    return;
  }
  if (!r.lines.length) return;
  const log = $("log");
  const follow = $("log-follow").checked;
  for (const line of r.lines) {
    const cls = line.text.includes("level=ERROR") ? "error" : line.text.includes("level=WARN") ? "warn" : "";
    log.append(el("span", cls, `${line.text}\n`));
    logAfter = line.seq;
  }
  while (log.childElementCount > MAX_LOG_LINES) log.firstElementChild.remove();
  if (follow) log.scrollTop = log.scrollHeight;
}

// --- Connection ---

const pair = { open: false, waitingForCode: false, busy: false };

function pairMethod() {
  return document.querySelector('input[name="pair-method"]:checked').value;
}

function pairProblem(message) {
  $("pair-problem").textContent = message || "";
  $("pair-problem").hidden = !message;
}

function renderConnection() {
  if (!status) return;
  const state = connectionState();
  const sentences = {
    connected: `Connected to ${gatewayHost()} as ${status.node}.`,
    disconnected: `Not connected to ${gatewayHost()}.`,
    rejected: `${gatewayHost()} no longer knows this machine.`,
    unpaired: "This machine isn't connected to a gateway yet.",
  };
  $("conn-state").replaceChildren(badge(state, sentences[state]));
  const details = {
    connected: `Since ${new Date(status.connected_since).toLocaleString()}. Agents can use this machine's tools through the gateway.`,
    disconnected: `The worker keeps trying by itself. ${status.connection_error ? `The last try failed: ${status.connection_error}` : "It's connecting now."}`,
    rejected: "Its token was revoked or the gateway was reinstalled. Pair it again below.",
    unpaired: "Enter the gateway's address below to pair it. Until then, its tools can be set up and tried here, but agents can't reach them.",
  };
  $("conn-detail").textContent = details[state];
  const mustPair = state === "unpaired" || state === "rejected";
  $("conn-actions").hidden = mustPair || pair.open;
  $("pair-form").hidden = !(mustPair || pair.open);
  $("pair-cancel-row").hidden = mustPair;
  if (!$("pair-gateway").value && status.gateway && document.activeElement !== $("pair-gateway")) {
    $("pair-gateway").value = status.gateway;
  }
  const method = pairMethod();
  $("pair-code-ask").hidden = method !== "code" || pair.waitingForCode;
  $("pair-code-enter").hidden = method !== "code" || !pair.waitingForCode;
  $("pair-key-pane").hidden = method !== "key";
  $("mcp-test").disabled = !status.paired || mcp.running;
  if (status.paired) {
    const url = `${status.gateway}/mcp`;
    $("mcp-url").textContent = url;
    $("mcp-command").textContent = `claude mcp add --transport http fleet ${url} --header "Authorization: Bearer YOUR_API_KEY"`;
  }
  $("mcp-howto").hidden = !status.paired;
}

async function pairRequest(buttonID, busyText, run) {
  if (pair.busy) return;
  const b = $(buttonID);
  const text = b.textContent;
  pair.busy = true;
  b.disabled = true;
  b.textContent = busyText;
  pairProblem("");
  try {
    await run();
  } catch (err) {
    if (err.status !== 401) pairProblem(err.message);
  } finally {
    pair.busy = false;
    b.disabled = false;
    b.textContent = text;
  }
}

function startPairing() {
  return pairRequest("pair-start", "Asking the gateway…", async () => {
    const r = await api("POST", "api/pair/start", { gateway: $("pair-gateway").value, name: "" });
    $("pair-gateway").value = r.gateway;
    $("pair-name").textContent = r.name;
    $("pair-code").value = "";
    pair.waitingForCode = true;
    renderConnection();
    $("pair-code").focus();
  });
}

async function paired(r) {
  pair.open = false;
  pair.waitingForCode = false;
  $("pair-code").value = "";
  $("pair-key").value = "";
  toast(`Paired with ${new URL(r.gateway).host} as ${r.node}`);
  await refreshStatus();
}

$("pair-start").addEventListener("click", startPairing);
$("pair-again").addEventListener("click", () => {
  pair.waitingForCode = false;
  pairProblem("");
  renderConnection();
  startPairing();
});
$("pair-confirm").addEventListener("click", () => pairRequest("pair-confirm", "Pairing…", async () => {
  try {
    await paired(await api("POST", "api/pair/confirm", { code: $("pair-code").value }));
  } catch (err) {
    if (err.status === 410 || err.status === 409) {
      pair.waitingForCode = false;
      renderConnection();
    }
    throw err;
  }
}));
$("pair-key-go").addEventListener("click", () => pairRequest("pair-key-go", "Connecting…", async () => {
  await paired(await api("POST", "api/pair/key", { gateway: $("pair-gateway").value, key: $("pair-key").value }));
}));
$("pair-gateway").addEventListener("keydown", (event) => {
  if (event.key === "Enter") if (pairMethod() === "code") $("pair-start").click(); else $("pair-key").focus();
});
$("pair-gateway").addEventListener("input", () => {
  // A code belongs to the gateway it was asked from.
  if (pair.waitingForCode) {
    pair.waitingForCode = false;
    renderConnection();
  }
});
$("pair-code").addEventListener("keydown", (event) => { if (event.key === "Enter") $("pair-confirm").click(); });
$("pair-key").addEventListener("keydown", (event) => { if (event.key === "Enter") $("pair-key-go").click(); });
for (const radio of document.querySelectorAll('input[name="pair-method"]')) {
  radio.addEventListener("change", () => { pairProblem(""); renderConnection(); });
}
$("conn-change").addEventListener("click", () => {
  pair.open = true;
  renderConnection();
  $("pair-gateway").focus();
});
$("pair-cancel").addEventListener("click", () => {
  pair.open = false;
  pair.waitingForCode = false;
  pairProblem("");
  renderConnection();
});

const mcp = { running: false };

$("mcp-test").addEventListener("click", async () => {
  if (mcp.running) return;
  mcp.running = true;
  const b = $("mcp-test");
  b.disabled = true;
  b.textContent = "Testing…";
  $("mcp-verdict").replaceChildren();
  $("mcp-steps").replaceChildren();
  try {
    const r = await api("POST", "api/mcp-test", {});
    $("mcp-steps").replaceChildren(...r.steps.map((s) => el("li", "",
      el("div", "step-state", badge(s.state), el("div", "step-time", s.state === "skipped" ? "" : `${s.ms} ms`)),
      el("div", "step-name", s.name),
      el("div", "step-detail", s.detail || ""))));
    $("mcp-verdict").replaceChildren(r.ok
      ? badge("ok", "Agents can reach this machine through MCP")
      : badge("failed", "The test didn't pass"));
  } catch (err) {
    if (err.status !== 401) $("mcp-verdict").replaceChildren(badge("failed", err.message));
  } finally {
    mcp.running = false;
    b.textContent = "Run the test again";
    b.disabled = !status || !status.paired;
  }
});

// --- This machine ---

function renderFacts() {
  if (!status) return;
  const facts = [
    ["Machine", status.node],
    ["Gateway", status.gateway || "Not paired yet"],
    ["Connection", status.connected ? `Connected since ${new Date(status.connected_since).toLocaleString()}` : "Not connected"],
    ["Jobs at once", String(status.max_jobs)],
    ["Settings file", status.config_file],
    ["Tools folder", status.tools_dir],
    ["State folder", status.state_dir],
    ["Platform", status.platform],
    ["Worker version", status.version],
  ];
  $("facts").replaceChildren(...facts.flatMap(([k, v]) => [el("dt", "", k), el("dd", "", v || "—")]));
}

function renderMachine() {
  renderFacts();
}

$("detect").addEventListener("click", async () => {
  const b = $("detect");
  b.disabled = true;
  b.textContent = "Looking…";
  try {
    const r = await api("GET", "api/detect?simulators=1");
    detected = r;
    const parts = [];
    const rows = r.programs.map((p) => el("tr", "",
      el("td", "", el("strong", "", p.name)),
      el("td", "", el("code", "", p.path)),
      el("td", "hint", p.on_path ? "Found by name too" : "Needs the full path")));
    parts.push(el("div", "table-wrap", el("table", "list",
      el("thead", "", el("tr", "", el("th", "", "Program"), el("th", "", "Path"), el("th", "", ""))),
      el("tbody", "", ...rows))));
    if (r.simulators) {
      parts.push(el("h2", "", "iPhone and iPad simulators"));
      if (r.simulators_error) parts.push(el("p", "problem", r.simulators_error));
      const sims = r.simulators.map((s) => el("tr", "",
        el("td", "", s.name), el("td", "", s.runtime), el("td", "", el("code", "", s.udid)), el("td", "hint", s.state)));
      if (sims.length) {
        parts.push(el("div", "table-wrap", el("table", "list",
          el("thead", "", el("tr", "", el("th", "", "Device"), el("th", "", "System"), el("th", "", "ID"), el("th", "", "State"))),
          el("tbody", "", ...sims))));
      }
    }
    $("detected").replaceChildren(...parts);
  } catch (err) {
    if (err.status !== 401) toast(err.message);
  } finally {
    b.disabled = false;
    b.textContent = "Find programs";
  }
});

start();
