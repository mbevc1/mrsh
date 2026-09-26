"use strict";

// The launch URL carries the session token in the fragment, which never
// reaches the server logs or Referer headers. Move it to sessionStorage and
// clear it from the address bar.
const token = (() => {
  const m = location.hash.match(/token=([0-9a-f]+)/);
  if (m) {
    sessionStorage.setItem("mrsh-token", m[1]);
    history.replaceState(null, "", location.pathname);
    return m[1];
  }
  return sessionStorage.getItem("mrsh-token") || "";
})();

const $ = (sel, root = document) => root.querySelector(sel);
const state = { version: "", cfg: null, editing: null };

class APIError extends Error {
  constructor(status, body) {
    super(body.error || `HTTP ${status}`);
    this.status = status;
    this.problems = body.errors || [];
  }
}

async function api(method, path, body) {
  const headers = { "X-Mrsh-Token": token };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  // Every write carries the version this page loaded, so a stale tab
  // cannot overwrite a newer change.
  if (method !== "GET" && path !== "/api/validate") headers["If-Match"] = state.version;
  const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new APIError(res.status, data);
  if (data.version) state.version = data.version;
  return data;
}

// banner shows a lasting warning or error; toast confirms a finished action.
function banner(msg) {
  const b = $("#banner");
  b.textContent = msg || "";
  b.hidden = !msg;
}

let toastTimer;
function toast(msg) {
  const t = $("#toast");
  t.textContent = msg;
  t.hidden = false;
  t.classList.remove("show");
  void t.offsetWidth; // restart the fade-in when toasts follow each other
  t.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 2500);
}

const clock = () => new Date().toLocaleTimeString();

// busy disables btn and shows label while fn runs. On loopback most calls
// finish in a few milliseconds, so the state stays up for at least 200 ms
// to be noticeable.
async function busy(btn, label, fn) {
  const text = btn.textContent;
  btn.disabled = true;
  btn.textContent = label;
  btn.setAttribute("aria-busy", "true");
  const started = performance.now();
  try {
    return await fn();
  } finally {
    const wait = 200 - (performance.now() - started);
    if (wait > 0) await new Promise((r) => setTimeout(r, wait));
    btn.disabled = false;
    btn.textContent = text;
    btn.removeAttribute("aria-busy");
  }
}

function showErrors(list, err) {
  list.replaceChildren();
  const items = err instanceof APIError && err.problems.length ? err.problems : [err.message || String(err)];
  for (const p of items) {
    const li = document.createElement("li");
    li.textContent = p;
    list.append(li);
  }
}

// load fetches the config and renders it; it reports whether that worked.
async function load() {
  try {
    state.cfg = await api("GET", "/api/config");
    state.version = state.cfg.version;
    banner("");
    render();
    return true;
  } catch (err) {
    banner(err.status === 401
      ? "This page has no valid session token. Open the URL printed by `mrsh ui` in your terminal."
      : `Could not load config: ${err.message}`);
    return false;
  }
}

// reload says whether anything changed, so an unchanged reload is still visible.
async function reload() {
  const before = state.version;
  const ok = await busy($("#reload"), "Reloading…", load);
  if (ok) toast(state.version === before ? `Reloaded at ${clock()}: no changes` : `Reloaded at ${clock()}: config changed`);
}

// On a version conflict, reload and tell the user what happened.
async function onConflict(err) {
  if (err.status === 412) {
    await load();
    banner("The config changed elsewhere (another tab or the CLI). Reloaded the latest version; please redo your change.");
    return true;
  }
  return false;
}

function secretText(s) {
  switch (s.kind) {
    case "plain": return s.value || "********"; // passwords arrive without a value
    case "env": return `env:${s.value}`;
    case "arn": return s.value;
    default: return "";
  }
}

function cell(text, cls) {
  const td = document.createElement("td");
  td.textContent = text;
  if (cls) td.className = cls;
  return td;
}

function render() {
  const cfg = state.cfg;
  $("#location").textContent = cfg.location;

  const filter = $("#group-filter");
  const current = filter.value;
  filter.replaceChildren(new Option("All groups", ""));
  const groups = $("#groups");
  groups.replaceChildren();
  for (const g of cfg.groups) {
    filter.append(new Option(g, g));
    groups.append(new Option(g));
  }
  filter.value = cfg.groups.includes(current) ? current : "";

  renderHosts();
  renderDefaults();
  $("#commands").value = cfg.commands.join("\n");
}

function renderHosts() {
  const q = $("#search").value.trim().toLowerCase();
  const group = $("#group-filter").value;
  const rows = state.cfg.hosts.filter((h) =>
    (!group || h.group === group) &&
    (!q || [h.name, h.host, h.group].some((v) => v.toLowerCase().includes(q))));

  const tbody = $("#hosts");
  tbody.replaceChildren();
  for (const h of rows) {
    const tr = document.createElement("tr");
    tr.append(
      cell(h.name), cell(h.host), cell(h.port ? String(h.port) : "default", h.port ? "num" : "muted"),
      cell(h.group || "-", h.group ? "" : "muted"),
      cell(secretText(h.user) || "default", "ref"), cell(secretText(h.pass) || "default", "ref"),
      cell(h.identity_file || "default", h.identity_file ? "ref" : "muted"),
    );
    const actions = document.createElement("td");
    actions.className = "actions";
    const edit = document.createElement("button");
    edit.type = "button"; edit.className = "link"; edit.textContent = "Edit";
    edit.addEventListener("click", () => openEditor(h));
    const del = document.createElement("button");
    del.type = "button"; del.className = "link danger"; del.textContent = "Delete";
    del.addEventListener("click", () => removeHost(h, del));
    actions.append(edit, del);
    tr.append(actions);
    tbody.append(tr);
  }
  $("#count").textContent = `${rows.length} of ${state.cfg.hosts.length} hosts`;
  $("#empty").hidden = state.cfg.hosts.length > 0;
}

// Secret editors: a source select plus a value input per field-group.
function setupSecret(fs) {
  const field = fs.dataset.secret;
  fs.append($("#secret-template").content.cloneNode(true));
  $("legend", fs).textContent = field === "pass" ? "Password" : "User";
  const kind = $(".kind", fs), value = $(".value", fs), hint = $(".secret-hint", fs);
  const sync = () => {
    const k = kind.value, stored = fs.dataset.stored === "true";
    value.hidden = k === "unset";
    value.type = k === "plain" && field === "pass" ? "password" : "text";
    value.placeholder = { plain: stored ? "(unchanged)" : "value", env: "VARIABLE_NAME",
      arn: "arn:aws:ssm:region:account:parameter/name" }[k] || "";
    hint.textContent = k === "plain" && stored ? "A literal is stored. Leave empty to keep it."
      : k === "unset" ? (fs.closest("#defaults-form") ? "Not set: hosts rely on keys or the SSH agent." : "Falls back to defaults, then the SSH agent.") : "";
  };
  kind.addEventListener("change", () => { value.value = ""; sync(); });
  fs.sync = sync;
}

function fillSecret(fs, s) {
  $(".kind", fs).value = s.kind || "unset";
  $(".value", fs).value = s.value || "";
  // Only the masked password has a hidden stored literal to keep.
  fs.dataset.stored = s.kind === "plain" && s.set && !s.value ? "true" : "false";
  fs.sync();
}

function readSecret(fs) {
  return { kind: $(".kind", fs).value, value: $(".value", fs).value };
}

function openEditor(h) {
  state.editing = h ? h.name : null;
  const form = $("#host-form");
  form.reset();
  $("#editor-title").textContent = h ? `Edit ${h.name}` : "Add host";
  const v = h || { name: "", host: "", group: "", port: 0, identity_file: "", known_hosts: "", user: { kind: "unset" }, pass: { kind: "unset" } };
  for (const k of ["name", "host", "group", "identity_file", "known_hosts"]) form.elements[k].value = v[k] || "";
  form.elements.port.value = v.port || "";
  fillSecret($('[data-secret="user"]', form), v.user);
  fillSecret($('[data-secret="pass"]', form), v.pass);
  $("#host-errors").replaceChildren();
  $("#editor").showModal();
  form.elements.name.focus();
}

function readHost() {
  const f = $("#host-form").elements;
  return {
    name: f.name.value.trim(), host: f.host.value.trim(), group: f.group.value.trim(),
    port: Number(f.port.value) || 0, identity_file: f.identity_file.value.trim(), known_hosts: f.known_hosts.value.trim(),
    user: readSecret($('#host-form [data-secret="user"]')), pass: readSecret($('#host-form [data-secret="pass"]')),
  };
}

let validateTimer;
function liveValidate() {
  clearTimeout(validateTimer);
  validateTimer = setTimeout(async () => {
    const host = readHost();
    if (!host.name || !host.host) return;
    try {
      const res = await api("POST", "/api/validate", { original: state.editing || "", host });
      const list = $("#host-errors");
      list.replaceChildren(...res.errors.map((e) => Object.assign(document.createElement("li"), { textContent: e })));
    } catch { /* surfaced on save */ }
  }, 300);
}

async function saveHost(ev) {
  ev.preventDefault();
  const host = readHost();
  const editing = state.editing;
  await busy($("#save"), "Saving…", async () => {
    try {
      if (editing) await api("PUT", `/api/hosts/${encodeURIComponent(editing)}`, host);
      else await api("POST", "/api/hosts", host);
      $("#editor").close();
      await load();
      toast(editing ? `Saved ${host.name}` : `Added ${host.name}`);
    } catch (err) {
      if (await onConflict(err)) { $("#editor").close(); return; }
      showErrors($("#host-errors"), err);
    }
  });
}

async function removeHost(h, btn) {
  if (!confirm(`Remove host ${h.name} (${h.host})?`)) return;
  await busy(btn, "Removing…", async () => {
    try {
      await api("DELETE", `/api/hosts/${encodeURIComponent(h.name)}`);
      await load();
      toast(`Removed ${h.name}`);
    } catch (err) {
      if (!(await onConflict(err))) banner(`Could not remove ${h.name}: ${err.message}`);
    }
  });
}

function renderDefaults() {
  const d = state.cfg.defaults, f = $("#defaults-form").elements;
  for (const k of ["port", "timeout", "parallel"]) f[k].value = d[k] || "";
  for (const k of ["output", "host_key_policy", "identity_file", "known_hosts"]) f[k].value = d[k] || "";
  f.debug.checked = !!d.debug;
  fillSecret($('#defaults-form [data-secret="user"]'), d.user);
  fillSecret($('#defaults-form [data-secret="pass"]'), d.pass);
}

async function saveDefaults(ev) {
  ev.preventDefault();
  const f = $("#defaults-form").elements;
  const body = {
    port: Number(f.port.value) || 0, timeout: Number(f.timeout.value) || 0, parallel: Number(f.parallel.value) || 0,
    output: f.output.value, host_key_policy: f.host_key_policy.value, debug: f.debug.checked,
    identity_file: f.identity_file.value.trim(), known_hosts: f.known_hosts.value.trim(),
    user: readSecret($('#defaults-form [data-secret="user"]')), pass: readSecret($('#defaults-form [data-secret="pass"]')),
  };
  await busy(ev.submitter || $("#defaults-form button[type=submit]"), "Saving…", async () => {
    try {
      await api("PUT", "/api/defaults", body);
      $("#defaults-errors").replaceChildren();
      await load();
      toast("Defaults saved");
    } catch (err) {
      if (!(await onConflict(err))) showErrors($("#defaults-errors"), err);
    }
  });
}

async function saveCommands(ev) {
  ev.preventDefault();
  const cmds = $("#commands").value.split("\n").map((s) => s.trim()).filter(Boolean);
  await busy(ev.submitter || $("#commands-form button[type=submit]"), "Saving…", async () => {
    try {
      await api("PUT", "/api/commands", cmds);
      $("#commands-errors").replaceChildren();
      await load();
      toast(`Commands saved (${cmds.length})`);
    } catch (err) {
      if (!(await onConflict(err))) showErrors($("#commands-errors"), err);
    }
  });
}

function selectTab(name) {
  for (const b of document.querySelectorAll(".tabs button")) b.setAttribute("aria-selected", String(b.dataset.tab === name));
  for (const t of ["hosts", "defaults", "commands"]) $(`#tab-${t}`).hidden = t !== name;
}

let heartbeatTimer;

// Browsers only let scripts close tabs they opened, so a tab opened from the
// terminal shows a "stopped" page instead. No unsaved-edit check is needed:
// the host editor is modal, so Exit can't be clicked mid-edit.
async function exitUI() {
  try {
    await busy($("#exit"), "Exiting…", () => api("POST", "/api/shutdown", {}));
  } catch (err) {
    banner(`Could not stop mrsh ui: ${err.message}`);
    return;
  }
  clearInterval(heartbeatTimer);
  sessionStorage.removeItem("mrsh-token");
  window.close();
  for (const el of ["header", "nav.tabs", "main", "#banner"]) $(el).hidden = true;
  $("#stopped").hidden = false;
  document.title = "mrsh ui stopped";
}

// Tell the user when the mrsh ui process has stopped.
async function heartbeat() {
  try {
    const res = await fetch("/api/health", { headers: { "X-Mrsh-Token": token } });
    if (!res.ok) throw new Error();
    if ($("#banner").dataset.down) { banner(""); delete $("#banner").dataset.down; }
  } catch {
    banner("mrsh ui is not running anymore. Start it again to keep editing.");
    $("#banner").dataset.down = "1";
  }
}

document.addEventListener("DOMContentLoaded", () => {
  document.querySelectorAll("fieldset.secret").forEach(setupSecret);
  document.querySelectorAll(".tabs button").forEach((b) => b.addEventListener("click", () => selectTab(b.dataset.tab)));
  $("#search").addEventListener("input", renderHosts);
  $("#group-filter").addEventListener("change", renderHosts);
  $("#add").addEventListener("click", () => openEditor(null));
  $("#cancel").addEventListener("click", () => $("#editor").close());
  $("#host-form").addEventListener("submit", saveHost);
  $("#host-form").addEventListener("input", liveValidate);
  $("#defaults-form").addEventListener("submit", saveDefaults);
  $("#commands-form").addEventListener("submit", saveCommands);
  $("#reload").addEventListener("click", reload);
  $("#exit").addEventListener("click", exitUI);
  load();
  heartbeatTimer = setInterval(heartbeat, 5000);
});
