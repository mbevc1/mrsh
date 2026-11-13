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
// picked: hosts ticked for upgrade (opt-in, as upgrades reboot devices);
// testSkipped: hosts unticked for testing (opt-out, so all are tested by default).
const state = { version: "", cfg: null, editing: null, tests: {}, upgrades: {}, picked: new Set(), testSkipped: new Set(), hostPicked: new Set(), rotating: false }; // tests: name -> {ok, detail, ms} or {running}

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
  if (method !== "GET" && path !== "/api/validate" && path !== "/api/test" && !path.startsWith("/api/upgrade/")) headers["If-Match"] = state.version;
  const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new APIError(res.status, data);
  if (data.version) state.version = data.version;
  return data;
}

// apiStream posts body and reads an NDJSON reply, handing each line to
// onLine as it arrives; it returns the closing {"results": ...} (or
// {"outcome": ...}) line.
async function apiStream(path, body, onLine) {
  const res = await fetch(path, {
    method: "POST",
    headers: { "X-Mrsh-Token": token, "Content-Type": "application/json" },
    body: JSON.stringify({ ...body, stream: true }),
  });
  if (!res.ok) throw new APIError(res.status, await res.json().catch(() => ({})));
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = "", last = {};
  for (;;) {
    const { value, done } = await reader.read();
    if (value) buf += value;
    let nl;
    while ((nl = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, nl).trim();
      buf = buf.slice(nl + 1);
      if (!line) continue;
      const msg = JSON.parse(line);
      if (msg.error) throw new APIError(res.status, msg);
      if (msg.results || msg.outcome) last = msg;
      else onLine(msg);
    }
    if (done) return last;
  }
}

// banner shows a lasting warning or error; toast confirms a finished action.
function banner(msg) {
  const b = $("#banner");
  b.textContent = msg || "";
  b.hidden = !msg;
}

let toastTimer;
function toast(msg, failed = false) {
  const t = $("#toast");
  t.textContent = msg;
  t.classList.toggle("fail", failed);
  t.hidden = false;
  t.classList.remove("show");
  void t.offsetWidth; // restart the fade-in when toasts follow each other
  t.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 2500);
}

const clock = () => new Date().toLocaleTimeString();

// copyIcon builds the usual copy glyph: a full page in front of the visible
// corner of one behind it. SVG, since CSP forbids inline styles.
function copyIcon() {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("aria-hidden", "true");
  const front = document.createElementNS(ns, "rect");
  for (const [k, v] of Object.entries({ x: 9, y: 9, width: 12, height: 12, rx: 2 })) front.setAttribute(k, v);
  const back = document.createElementNS(ns, "path");
  back.setAttribute("d", "M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1");
  svg.append(front, back);
  return svg;
}

// copyText uses the Clipboard API (loopback counts as a secure context),
// falling back to a hidden textarea for older browsers.
async function copyText(text, what, echo = true) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.className = "offscreen";
    document.body.append(ta);
    ta.select();
    const ok = document.execCommand("copy");
    ta.remove();
    if (!ok) { toast(`Could not copy ${what}`); return; }
  }
  toast(echo ? `Copied ${what}: ${text}` : `Copied ${what}`);
}

function copyButton(text, what, title = `Copy ${what}`) {
  const b = document.createElement("button");
  b.type = "button"; b.className = "icon";
  b.title = title;
  b.setAttribute("aria-label", b.title);
  b.append(copyIcon());
  b.addEventListener("click", () => copyText(text, what));
  return b;
}

// busy disables btn and shows label while fn runs. On loopback most calls
// finish in a few milliseconds, so the state stays up for at least 200 ms
// to be noticeable.
// stackLabels puts a button's idle and busy text in one grid cell, only one
// visible at a time, so the button is always as wide as the longer one and
// never resizes (nor shifts its neighbours) while it is busy.
function stackLabels(btn, busyText) {
  let busyLabel = btn.querySelector(":scope > .busy-label");
  if (!busyLabel) {
    const idle = document.createElement("span");
    idle.className = "idle-label";
    idle.textContent = btn.textContent;
    busyLabel = document.createElement("span");
    busyLabel.className = "busy-label";
    btn.replaceChildren(idle, busyLabel);
    btn.classList.add("stacked");
  }
  busyLabel.textContent = busyText;
  return btn;
}

async function busy(btn, label, fn) {
  stackLabels(btn, label);
  btn.disabled = true;
  btn.setAttribute("aria-busy", "true");
  const started = performance.now();
  try {
    return await fn();
  } finally {
    const wait = 200 - (performance.now() - started);
    if (wait > 0) await new Promise((r) => setTimeout(r, wait));
    btn.disabled = false;
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

const literalTitle = "Literal password: stored in the config file in the clear. Prefer an environment variable or Store in AWS SSM.";

// warnIcon is the warning triangle used by the literal-password marks.
function warnIcon() {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("aria-hidden", "true");
  for (const d of ["M12 3 2 21h20L12 3z", "M12 10v5", "M12 18h.01"]) {
    const p = document.createElementNS(ns, "path");
    p.setAttribute("d", d);
    svg.append(p);
  }
  return svg;
}

// passCell shows a password reference, marking a literal one like the CLI's warning.
function passCell(pass, fallback) {
  if (pass.kind !== "plain") return fallback;
  const td = document.createElement("td");
  td.className = "ref";
  const mark = document.createElement("span");
  mark.className = "literal";
  mark.title = literalTitle;
  mark.setAttribute("aria-label", "literal password, stored in the clear");
  mark.append(warnIcon(), "********");
  td.append(mark);
  return td;
}

// trackCovered marks a table box "covered" while some of its columns are
// out of view to the right, under the pinned row actions: when the table is
// wider than the box and not scrolled to its end. Size changes (a re-render,
// a resize, a tab shown) and scrolling both update it.
function trackCovered(wrap) {
  const update = () => wrap.classList.toggle("covered", wrap.scrollLeft + wrap.clientWidth < wrap.scrollWidth - 1);
  wrap.addEventListener("scroll", update, { passive: true });
  const ro = new ResizeObserver(update);
  ro.observe(wrap);
  ro.observe(wrap.querySelector("table"));
}

function cell(text, cls) {
  const td = document.createElement("td");
  td.textContent = text;
  if (cls) td.className = cls;
  return td;
}

const shellQuote = (s) => /^[\w@%+=:,./~-]+$/.test(s) ? s : `'${s.replaceAll("'", `'\\''`)}'`;

// sshCommand builds the ssh invocation from the host's effective settings.
// A user held in an env var or ARN is unresolved here, so it is left out;
// passwords never appear (ssh takes none on the command line).
function sshCommand(h, d) {
  // Same order as the CLI: host, host's set, defaults, defaults' set.
  const set = (name) => state.cfg.credentials.find((c) => c.name === name) || { user: { kind: "unset" }, identity_file: "" };
  const sources = [h, set(h.credential), d, set(d.credential)];
  const user = sources.map((x) => x.user).find((u) => u.kind !== "unset") || { kind: "unset" };
  const port = h.port || d.port || 22;
  const identity = sources.map((x) => x.identity_file).find(Boolean);
  const args = ["ssh"];
  if (port !== 22) args.push("-p", String(port));
  if (identity) args.push("-i", shellQuote(identity));
  args.push(user.kind === "plain" && user.value ? `${shellQuote(user.value)}@${h.host}` : h.host);
  return { cmd: args.join(" "), userLeftOut: user.kind === "env" || user.kind === "arn" };
}

function hostCell(h) {
  const td = cell(h.host);
  const { cmd, userLeftOut } = sshCommand(h, state.cfg.defaults);
  td.append(copyButton(cmd, `ssh command for ${h.name}`,
    userLeftOut ? "Copy ssh command (user comes from an env var or ARN, so it is left out)" : "Copy ssh command"));
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

  const testGroup = $("#test-group");
  const testCurrent = testGroup.value;
  testGroup.replaceChildren(new Option("All groups", ""), ...cfg.groups.map((g) => new Option(g, g)));
  testGroup.value = cfg.groups.includes(testCurrent) ? testCurrent : "";
  const upgradeGroup = $("#upgrade-group");
  const upgradeCurrent = upgradeGroup.value;
  upgradeGroup.replaceChildren(new Option("All groups", ""), ...cfg.groups.map((g) => new Option(g, g)));
  upgradeGroup.value = cfg.groups.includes(upgradeCurrent) ? upgradeCurrent : "";
  const names = new Set(cfg.hosts.map((h) => h.name));
  for (const n of Object.keys(state.tests)) if (!names.has(n)) delete state.tests[n];
  for (const n of Object.keys(state.upgrades)) if (!names.has(n)) delete state.upgrades[n];
  for (const n of state.picked) if (!names.has(n)) state.picked.delete(n);
  for (const n of state.hostPicked) if (!names.has(n)) state.hostPicked.delete(n);
  for (const n of state.testSkipped) if (!names.has(n)) state.testSkipped.delete(n);

  renderCredentials(); // first: fills the credential dropdowns the editors read
  renderHosts();
  renderTests();
  renderUpgrades();
  renderDefaults();
  $("#commands").value = cfg.commands.join("\n");
}

// statusBadge shows a host's last result from results (tests or upgrades),
// or nothing before one.
function statusBadge(name, results = state.tests) {
  const r = results[name];
  const span = document.createElement("span");
  if (!r) return span;
  if (r.running) {
    span.className = "badge running"; span.textContent = r.label || "testing…";
    span.title = span.textContent;
  } else if (r.ok && (r.pending || (r.action && r.action !== "none"))) {
    // Firmware pending (test), or an upgrade step available (dry run) or under
    // way: not the same as "up to date".
    span.className = "badge update";
    span.textContent = `↑ ${r.summary}`;
    span.title = r.detail;
  } else {
    span.className = r.ok ? "badge ok" : "badge fail";
    span.textContent = `${r.ok ? "✓" : "✗"} ${r.summary}`;
    span.title = r.detail; // the full message, e.g. with the address and wrapping context
  }
  return span;
}

function inherited(own, credential, cls) {
  if (own) return cell(own, cls);
  return cell(credential ? `set: ${credential}` : "default", "muted");
}

function statusCell(name, results = state.tests) {
  const td = document.createElement("td");
  td.className = "status";
  td.append(statusBadge(name, results));
  return td;
}

const fmtMs = (r) => {
  if (!r || r.running || !(r.ms > 0)) return "";
  if (r.ms < 1000) return `${r.ms} ms`;
  if (r.ms < 60000) return `${(r.ms / 1000).toFixed(1)} s`;
  return `${Math.floor(r.ms / 60000)}m ${Math.round((r.ms % 60000) / 1000)}s`; // upgrades that wait through reboots
};

function upgradeNames() {
  const group = $("#upgrade-group").value;
  return state.cfg.hosts.filter((h) => !group || h.group === group).map((h) => h.name);
}

function renderUpgrades() {
  const shown = upgradeNames();
  const tbody = $("#upgrade-rows");
  tbody.replaceChildren();
  for (const h of state.cfg.hosts.filter((h) => shown.includes(h.name))) {
    const r = state.upgrades[h.name];
    const pick = pickCell(h.name, state.picked.has(h.name), (on) => {
      if (on) state.picked.add(h.name); else state.picked.delete(h.name);
      renderUpgrades();
    });
    const tr = document.createElement("tr");
    tr.append(pick, cell(h.name), cell(h.host), cell(h.group || "-", h.group ? "" : "muted"),
      r ? statusCell(h.name, state.upgrades) : cell("", "status"), cell(fmtMs(r), "num"));
    tbody.append(tr);
  }
  const picked = shown.filter((n) => state.picked.has(n));
  syncSelection($("#upgrade-all"), $("#upgrade-count"), [$("#upgrade-run"), $("#upgrade-dry")], picked.length, shown.length);
}

// runUpgrades runs the `mt upgrade` step (or its dry run) on the ticked hosts.
async function runUpgrades(apply) {
  const names = upgradeNames().filter((n) => state.picked.has(n));
  if (!names.length) return;
  const wait = apply && $("#upgrade-wait").checked;
  const how = wait ? "Each takes every step, rebooting after each one; this can take several minutes per device"
    : "Each installs updates and reboots";
  if (apply && !confirm(`Upgrade ${names.length} device(s)? ${how}:\n\n${names.join("\n")}`)) return;
  const btn = apply ? $("#upgrade-run") : $("#upgrade-dry");
  for (const n of names) state.upgrades[n] = { running: true, label: apply ? "upgrading…" : "checking…" };
  renderUpgrades();
  await busy(btn, apply ? "Upgrading…" : "Checking…", async () => {
    try {
      // Each device's row updates as soon as the server reports on it.
      const show = (r) => {
        state.upgrades[r.name] = r.running ? { running: true, label: r.summary }
          : { ok: r.ok, summary: r.summary || r.detail, detail: r.detail, ms: r.duration_ms, action: r.action };
      };
      const { results = [] } = await apiStream(apply ? "/api/upgrade/apply" : "/api/upgrade/dry",
        { hosts: names, confirm: apply, wait }, (r) => { show(r); renderUpgrades(); });
      results.forEach(show);
      const failed = results.filter((r) => !r.ok).length;
      // With wait, a device that took steps ends up to date; count the steps instead.
      const acted = results.filter((r) => r.ok && (wait ? r.steps?.length : r.action !== "none")).length;
      const verb = wait ? "upgraded" : apply ? "upgrading" : "would upgrade";
      toast(`${acted} ${verb}, ${results.length - acted - failed} up to date, ${failed} failed`, failed > 0);
    } catch (err) {
      for (const n of names) if (state.upgrades[n]?.running) delete state.upgrades[n]; // keep finished rows
      banner(`${apply ? "Upgrade" : "Dry run"} failed: ${err.message}`);
    }
  });
  renderUpgrades();
}

// runTests asks the server to connect to the named saved hosts.
async function runTests(names, btn) {
  if (!names.length) return;
  for (const n of names) state.tests[n] = { running: true };
  renderHosts(); renderTests();
  await busy(btn, "Testing…", async () => {
    try {
      // Each host's row fills in as soon as it answers or fails.
      const show = (r) => {
        state.tests[r.name] = { ok: r.ok, pending: r.pending, summary: r.summary || r.detail, detail: r.detail, ms: r.duration_ms };
      };
      const { results = [] } = await apiStream("/api/test", { hosts: names }, (r) => { show(r); renderHosts(); renderTests(); });
      results.forEach(show);
      const ok = results.filter((r) => r.ok).length;
      toast(results.length === 1 ? `${results[0].name}: ${results[0].summary || results[0].detail}` : `${ok} of ${results.length} hosts OK`,
        ok < results.length);
    } catch (err) {
      for (const n of names) if (state.tests[n]?.running) delete state.tests[n]; // keep finished rows
      banner(`Test failed: ${err.message}`);
    }
  });
  renderHosts(); renderTests();
}

// pickCell is a row's selection checkbox; onChange gets the new state.
function pickCell(name, checked, onChange) {
  const pick = document.createElement("input");
  pick.type = "checkbox"; pick.checked = checked;
  pick.setAttribute("aria-label", `Select ${name}`);
  pick.addEventListener("change", () => onChange(pick.checked));
  const td = document.createElement("td");
  td.className = "check";
  td.append(pick);
  return td;
}

// syncSelection updates a select-all box, count label and action buttons.
function syncSelection(allBox, count, buttons, selected, shown) {
  allBox.checked = shown > 0 && selected === shown;
  allBox.indeterminate = selected > 0 && selected < shown;
  count.textContent = `${selected} of ${shown} selected`;
  for (const b of buttons) b.disabled = selected === 0;
}

function selectedTests() {
  return testNames().filter((n) => !state.testSkipped.has(n));
}

function testNames() {
  const group = $("#test-group").value;
  return state.cfg.hosts.filter((h) => !group || h.group === group).map((h) => h.name);
}

function renderTests() {
  const names = new Set(testNames());
  const tbody = $("#test-rows");
  tbody.replaceChildren();
  for (const h of state.cfg.hosts.filter((h) => names.has(h.name))) {
    const r = state.tests[h.name];
    const result = r ? statusCell(h.name) : cell("not tested", "status muted");
    // A host that failed before connecting (e.g. an unset env var) has no time.
    const ms = fmtMs(r);
    const tr = document.createElement("tr");
    const pick = pickCell(h.name, !state.testSkipped.has(h.name), (on) => {
      if (on) state.testSkipped.delete(h.name); else state.testSkipped.add(h.name);
      renderTests();
    });
    tr.append(pick, cell(h.name), cell(h.host), cell(h.group || "-", h.group ? "" : "muted"), result, cell(ms, "num"));
    tbody.append(tr);
  }
  syncSelection($("#test-all"), $("#test-count"), [$("#test-run")], selectedTests().length, names.size);
}

// shownHosts is the Hosts tab's rows under its search and group filter.
function shownHosts() {
  const q = $("#search").value.trim().toLowerCase();
  const group = $("#group-filter").value;
  return state.cfg.hosts.filter((h) =>
    (!group || h.group === group) &&
    (!q || [h.name, h.host, h.group].some((v) => v.toLowerCase().includes(q))));
}

// pickedHosts is what the bulk buttons act on: ticked hosts still shown.
const pickedHosts = () => shownHosts().filter((h) => state.hostPicked.has(h.name));

function renderHosts() {
  const rows = shownHosts();

  const tbody = $("#hosts");
  tbody.replaceChildren();
  for (const h of rows) {
    const tr = document.createElement("tr");
    tr.append(
      pickCell(h.name, state.hostPicked.has(h.name), (on) => {
        if (on) state.hostPicked.add(h.name); else state.hostPicked.delete(h.name);
        renderHosts();
      }),
      cell(h.name), hostCell(h), cell(h.port ? String(h.port) : "default", h.port ? "num" : "muted"),
      cell(h.group || "-", h.group ? "" : "muted"),
      // Unset fields come from the host's credential set (shown as "set: NAME"), else Defaults.
      inherited(secretText(h.user), h.credential, "ref"), passCell(h.pass, inherited(secretText(h.pass), h.credential, "ref")),
      inherited(h.identity_file, h.credential, "ref"),
      statusCell(h.name),
    );
    const actions = document.createElement("td");
    actions.className = "actions";
    const test = document.createElement("button");
    test.type = "button"; test.className = "link"; test.textContent = "Test";
    test.title = "Connect and run a read-only version check";
    test.addEventListener("click", () => runTests([h.name], test));
    stackLabels(test, "Testing…");
    const edit = document.createElement("button");
    edit.type = "button"; edit.className = "link"; edit.textContent = "Edit";
    edit.addEventListener("click", () => openEditor(h));
    const del = document.createElement("button");
    del.type = "button"; del.className = "link danger"; del.textContent = "Delete";
    del.addEventListener("click", () => removeHost(h, del));
    stackLabels(del, "Removing…");
    // Only a host's own password; one from a credential set rotates with the set.
    actions.append(test, edit, rotateButton("host", h.name, h.pass.kind !== "unset"), del);
    tr.append(actions);
    tbody.append(tr);
  }
  $("#count").textContent = `${rows.length} of ${state.cfg.hosts.length} hosts`;
  syncSelection($("#hosts-all"), $("#hosts-picked"), [$("#bulk-edit"), $("#bulk-delete")], pickedHosts().length, rows.length);
  $("#empty").hidden = state.cfg.hosts.length > 0;
}

// Secret editors: a source select plus a value input per field-group.
function setupSecret(fs) {
  const field = fs.dataset.secret;
  fs.append($("#secret-template").content.cloneNode(true));
  $("legend", fs).textContent = field === "pass" ? "Password" : "User";
  const kind = $(".kind", fs), value = $(".value", fs), hint = $(".secret-hint", fs);
  const ssmRow = $(".ssm-row", fs), ssmName = $(".ssm-name", fs);
  const sync = () => {
    const k = kind.value, stored = fs.dataset.stored === "true";
    value.hidden = k === "unset";
    value.type = (k === "plain" || k === "ssm") && field === "pass" ? "password" : "text";
    value.placeholder = { plain: stored ? "(unchanged)" : "value", env: "VARIABLE_NAME",
      arn: "arn:aws:ssm:region:account:parameter/name", ssm: "value to store" }[k] || "";
    ssmRow.hidden = k !== "ssm";
    ssmName.placeholder = `Parameter, default ${defaultSSMName(fs, field)}`;
    hint.classList.toggle("warn", k === "plain" && field === "pass");
    hint.textContent = k === "plain" && field === "pass"
      ? `Stored in the config file in the clear; prefer an environment variable or Store in AWS SSM.${stored ? " Leave empty to keep the current one." : ""}`
      : k === "ssm" ? "Stored as an encrypted SecureString when you save; the config keeps only its ARN."
      : k !== "unset" ? ""
      : fs.closest("#defaults-form") ? "Not set: falls back to the Defaults credential set, then keys or the SSH agent."
      : fs.closest("#cred-form") ? "Not set: hosts using this set fall back to Defaults."
      : "Falls back to the credential set, then Defaults, then the SSH agent.";
  };
  kind.addEventListener("change", () => { value.value = ""; ssmName.value = ""; $(".ssm-overwrite", fs).checked = false; sync(); });
  // The default parameter name follows the entry's name as it is typed.
  fs.closest("form")?.elements.name?.addEventListener("input", sync);
  fs.sync = sync;
}

// defaultSSMName mirrors the server's default: <prefix>/<creds|hosts>/<name>/<field>.
function defaultSSMName(fs, field) {
  const prefix = (state.cfg && state.cfg.defaults.ssm_prefix) || "/mrsh";
  const form = fs.closest("form");
  if (form.id === "defaults-form") return `${prefix}/defaults/${field}`;
  const owner = form.id === "cred-form" ? "creds" : "hosts";
  const name = (form.elements.name && form.elements.name.value.trim()) || "NAME";
  return `${prefix}/${owner}/${name}/${field}`;
}

function fillSecret(fs, s) {
  $(".kind", fs).value = s.kind || "unset";
  $(".value", fs).value = s.value || "";
  // Only the masked password has a hidden stored literal to keep.
  fs.dataset.stored = s.kind === "plain" && s.set && !s.value ? "true" : "false";
  fs.sync();
}

function readSecret(fs) {
  const s = { kind: $(".kind", fs).value, value: $(".value", fs).value };
  if (s.kind === "ssm") {
    s.name = $(".ssm-name", fs).value.trim();
    s.overwrite = $(".ssm-overwrite", fs).checked;
  }
  return s;
}

function openEditor(h) {
  state.editing = h ? h.name : null;
  const form = $("#host-form");
  form.reset();
  $("#editor-title").textContent = h ? `Edit ${h.name}` : "Add host";
  const v = h || { name: "", host: "", group: "", port: 0, identity_file: "", known_hosts: "", user: { kind: "unset" }, pass: { kind: "unset" } };
  for (const k of ["name", "host", "group", "credential", "identity_file", "known_hosts"]) form.elements[k].value = v[k] || "";
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
    name: f.name.value.trim(), host: f.host.value.trim(), group: f.group.value.trim(), credential: f.credential.value,
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
      delete state.tests[host.name]; // an earlier result no longer describes this entry
      if (editing) delete state.tests[editing];
      await load();
      toast(editing ? `Saved ${host.name}` : `Added ${host.name}`);
    } catch (err) {
      if (await onConflict(err)) { $("#editor").close(); return; }
      showErrors($("#host-errors"), err);
    }
  });
}

// askConfirm shows the in-page confirmation, optionally with one checkbox,
// and resolves to {ok, checked}.
function askConfirm({ title, text, ok, option }) {
  const dlg = $("#confirm-dialog");
  $("#confirm-title").textContent = title;
  $("#confirm-text").textContent = text;
  $("#confirm-ok").textContent = ok;
  $("#confirm-option").hidden = !option;
  $("#confirm-checkbox").checked = false;
  $("#confirm-option-text").textContent = option || "";
  dlg.returnValue = "cancel";
  dlg.showModal();
  return new Promise((resolve) => dlg.addEventListener("close",
    () => resolve({ ok: dlg.returnValue === "ok", checked: $("#confirm-checkbox").checked }), { once: true }));
}

// ownSSM counts the SSM parameters an entry's own user and pass point at.
const ownSSM = (e) => [e.user, e.pass].filter((s) => s && s.kind === "arn" && s.value.includes(":ssm:")).length;

// removedNote turns a removal's "secrets" report into toast text, and puts
// anything that went wrong in the banner.
function removedNote(what, reply) {
  const rep = reply && reply.secrets;
  if (!rep) return `Removed ${what}`;
  if (rep.errors.length) banner(`Removed ${what}, but deleting from SSM failed: ${rep.errors.join("; ")}`);
  const kept = rep.kept.length ? ` (kept ${rep.kept.length}: ${rep.kept.join("; ")})` : "";
  return `Removed ${what}; deleted ${rep.deleted.length} SSM parameter(s)${kept}`;
}

// openBulkEditor starts every field as "leave as is" (unticked, disabled).
function openBulkEditor() {
  const hosts = pickedHosts();
  if (!hosts.length) return;
  const form = $("#bulk-form");
  form.reset();
  for (const box of form.querySelectorAll("[data-change]")) setChange(box, false);
  fillSecret($('#bulk-form [data-secret="user"]'), { kind: "unset" });
  fillSecret($('#bulk-form [data-secret="pass"]'), { kind: "unset" });
  $("#bulk-title").textContent = `Edit ${hosts.length} host${hosts.length === 1 ? "" : "s"}`;
  $("#bulk-hosts").textContent = hosts.map((h) => h.name).join(", ");
  $("#bulk-errors").replaceChildren();
  $("#bulk-editor").showModal();
}

// setChange ticks or unticks a bulk field and enables its control to match.
function setChange(box, on) {
  box.checked = on;
  const target = box.closest(".bulk-field").querySelector("input:not([data-change]), select, fieldset");
  target.disabled = !on;
  return target;
}

async function saveBulk(ev) {
  ev.preventDefault();
  const hosts = pickedHosts().map((h) => h.name);
  const f = $("#bulk-form").elements;
  const changes = {};
  for (const box of $("#bulk-form").querySelectorAll("[data-change]:checked")) {
    const k = box.dataset.change;
    if (k === "user" || k === "pass") changes[k] = readSecret($(`#bulk-form [data-secret="${k}"]`));
    else if (k === "port") changes.port = Number(f.port.value) || 0;
    else changes[k] = f[k].value.trim();
  }
  if (!Object.keys(changes).length) {
    showErrors($("#bulk-errors"), new Error("Tick at least one field to change."));
    return;
  }
  await busy($("#bulk-save"), "Saving…", async () => {
    try {
      await api("POST", "/api/hosts/bulk-edit", { hosts, changes });
      $("#bulk-editor").close();
      for (const n of hosts) delete state.tests[n]; // earlier results no longer describe them
      await load();
      toast(`Updated ${hosts.length} host${hosts.length === 1 ? "" : "s"}`);
    } catch (err) {
      if (await onConflict(err)) { $("#bulk-editor").close(); return; }
      showErrors($("#bulk-errors"), err);
    }
  });
}

async function removeHosts() {
  const hosts = pickedHosts();
  if (!hosts.length) return;
  const names = hosts.map((h) => h.name);
  const n = hosts.length, own = hosts.some((h) => ownSSM(h) > 0);
  const answer = await askConfirm({
    title: `Remove ${n} host${n === 1 ? "" : "s"}?`, text: `${names.join(", ")} ${n === 1 ? "is" : "are"} removed from the config.`, ok: "Remove",
    option: own ? "Also delete their SSM parameters (only ones under the SSM prefix that nothing left uses)" : "",
  });
  if (!answer.ok) return;
  await busy($("#bulk-delete"), "Removing…", async () => {
    try {
      const reply = await api("POST", "/api/hosts/bulk-delete", { hosts: names, delete_secret: answer.checked });
      for (const name of names) state.hostPicked.delete(name);
      await load();
      toast(removedNote(n === 1 ? names[0] : `${n} hosts`, reply));
    } catch (err) {
      if (!(await onConflict(err))) banner(`Could not remove ${n === 1 ? names[0] : `${n} hosts`}: ${err.message}`);
    }
  });
}

async function removeHost(h, btn) {
  const n = ownSSM(h);
  const answer = await askConfirm({
    title: `Remove host ${h.name}?`, text: `${h.name} (${h.host}) is removed from the config.`, ok: "Remove",
    option: n ? `Also delete its SSM parameter${n > 1 ? "s" : ""} (only ones under the SSM prefix that nothing else uses)` : "",
  });
  if (!answer.ok) return;
  await busy(btn, "Removing…", async () => {
    try {
      const reply = await api("DELETE", `/api/hosts/${encodeURIComponent(h.name)}${answer.checked ? "?delete_secret=1" : ""}`);
      await load();
      toast(removedNote(h.name, reply));
    } catch (err) {
      if (!(await onConflict(err))) banner(`Could not remove ${h.name}: ${err.message}`);
    }
  });
}

function renderDefaults() {
  const d = state.cfg.defaults, f = $("#defaults-form").elements;
  for (const k of ["port", "timeout", "parallel"]) f[k].value = d[k] || "";
  for (const k of ["output", "host_key_policy", "credential", "identity_file", "known_hosts", "ssm_prefix"]) f[k].value = d[k] || "";
  f.debug.checked = !!d.debug;
  fillSecret($('#defaults-form [data-secret="user"]'), d.user);
  fillSecret($('#defaults-form [data-secret="pass"]'), d.pass);
}

async function saveDefaults(ev) {
  ev.preventDefault();
  const f = $("#defaults-form").elements;
  const body = {
    port: Number(f.port.value) || 0, timeout: Number(f.timeout.value) || 0, parallel: Number(f.parallel.value) || 0,
    output: f.output.value, host_key_policy: f.host_key_policy.value, debug: f.debug.checked, credential: f.credential.value,
    identity_file: f.identity_file.value.trim(), known_hosts: f.known_hosts.value.trim(), ssm_prefix: f.ssm_prefix.value.trim(),
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

// Credential sets.
function renderCredentials() {
  const creds = state.cfg.credentials;
  for (const sel of document.querySelectorAll(".cred-select")) {
    const keep = sel.value;
    const none = sel.options[0];
    sel.replaceChildren(none, ...creds.map((c) => new Option(c.name, c.name)));
    sel.value = creds.some((c) => c.name === keep) ? keep : "";
  }
  const tbody = $("#creds");
  tbody.replaceChildren();
  for (const c of creds) {
    const tr = document.createElement("tr");
    const used = c.used_by.length ? c.used_by.join(", ") : "unused";
    tr.append(cell(c.name), cell(secretText(c.user) || "-", "ref"), passCell(c.pass, cell(secretText(c.pass) || "-", "ref")),
      cell(c.identity_file || "-", c.identity_file ? "ref" : "muted"), cell(used, c.used_by.length ? "" : "muted"));
    const actions = document.createElement("td");
    actions.className = "actions";
    const edit = document.createElement("button");
    edit.type = "button"; edit.className = "link"; edit.textContent = "Edit";
    edit.addEventListener("click", () => openCredEditor(c));
    const del = document.createElement("button");
    del.type = "button"; del.className = "link danger"; del.textContent = "Delete";
    if (c.used_by.length) { del.disabled = true; del.title = `Used by ${used}; point them at another set first`; }
    del.addEventListener("click", () => removeCredential(c, del));
    stackLabels(del, "Removing…");
    actions.append(edit, rotateButton("credential", c.name, c.pass.kind !== "unset"), del);
    tr.append(actions);
    tbody.append(tr);
  }
  $("#cred-count").textContent = `${creds.length} set${creds.length === 1 ? "" : "s"}`;
  $("#cred-empty").hidden = creds.length > 0;
}

function openCredEditor(c) {
  state.editingCred = c ? c.name : null;
  const form = $("#cred-form");
  form.reset();
  $("#cred-title").textContent = c ? `Edit credential ${c.name}` : "Add credential";
  form.elements.name.value = c ? c.name : "";
  form.elements.name.readOnly = !!c; // hosts reference sets by name
  form.elements.identity_file.value = c ? c.identity_file : "";
  fillSecret($('[data-secret="user"]', form), c ? c.user : { kind: "unset" });
  fillSecret($('[data-secret="pass"]', form), c ? c.pass : { kind: "unset" });
  $("#cred-errors").replaceChildren();
  $("#cred-editor").showModal();
  (c ? form.elements.identity_file : form.elements.name).focus();
}

async function saveCredential(ev) {
  ev.preventDefault();
  const f = $("#cred-form").elements;
  const body = {
    name: f.name.value.trim(), identity_file: f.identity_file.value.trim(),
    user: readSecret($('#cred-form [data-secret="user"]')), pass: readSecret($('#cred-form [data-secret="pass"]')),
  };
  const editing = state.editingCred;
  await busy($("#cred-save"), "Saving…", async () => {
    try {
      if (editing) await api("PUT", `/api/credentials/${encodeURIComponent(editing)}`, body);
      else await api("POST", "/api/credentials", body);
      $("#cred-editor").close();
      await load();
      toast(editing ? `Saved credential ${body.name}` : `Added credential ${body.name}`);
    } catch (err) {
      if (await onConflict(err)) { $("#cred-editor").close(); return; }
      showErrors($("#cred-errors"), err);
    }
  });
}

async function removeCredential(c, btn) {
  const n = ownSSM(c);
  const answer = await askConfirm({
    title: `Remove credential ${c.name}?`, text: `The credential set ${c.name} is removed from the config.`, ok: "Remove",
    option: n ? `Also delete its SSM parameter${n > 1 ? "s" : ""} (only ones under the SSM prefix that nothing else uses)` : "",
  });
  if (!answer.ok) return;
  await busy(btn, "Removing…", async () => {
    try {
      const reply = await api("DELETE", `/api/credentials/${encodeURIComponent(c.name)}${answer.checked ? "?delete_secret=1" : ""}`);
      await load();
      toast(removedNote(`credential ${c.name}`, reply));
    } catch (err) {
      if (!(await onConflict(err))) banner(`Could not remove ${c.name}: ${err.message}`);
    }
  });
}

// rotateButton is always built, so every row's buttons line up; without a
// password of its own to rotate it is an invisible, inert placeholder.
// showOnce puts the new password in the result dialog. It is never kept:
// closing the dialog clears it, and the server has no way to show it again.
function showOnce(password, plan) {
  $("#rotate-password").textContent = password;
  $("#rotate-secret-note").textContent = plan.store === "env"
    ? `Set ${plan.dest} to it before the next mrsh run; nothing else keeps it. It is not shown again after you close this window.`
    : "It is not shown again after you close this window.";
  $("#rotate-secret").hidden = false;
}

function rotateButton(kind, name, available) {
  const b = document.createElement("button");
  b.type = "button"; b.className = "link"; b.textContent = "Rotate";
  stackLabels(b, "Rotating…");
  if (!available) {
    b.classList.add("placeholder");
    b.disabled = true;
    return b;
  }
  b.title = "Set a new random password on its RouterOS devices, kept where the current one is";
  b.addEventListener("click", () => rotatePass(kind, name, b));
  return b;
}

// rotatePass asks the server what a rotation would change, confirms it,
// then runs it with each device's step shown as it happens. The dialog
// stays open until the rotation ends: the server finishes it either way.
async function rotatePass(kind, name, btn) {
  let plan;
  try {
    plan = await busy(btn, "Rotating…", () => api("POST", "/api/rotate/plan", { kind, name }));
  } catch (err) {
    banner(`Cannot rotate the password of ${kind} ${name}: ${err.message}`);
    return;
  }
  const n = plan.hosts.length;
  const answer = await askConfirm({
    title: `Rotate the password of ${plan.label}?`,
    text: `A new random password is set on ${n} RouterOS device${n === 1 ? "" : "s"} (${plan.hosts.join(", ")}) and ` +
      { ssm: `replaces the one in SSM parameter ${plan.dest}.`, literal: "replaces the literal in the config.",
        env: `shown to you: set the environment variable ${plan.dest} to it yourself (this mrsh ui uses it until it stops).` }[plan.store] +
      " If any device fails, the others are set back to the old password. The new password is shown once.",
    ok: "Rotate",
  });
  if (!answer.ok) return;

  const dlg = $("#rotate-dialog"), list = $("#rotate-list"), msg = $("#rotate-message"), close = $("#rotate-close");
  const results = {}, rows = {};
  const show = (host) => rows[host] && rows[host].lastChild.replaceWith(statusBadge(host, results));
  $("#rotate-title").textContent = `Rotating the password of ${plan.label}`;
  list.replaceChildren(...plan.hosts.map((host) => {
    results[host] = { running: true, label: "waiting…" };
    const li = document.createElement("li");
    const label = document.createElement("span");
    label.textContent = host; label.title = host;
    li.append(label, statusBadge(host, results));
    rows[host] = li;
    return li;
  }));
  msg.hidden = true; msg.className = "";
  $("#rotate-secret").hidden = true;
  state.rotating = true;
  close.disabled = true;
  dlg.showModal();
  await busy(btn, "Rotating…", async () => {
    let text, failed;
    try {
      const { outcome } = await apiStream("/api/rotate/apply", { kind, name, confirm: true }, (r) => {
        results[r.name] = r.running ? { running: true, label: r.summary } : r;
        show(r.name);
      });
      for (const r of outcome.results) { results[r.name] = r; show(r.name); }
      text = [outcome.message, ...(outcome.notes || []).map((x) => `Note: ${x}`)].join(" ");
      failed = !outcome.ok;
      if (outcome.password) showOnce(outcome.password, plan);
    } catch (err) {
      text = `Rotation failed: ${err.message}`;
      failed = true;
      for (const host of plan.hosts) {
        if (results[host].running) { results[host] = { ok: false, summary: "not changed", detail: err.message }; show(host); }
      }
    }
    msg.textContent = text;
    msg.classList.toggle("fail", failed);
    msg.hidden = false;
    state.rotating = false;
    close.disabled = false;
    close.focus();
    await load();
    if (!failed) toast(`Rotated the password of ${plan.label}`);
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
  for (const b of document.querySelectorAll(".tabs button")) {
    b.setAttribute("aria-selected", String(b.dataset.tab === name));
    if (b.dataset.tab === name) b.scrollIntoView({ block: "nearest", inline: "nearest" }); // a narrow, scrolled tab strip
  }
  for (const t of ["hosts", "credentials", "defaults", "commands", "test", "upgrade"]) $(`#tab-${t}`).hidden = t !== name;
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
  // Reserve each busy label's width before the first click.
  for (const b of document.querySelectorAll("button[data-busy]")) stackLabels(b, b.dataset.busy);
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
  $("#cred-add").addEventListener("click", () => openCredEditor(null));
  $("#cred-cancel").addEventListener("click", () => $("#cred-editor").close());
  $("#cred-form").addEventListener("submit", saveCredential);
  $("#test-group").addEventListener("change", renderTests);
  $("#upgrade-group").addEventListener("change", renderUpgrades);
  $("#upgrade-all").addEventListener("change", (e) => {
    for (const n of upgradeNames()) if (e.target.checked) state.picked.add(n); else state.picked.delete(n);
    renderUpgrades();
  });
  $("#upgrade-dry").addEventListener("click", () => runUpgrades(false));
  $("#upgrade-run").addEventListener("click", () => runUpgrades(true));
  $("#test-all").addEventListener("change", (e) => {
    for (const n of testNames()) if (e.target.checked) state.testSkipped.delete(n); else state.testSkipped.add(n);
    renderTests();
  });
  $("#test-run").addEventListener("click", () => runTests(selectedTests(), $("#test-run")));
  $("#hosts-all").addEventListener("change", (e) => {
    for (const h of shownHosts()) if (e.target.checked) state.hostPicked.add(h.name); else state.hostPicked.delete(h.name);
    renderHosts();
  });
  $("#bulk-edit").addEventListener("click", openBulkEditor);
  $("#bulk-delete").addEventListener("click", removeHosts);
  $("#bulk-cancel").addEventListener("click", () => $("#bulk-editor").close());
  $("#bulk-form").addEventListener("submit", saveBulk);
  for (const box of document.querySelectorAll("#bulk-form [data-change]")) {
    box.addEventListener("change", () => {
      const t = setChange(box, box.checked);
      if (box.checked) { $("#bulk-errors").replaceChildren(); (t.querySelector?.("select") || t).focus(); }
    });
  }
  // Storing in SSM is one host at a time; shared passwords belong in a credential set.
  for (const opt of document.querySelectorAll('#bulk-form option[value="ssm"]')) opt.remove();
  // The rotation's outcome must be seen; Esc does not close it while it runs.
  $("#rotate-dialog").addEventListener("cancel", (e) => { if (state.rotating) e.preventDefault(); });
  $("#rotate-dialog").addEventListener("close", () => { $("#rotate-password").textContent = ""; $("#rotate-secret").hidden = true; });
  $("#rotate-copy").append(copyIcon());
  $("#rotate-copy").addEventListener("click", () => copyText($("#rotate-password").textContent, "the new password", false));
  document.querySelectorAll(".table-wrap").forEach(trackCovered);
  $("#copy-location").append(copyIcon());
  $("#copy-location").addEventListener("click", () => copyText(state.cfg.location, "config location"));
  load();
  heartbeatTimer = setInterval(heartbeat, 5000);
});
