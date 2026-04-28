const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

let currentTitle = null;
let currentInst = null;
let isDragging = false;

// --- Auth -----------------------------------------------------------------

let loginSessionId = null;
let loginWs = null;

async function checkAuth() {
    try {
        const r = await fetch("/api/auth/status");
        if (!r.ok) return;
        const { authed } = await r.json();
        $("#auth-banner").hidden = authed;
    } catch (e) {
        console.error("auth status check failed", e);
    }
}

async function startLogin() {
    $("#login-output").textContent = "";
    $("#login-text").value = "";
    $("#login-dialog").showModal();
    $("#login-text").focus();

    const r = await fetch("/api/auth/login", { method: "POST" });
    if (!r.ok) {
        appendLoginOutput(`\n[error] failed to start login: ${r.status}\n`);
        return;
    }
    const { id } = await r.json();
    loginSessionId = id;

    const proto = location.protocol === "https:" ? "wss" : "ws";
    loginWs = new WebSocket(`${proto}://${location.host}/api/auth/login/${encodeURIComponent(id)}`);
    loginWs.onmessage = (ev) => {
        try {
            const msg = JSON.parse(ev.data);
            if (msg.type === "output") {
                appendLoginOutput(msg.text);
            } else if (msg.type === "done") {
                appendLoginOutput(`\n[login process exited, code ${msg.returncode}]\n`);
                loginWs = null;
                loginSessionId = null;
                setTimeout(async () => {
                    await checkAuth();
                    if ($("#auth-banner").hidden) $("#login-dialog").close();
                }, 200);
            }
        } catch (err) {
            console.error("bad login event", err, ev.data);
        }
    };
    loginWs.onclose = () => { loginWs = null; };
}

function appendLoginOutput(text) {
    const clean = text.replace(/\x1b\[[0-9;?]*[A-Za-z]/g, "");
    const out = $("#login-output");
    const frag = document.createDocumentFragment();
    let idx = 0;
    const re = /(https?:\/\/[^\s]+)/g;
    let m;
    while ((m = re.exec(clean)) !== null) {
        if (m.index > idx) frag.appendChild(document.createTextNode(clean.slice(idx, m.index)));
        const a = document.createElement("a");
        a.href = m[1];
        a.target = "_blank";
        a.rel = "noopener";
        a.textContent = m[1];
        frag.appendChild(a);
        idx = m.index + m[1].length;
    }
    if (idx < clean.length) frag.appendChild(document.createTextNode(clean.slice(idx)));
    out.appendChild(frag);
    out.scrollTop = out.scrollHeight;
}

async function sendLoginInput(data) {
    if (!loginSessionId) {
        appendLoginOutput("\n[no active login session]\n");
        return;
    }
    if (!data) return;
    const r = await fetch(`/api/auth/login/${encodeURIComponent(loginSessionId)}/input`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ data }),
    });
    if (!r.ok) {
        appendLoginOutput(`\n[error] send input failed: ${r.status}\n`);
    }
}

async function submitLoginText() {
    const text = $("#login-text").value;
    $("#login-text").value = "";
    await sendLoginInput(text + "\r");
}

function decodeKeyAttr(attr) {
    if (attr === "\\r" || attr === "\r") return "\r";
    if (attr.startsWith("[") || attr.startsWith("O")) return "\x1b" + attr;
    return attr;
}

async function cancelLogin() {
    if (loginSessionId) {
        await fetch(`/api/auth/login/${encodeURIComponent(loginSessionId)}`, { method: "DELETE" });
        loginSessionId = null;
    }
    if (loginWs) { loginWs.close(); loginWs = null; }
    $("#login-dialog").close();
}

$("#login-btn").addEventListener("click", startLogin);
$("#login-cancel").addEventListener("click", cancelLogin);
$("#login-form").addEventListener("submit", (e) => {
    e.preventDefault();
    submitLoginText();
});
for (const btn of $$(".key-btn")) {
    btn.addEventListener("click", () => sendLoginInput(decodeKeyAttr(btn.dataset.key)));
}

// --- Stream cache ---------------------------------------------------------
//
// One Stream per instance. The WS opens when the instance first appears in
// loadInstances() and stays open until the instance is deleted, regardless
// of which instance the user is viewing. Each stream owns a DOM subtree
// inside #output-area; switching instances shows the right subtree.

const streams = new Map();  // title -> { ws, outputDiv, status, evicting }

function getOrCreateStream(title) {
    let stream = streams.get(title);
    if (stream) return stream;
    const outputDiv = document.createElement("div");
    outputDiv.className = "instance-output";
    outputDiv.dataset.title = title;
    outputDiv.style.display = "none";
    $("#output-area").appendChild(outputDiv);
    stream = {
        ws: null,
        outputDiv,
        status: null,
        evicting: false,
        totals: makeEmptyTotals(),
    };
    streams.set(title, stream);
    openStreamWs(title, stream);
    return stream;
}

function makeEmptyTotals() {
    return {
        cost: 0,
        input_tokens: 0,
        output_tokens: 0,
        cache_read: 0,
        cache_creation: 0,
        turns: 0,
    };
}

function openStreamWs(title, stream) {
    const proto = location.protocol === "https:" ? "wss" : "ws";
    const ws = new WebSocket(`${proto}://${location.host}/api/instances/${encodeURIComponent(title)}/events`);
    stream.ws = ws;
    ws.onmessage = (ev) => {
        try {
            handleStreamEvent(title, JSON.parse(ev.data));
        } catch (err) {
            console.error("bad event", err, ev.data);
        }
    };
    ws.onclose = () => {
        stream.ws = null;
        if (!stream.evicting) appendNoteToStream(stream, "connection closed");
    };
    ws.onerror = () => {
        if (!stream.evicting) appendNoteToStream(stream, "connection error");
    };
}

function evictStream(title) {
    const stream = streams.get(title);
    if (!stream) return;
    stream.evicting = true;
    if (stream.ws) {
        try { stream.ws.close(); } catch {}
        stream.ws = null;
    }
    if (stream.outputDiv.parentElement) {
        stream.outputDiv.parentElement.removeChild(stream.outputDiv);
    }
    streams.delete(title);
}

function attachStreamView(title) {
    for (const [t, s] of streams) {
        s.outputDiv.style.display = (t === title) ? "" : "none";
    }
    const stream = streams.get(title);
    if (stream) {
        // Always scroll to bottom on switch
        const host = $("#output-area");
        host.scrollTop = host.scrollHeight;
    }
}

function handleStreamEvent(title, event) {
    const stream = streams.get(title);
    if (!stream) return;

    if (event.type === "status") {
        stream.status = event.status;
        if (currentTitle === title) {
            if (currentInst) currentInst.status = event.status;
            updateStatusBar(stream);
        }
        if (event.status === "ready" && !hasMeaningfulOutputIn(stream.outputDiv)) {
            appendNoteToStream(stream, "Claude is ready. Send a prompt to get started.");
        }
        renderSidebar();
        return;
    }

    if (event.type === "result") {
        accumulateTotals(stream.totals, event);
        if (currentTitle === title) updateStatusBar(stream);
        // fall through — still render the result event in the turn body
    }

    if (event.type === "user_prompt") {
        const turn = startUserTurn(stream, event);
        if (currentTitle === title) turn.scrollIntoView({ block: "end" });
        return;
    }

    // Everything else nests into the current turn (creating a session turn if
    // no user prompt has happened yet).
    const body = getOrCreateCurrentTurnBody(stream);

    const el = document.createElement("div");
    el.className = `event event-${event.type}`;
    if (defaultOpenForEvent(event.type)) el.classList.add("open");

    const labelEl = makeLabelSpan(labelFor(event), event.ts, el);
    el.appendChild(labelEl);

    const bodyText = (event.type === "system_init")
        ? JSON.stringify(event.data ?? {}, null, 2)
        : bodyFor(event);

    // For tool events, append a one-line preview that's only visible while
    // the event is collapsed.
    if (event.type === "tool_use" || event.type === "tool_result") {
        const preview = document.createElement("span");
        preview.className = "event-preview";
        preview.textContent = shortPreview(bodyText);
        labelEl.appendChild(preview);
    }

    const bodyEl = document.createElement("pre");
    bodyEl.textContent = bodyText;
    el.appendChild(bodyEl);

    body.appendChild(el);
    if (currentTitle === title) el.scrollIntoView({ block: "end" });
}

function shortPreview(text, max = 50) {
    if (typeof text !== "string") text = String(text ?? "");
    const compact = text.replace(/\s+/g, " ").trim();
    if (compact.length <= max) return compact;
    return compact.slice(0, max) + "…";
}

function startUserTurn(stream, event) {
    const turn = document.createElement("div");
    turn.className = "turn turn-user open";

    const header = document.createElement("div");
    header.className = "turn-header";

    header.appendChild(makeToggleButton(turn));
    header.appendChild(makeLabelSpan("user", event.ts));
    const pre = document.createElement("pre");
    pre.textContent = event.text ?? "";
    header.appendChild(pre);

    const body = document.createElement("div");
    body.className = "turn-body";

    turn.appendChild(header);
    turn.appendChild(body);
    stream.outputDiv.appendChild(turn);
    return turn;
}

function makeToggleButton(turn) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "turn-toggle";
    btn.setAttribute("aria-label", "Toggle turn");
    btn.textContent = "▾";
    btn.addEventListener("click", () => {
        turn.classList.toggle("open");
    });
    return btn;
}

function getOrCreateCurrentTurnBody(stream) {
    const last = stream.outputDiv.lastElementChild;
    if (last && last.classList.contains("turn")) {
        return last.querySelector(":scope > .turn-body");
    }
    // No turn yet — synthesize a "session" turn for pre-prompt events.
    const turn = document.createElement("div");
    turn.className = "turn turn-session open";
    const header = document.createElement("div");
    header.className = "turn-header";
    header.appendChild(makeToggleButton(turn));
    header.appendChild(makeLabelSpan("session", null));
    const body = document.createElement("div");
    body.className = "turn-body";
    turn.appendChild(header);
    turn.appendChild(body);
    stream.outputDiv.appendChild(turn);
    return body;
}

function makeLabelSpan(text, ts, toggleTarget = null) {
    const labelEl = document.createElement("span");
    labelEl.className = "label";
    if (toggleTarget) {
        labelEl.appendChild(makeToggleButton(toggleTarget));
    }
    labelEl.appendChild(document.createTextNode(text));
    if (ts) {
        const tsEl = document.createElement("span");
        tsEl.className = "ts";
        tsEl.textContent = formatTimestamp(ts);
        tsEl.title = ts;
        labelEl.appendChild(tsEl);
    }
    return labelEl;
}

function defaultOpenForEvent(type) {
    return type !== "tool_use" && type !== "tool_result" && type !== "system_init";
}

function formatTimestamp(iso) {
    const d = new Date(iso);
    if (isNaN(d.getTime())) return "";
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
}

function hasMeaningfulOutputIn(outputDiv) {
    // "Meaningful" = anything that's a turn (user or session) with content,
    // since notes live inside turns now. Walk one level deep.
    for (const child of outputDiv.children) {
        if (child.classList.contains("turn-user")) return true;
        if (child.classList.contains("turn-session")) {
            const body = child.querySelector(":scope > .turn-body");
            if (body) {
                for (const grand of body.children) {
                    if (!grand.classList.contains("event-note")) return true;
                }
            }
        }
    }
    return false;
}

function appendNoteToStream(stream, text) {
    const body = getOrCreateCurrentTurnBody(stream);
    const el = document.createElement("div");
    el.className = "event event-note";
    el.textContent = text;
    body.appendChild(el);
}

function appendNote(text) {
    if (!currentTitle) return;
    const stream = streams.get(currentTitle);
    if (stream) appendNoteToStream(stream, text);
}

function accumulateTotals(totals, event) {
    if (typeof event.total_cost_usd === "number" && !isNaN(event.total_cost_usd)) {
        totals.cost += event.total_cost_usd;
    }
    // num_turns from each ResultMessage is the cumulative turn count for that
    // receive_response cycle, so take the max rather than summing.
    if (typeof event.num_turns === "number") {
        totals.turns = Math.max(totals.turns, event.num_turns);
    }
    const u = event.usage;
    if (u && typeof u === "object") {
        totals.input_tokens += u.input_tokens || 0;
        totals.output_tokens += u.output_tokens || 0;
        totals.cache_read += u.cache_read_input_tokens || 0;
        totals.cache_creation += u.cache_creation_input_tokens || 0;
    }
}

function updateStatusBar(stream) {
    const bar = $("#status-bar");
    if (!bar) return;
    const status = (stream && stream.status) || "creating";

    const spinner = bar.querySelector(".spinner");
    const dot = bar.querySelector(".status-dot");
    const text = bar.querySelector(".status-text");

    if (status === "running") {
        spinner.style.display = "";
        dot.style.display = "none";
    } else {
        spinner.style.display = "none";
        dot.style.display = "";
    }
    dot.className = `status-dot ${status}`;
    text.className = `status-text ${status}`;
    text.textContent = statusLabel(status);

    const t = (stream && stream.totals) || makeEmptyTotals();
    bar.querySelector(".totals-cost").textContent = `$${t.cost.toFixed(4)}`;
    bar.querySelector(".totals-tokens").textContent = `${formatNum(t.input_tokens)} in / ${formatNum(t.output_tokens)} out`;
    const cacheTotal = t.cache_read + t.cache_creation;
    const cacheEl = bar.querySelector(".totals-cache");
    if (cacheTotal > 0) {
        cacheEl.hidden = false;
        cacheEl.textContent = `${formatNum(cacheTotal)} cached`;
    } else {
        cacheEl.hidden = true;
    }
    bar.querySelector(".totals-turns").textContent = `${t.turns} ${t.turns === 1 ? "turn" : "turns"}`;
}

function statusLabel(status) {
    switch (status) {
        case "running": return "working…";
        case "ready": return "idle";
        case "creating":
        case "loading": return "starting…";
        case "error": return "error";
        case "paused": return "paused";
        case "deleted": return "deleted";
        default: return status || "—";
    }
}

function formatNum(n) {
    return new Intl.NumberFormat().format(n || 0);
}

function labelFor(event) {
    switch (event.type) {
        case "user_prompt": return "user";
        case "assistant_text": return "assistant";
        case "thinking": return "thinking";
        case "tool_use": return `tool_use · ${event.name ?? "?"}`;
        case "tool_result": return event.is_error ? "tool_result · error" : "tool_result";
        case "result": return "result";
        case "error": return "error";
        case "system_init": return "system · init";
        default: return event.type;
    }
}

function bodyFor(event) {
    switch (event.type) {
        case "user_prompt":
        case "assistant_text":
        case "thinking":
            return event.text ?? "";
        case "tool_use":
            return JSON.stringify(event.input ?? {}, null, 2);
        case "tool_result":
            return typeof event.output === "string" ? event.output : JSON.stringify(event.output, null, 2);
        case "result": {
            const cost = event.total_cost_usd != null ? `$${event.total_cost_usd.toFixed(4)}` : "?";
            return `turns=${event.num_turns ?? "?"}  duration=${event.duration_ms ?? "?"}ms  cost=${cost}`;
        }
        case "error":
            return event.message ?? "";
        default:
            return JSON.stringify(event, null, 2);
    }
}

// --- Instances list -------------------------------------------------------

let instancesCache = [];

async function loadInstances() {
    if (isDragging) return;
    let list;
    try {
        const r = await fetch("/api/instances");
        if (!r.ok) return;
        list = await r.json();
    } catch (e) {
        console.error("loadInstances failed", e);
        return;
    }
    instancesCache = list;

    // Reconcile streams: open WS for new instances, evict for gone ones.
    const seen = new Set();
    for (const inst of list) {
        seen.add(inst.title);
        getOrCreateStream(inst.title);
    }
    for (const title of [...streams.keys()]) {
        if (!seen.has(title)) evictStream(title);
    }

    // Update currentInst pointer (status/display_title may have changed server-side).
    let foundCurrent = null;
    for (const inst of list) {
        if (inst.title === currentTitle) {
            foundCurrent = inst;
            break;
        }
    }
    if (currentTitle && !foundCurrent) {
        deselectInstance();
    } else if (foundCurrent) {
        currentInst = foundCurrent;
        applyInstanceToToolbar(foundCurrent);
        // Keep the local stream's "live" status overriding the snapshot's lag.
        const stream = streams.get(currentTitle);
        if (stream && stream.status) currentInst.status = stream.status;
        updateStatusBar(stream);
    }
    renderSidebar();
}

function renderSidebar() {
    if (isDragging) return;
    const root = $("#instance-list");
    root.innerHTML = "";
    for (const inst of instancesCache) {
        // Prefer live status from the open WS over the server snapshot.
        const live = streams.get(inst.title)?.status;
        const merged = live ? { ...inst, status: live } : inst;
        root.appendChild(renderInstanceItem(merged));
    }
}

function displayName(inst) {
    return (inst.display_title && inst.display_title.trim()) || inst.title;
}

function renderInstanceItem(inst) {
    const el = document.createElement("div");
    el.className = "instance-item";
    el.classList.toggle("active", inst.title === currentTitle);
    el.classList.add(inst.status);
    el.dataset.title = inst.title;
    el.draggable = true;

    const titleRow = document.createElement("div");
    titleRow.style.display = "flex";
    titleRow.style.alignItems = "center";
    titleRow.style.gap = "6px";

    const dot = document.createElement("span");
    dot.className = `status-dot ${inst.status}`;
    titleRow.appendChild(dot);

    const title = document.createElement("span");
    title.className = "instance-title";
    title.textContent = displayName(inst);
    titleRow.appendChild(title);

    const del = document.createElement("button");
    del.className = "instance-delete";
    del.type = "button";
    del.title = "delete";
    del.textContent = "×";
    del.addEventListener("click", async (e) => {
        e.stopPropagation();
        if (!confirm(`Delete instance "${displayName(inst)}"?`)) return;
        await fetch(`/api/instances/${encodeURIComponent(inst.title)}`, { method: "DELETE" });
        evictStream(inst.title);
        if (currentTitle === inst.title) deselectInstance();
        await loadInstances();
    });
    titleRow.appendChild(del);

    const path = document.createElement("div");
    path.className = "instance-path";
    path.textContent = inst.path;
    path.title = inst.path;

    const meta = document.createElement("div");
    meta.className = "instance-meta";
    const status = document.createElement("span");
    status.className = `status-label ${inst.status}`;
    status.textContent = inst.status;
    meta.appendChild(status);
    if (inst.permission_mode && inst.permission_mode !== "acceptEdits") {
        const m = document.createElement("span");
        m.textContent = `· ${inst.permission_mode}`;
        meta.appendChild(m);
    }

    el.appendChild(titleRow);
    el.appendChild(path);
    el.appendChild(meta);

    el.addEventListener("click", () => selectInstance(inst));

    el.addEventListener("dragstart", (e) => {
        isDragging = true;
        el.classList.add("dragging");
        e.dataTransfer.effectAllowed = "move";
        e.dataTransfer.setData("text/plain", inst.title);
    });
    el.addEventListener("dragend", () => {
        isDragging = false;
        el.classList.remove("dragging");
        clearDropHints();
    });

    return el;
}

function clearDropHints() {
    for (const el of $$(".instance-item")) {
        el.classList.remove("drop-before", "drop-after");
    }
}

function selectInstance(inst) {
    if (currentTitle === inst.title) return;
    currentTitle = inst.title;
    currentInst = inst;
    $("#empty-state").style.display = "none";
    $("#active-view").classList.add("visible");
    $("#prompt-input").disabled = false;
    $("#send-btn").disabled = false;
    applyInstanceToToolbar(inst);
    const stream = getOrCreateStream(inst.title);
    attachStreamView(inst.title);
    if (!stream.status && inst.status) stream.status = inst.status;
    updateStatusBar(stream);
    renderSidebar();
    setActiveTab("terminal");
}

function deselectInstance() {
    currentTitle = null;
    currentInst = null;
    $("#empty-state").style.display = "";
    $("#active-view").classList.remove("visible");
    $("#prompt-input").disabled = true;
    $("#send-btn").disabled = true;
    // Hide all stream outputs (don't close them — they keep accumulating).
    for (const s of streams.values()) {
        s.outputDiv.style.display = "none";
    }
    renderSidebar();
}

function applyInstanceToToolbar(inst) {
    const titleEl = $("#toolbar-title");
    if (document.activeElement !== titleEl) {
        titleEl.value = displayName(inst);
    }
    const mode = inst.permission_mode || "acceptEdits";
    $("#mode-agent").classList.toggle("active", mode !== "plan");
    $("#mode-plan").classList.toggle("active", mode === "plan");
}

// --- Drag-to-reorder ------------------------------------------------------

const sidebarList = $("#instance-list");

sidebarList.addEventListener("dragover", (e) => {
    e.preventDefault();
    const item = e.target.closest(".instance-item");
    clearDropHints();
    if (!item) return;
    const rect = item.getBoundingClientRect();
    const before = (e.clientY - rect.top) < rect.height / 2;
    item.classList.add(before ? "drop-before" : "drop-after");
    e.dataTransfer.dropEffect = "move";
});

sidebarList.addEventListener("dragleave", (e) => {
    if (!sidebarList.contains(e.relatedTarget)) clearDropHints();
});

sidebarList.addEventListener("drop", async (e) => {
    e.preventDefault();
    const draggedTitle = e.dataTransfer.getData("text/plain");
    const target = e.target.closest(".instance-item");
    const orderedTitles = $$(".instance-item").map((el) => el.dataset.title);
    let newOrder;
    if (!target) {
        newOrder = orderedTitles.filter((t) => t !== draggedTitle);
        newOrder.push(draggedTitle);
    } else {
        const rect = target.getBoundingClientRect();
        const before = (e.clientY - rect.top) < rect.height / 2;
        const without = orderedTitles.filter((t) => t !== draggedTitle);
        const insertAt = without.indexOf(target.dataset.title) + (before ? 0 : 1);
        without.splice(insertAt, 0, draggedTitle);
        newOrder = without;
    }
    clearDropHints();
    isDragging = false;
    const r = await fetch("/api/instances/reorder", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ titles: newOrder }),
    });
    if (!r.ok) appendNote(`reorder failed: ${r.status}`);
    await loadInstances();
});

// --- Tabs -----------------------------------------------------------------

function setActiveTab(name) {
    for (const tab of $$(".tab")) tab.classList.toggle("active", tab.dataset.tab === name);
    for (const pane of $$(".tab-pane")) pane.classList.toggle("active", pane.dataset.pane === name);
    onTabActivated(name);
}

function onTabActivated(name) {
    if (!currentTitle) return;
    if (name === "diff") {
        loadDiff(currentTitle);
    } else if (name === "settings" || name === "plans" || name === "memory") {
        const pane = document.querySelector(`.tab-pane[data-pane="${name}"]`);
        getOrCreateFileEditor(pane).load(currentTitle);
    }
}

for (const tab of $$(".tab")) {
    tab.addEventListener("click", () => setActiveTab(tab.dataset.tab));
}

// --- Diff tab -------------------------------------------------------------

let lastDiffTitle = null;

async function loadDiff(title) {
    const pane = document.querySelector('.tab-pane[data-pane="diff"]');
    const out = pane.querySelector(".diff-content");
    out.textContent = "loading…";
    out.className = "diff-content";
    try {
        const r = await fetch(`/api/instances/${encodeURIComponent(title)}/diff`);
        if (!r.ok) {
            out.textContent = `error: ${r.status} ${await r.text()}`;
            return;
        }
        const { content, error, returncode } = await r.json();
        if (returncode !== 0 && !content) {
            out.textContent = error || `git diff exited ${returncode}`;
            return;
        }
        renderDiff(out, content);
        lastDiffTitle = title;
    } catch (e) {
        out.textContent = `error: ${e}`;
    }
}

function renderDiff(out, content) {
    out.textContent = "";
    if (!content) {
        // empty state handled by ::before
        return;
    }
    const frag = document.createDocumentFragment();
    for (const line of content.split("\n")) {
        const span = document.createElement("span");
        if (line.startsWith("@@")) span.className = "hunk";
        else if (line.startsWith("+++") || line.startsWith("---") || line.startsWith("diff ") || line.startsWith("index ")) span.className = "meta";
        else if (line.startsWith("+")) span.className = "add";
        else if (line.startsWith("-")) span.className = "del";
        span.textContent = line + "\n";
        frag.appendChild(span);
    }
    out.appendChild(frag);
}

// Refresh buttons
for (const btn of $$(".pane-refresh-btn")) {
    btn.addEventListener("click", () => {
        if (!currentTitle) return;
        const target = btn.dataset.paneTarget;
        if (target === "diff") loadDiff(currentTitle);
    });
}

// --- File-editor tabs (settings / plans / memory) ------------------------

const fileEditors = new WeakMap();  // pane element -> FileEditor instance

function getOrCreateFileEditor(pane) {
    let ed = fileEditors.get(pane);
    if (!ed) {
        ed = new FileEditor(pane);
        fileEditors.set(pane, ed);
    }
    return ed;
}

const PERMISSIONS_TAB_INDEX = -2;

class FileEditor {
    constructor(pane) {
        this.pane = pane;
        this.endpoint = pane.dataset.endpoint;       // "rules" | "plans" | "memory"
        this.tabsEl = pane.querySelector(".file-tabs");
        this.pathEl = pane.querySelector(".file-path");
        this.statusEl = pane.querySelector(".file-status");
        this.refreshBtn = pane.querySelector(".file-refresh-btn");
        this.saveBtn = pane.querySelector(".file-save-btn");
        this.editor = pane.querySelector(".file-editor");
        this.fileToolbar = pane.querySelector(".file-toolbar");
        this.permPanelEl = pane.querySelector(".permissions-panel");

        this.title = null;
        this.files = [];          // [{name, path, content, exists?, writable?}]
        this.activeIndex = -1;
        this.dirty = false;       // whether the editor diverges from files[active].content
        this.savedContent = "";

        this.hasPermissionsTab = pane.dataset.hasPermissions === "true";
        this.permPanel = this.hasPermissionsTab && this.permPanelEl
            ? new PermissionsPanel(this.permPanelEl)
            : null;

        this.refreshBtn.addEventListener("click", () => this.load(this.title, { force: true }));
        this.saveBtn.addEventListener("click", () => this.save());
        this.editor.addEventListener("input", () => this.onEdit());
        this.editor.addEventListener("keydown", (e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === "s") {
                e.preventDefault();
                this.save();
            }
        });
    }

    async load(title, { force = false } = {}) {
        if (!title) return;
        if (!force && title === this.title && this.files.length) return;
        this.title = title;
        this.editor.disabled = true;
        this.editor.value = "loading…";
        try {
            const r = await fetch(`/api/instances/${encodeURIComponent(title)}/${this.endpoint}`);
            if (!r.ok) {
                this.editor.value = `error: ${r.status} ${await r.text()}`;
                return;
            }
            const data = await r.json();
            this.files = data.files || [];
            this.renderTabs();
            this.editor.disabled = false;
            if (this.activeIndex === PERMISSIONS_TAB_INDEX) {
                // Stay on permissions tab; just refresh tabs (file list might have changed).
                return;
            }
            if (this.files.length) {
                this.selectFile(this.activeIndex >= 0 && this.activeIndex < this.files.length ? this.activeIndex : 0);
            } else {
                this.editor.value = "";
                this.pathEl.textContent = data.directory ? `(empty) ${data.directory}` : "(no files)";
                this.statusEl.textContent = "";
                this.saveBtn.disabled = true;
            }
        } catch (e) {
            this.editor.value = `error: ${e}`;
        }
    }

    renderTabs() {
        this.tabsEl.innerHTML = "";
        for (let i = 0; i < this.files.length; i++) {
            const f = this.files[i];
            const btn = document.createElement("button");
            btn.type = "button";
            btn.className = "file-tab";
            btn.classList.toggle("active", i === this.activeIndex);
            if (f.exists === false) btn.classList.add("missing");
            btn.textContent = f.name;
            btn.addEventListener("click", () => this.selectFile(i));
            this.tabsEl.appendChild(btn);
        }
        if (this.hasPermissionsTab) {
            const btn = document.createElement("button");
            btn.type = "button";
            btn.className = "file-tab perm-tab";
            btn.classList.toggle("active", this.activeIndex === PERMISSIONS_TAB_INDEX);
            btn.textContent = "Allowed directories";
            btn.addEventListener("click", () => this.selectPermissions());
            this.tabsEl.appendChild(btn);
        }
    }

    showFileView() {
        this.fileToolbar.hidden = false;
        this.editor.hidden = false;
        if (this.permPanelEl) this.permPanelEl.hidden = true;
    }

    showPermissionsView() {
        this.fileToolbar.hidden = true;
        this.editor.hidden = true;
        if (this.permPanelEl) this.permPanelEl.hidden = false;
    }

    selectFile(index) {
        if (this.dirty && !confirm("Discard unsaved changes?")) return;
        this.activeIndex = index;
        this.showFileView();
        const f = this.files[index];
        this.editor.value = f.content || "";
        this.savedContent = this.editor.value;
        this.pathEl.textContent = f.path;
        this.statusEl.className = "file-status";
        this.statusEl.textContent = f.exists === false ? "(file does not exist yet)" : "saved";
        this.statusEl.classList.add("saved");
        this.saveBtn.disabled = true;
        this.saveBtn.classList.remove("dirty");
        this.dirty = false;
        this.renderTabs();
    }

    async selectPermissions() {
        if (this.dirty && !confirm("Discard unsaved changes?")) return;
        if (!this.permPanel) return;
        this.activeIndex = PERMISSIONS_TAB_INDEX;
        this.showPermissionsView();
        this.renderTabs();
        await this.permPanel.load(this.title);
    }

    onEdit() {
        const isDirty = this.editor.value !== this.savedContent;
        if (isDirty === this.dirty) return;
        this.dirty = isDirty;
        this.saveBtn.disabled = !isDirty;
        this.saveBtn.classList.toggle("dirty", isDirty);
        this.statusEl.className = "file-status";
        if (isDirty) {
            this.statusEl.textContent = "unsaved";
            this.statusEl.classList.add("unsaved");
        } else {
            this.statusEl.textContent = "saved";
            this.statusEl.classList.add("saved");
        }
        // Mark active tab dirty
        const activeTab = this.tabsEl.children[this.activeIndex];
        if (activeTab) activeTab.classList.toggle("dirty", isDirty);
    }

    async save() {
        if (this.activeIndex < 0 || !this.title) return;
        const f = this.files[this.activeIndex];
        const content = this.editor.value;
        const r = await fetch(`/api/instances/${encodeURIComponent(this.title)}/${this.endpoint}`, {
            method: "PUT",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ path: f.path, content }),
        });
        if (!r.ok) {
            this.statusEl.className = "file-status error";
            this.statusEl.textContent = `save failed: ${r.status}`;
            return;
        }
        f.content = content;
        f.exists = true;
        this.savedContent = content;
        this.dirty = false;
        this.saveBtn.disabled = true;
        this.saveBtn.classList.remove("dirty");
        this.statusEl.className = "file-status saved";
        this.statusEl.textContent = "saved";
        const activeTab = this.tabsEl.children[this.activeIndex];
        if (activeTab) {
            activeTab.classList.remove("dirty", "missing");
        }
    }
}

class PermissionsPanel {
    constructor(el) {
        this.el = el;
        this.modeEl = el.querySelector(".perm-mode");
        this.dirsListEl = el.querySelector(".dirs-list");
        this.addInputEl = el.querySelector(".dir-add-input");
        this.addBtnEl = el.querySelector(".dir-add-btn");
        this.applyBtnEl = el.querySelector(".perm-apply-btn");
        this.applyStatusEl = el.querySelector(".perm-apply-status");

        this.title = null;
        this.dirs = [];
        this.permission_mode = "acceptEdits";
        // Track dirty so we know whether to enable Apply.
        this.savedMode = "acceptEdits";
        this.savedDirs = [];

        this.addBtnEl.addEventListener("click", () => this.addDir());
        this.addInputEl.addEventListener("keydown", (e) => {
            if (e.key === "Enter") {
                e.preventDefault();
                this.addDir();
            }
        });
        this.modeEl.addEventListener("change", () => {
            this.permission_mode = this.modeEl.value;
            this.refreshDirty();
        });
        this.applyBtnEl.addEventListener("click", () => this.apply());
    }

    async load(title) {
        if (!title) return;
        this.title = title;
        this.applyStatusEl.textContent = "loading…";
        this.applyBtnEl.disabled = true;
        try {
            const r = await fetch(`/api/instances/${encodeURIComponent(title)}`);
            if (!r.ok) {
                this.applyStatusEl.textContent = `error: ${r.status}`;
                return;
            }
            const inst = await r.json();
            this.permission_mode = inst.permission_mode || "acceptEdits";
            this.dirs = (inst.add_dirs || []).slice();
            this.savedMode = this.permission_mode;
            this.savedDirs = this.dirs.slice();
            this.modeEl.value = this.permission_mode;
            this.renderDirs();
            this.applyStatusEl.textContent = "";
            this.applyBtnEl.disabled = true;
        } catch (e) {
            this.applyStatusEl.textContent = `error: ${e}`;
        }
    }

    renderDirs() {
        this.dirsListEl.innerHTML = "";
        if (this.dirs.length === 0) {
            const li = document.createElement("li");
            li.className = "dirs-empty";
            li.textContent = "Only the working directory is accessible. Add paths below to extend.";
            this.dirsListEl.appendChild(li);
            return;
        }
        for (let i = 0; i < this.dirs.length; i++) {
            const li = document.createElement("li");
            const span = document.createElement("span");
            span.className = "dir-path";
            span.textContent = this.dirs[i];
            const btn = document.createElement("button");
            btn.type = "button";
            btn.className = "dir-remove";
            btn.title = "Remove";
            btn.textContent = "×";
            btn.addEventListener("click", () => {
                this.dirs.splice(i, 1);
                this.renderDirs();
                this.refreshDirty();
            });
            li.appendChild(span);
            li.appendChild(btn);
            this.dirsListEl.appendChild(li);
        }
    }

    addDir() {
        const path = this.addInputEl.value.trim();
        if (!path) return;
        if (this.dirs.includes(path)) {
            this.applyStatusEl.textContent = `already in list: ${path}`;
            return;
        }
        this.dirs.push(path);
        this.addInputEl.value = "";
        this.applyStatusEl.textContent = "";
        this.renderDirs();
        this.refreshDirty();
    }

    refreshDirty() {
        const dirty = this.permission_mode !== this.savedMode
            || !sameStringList(this.dirs, this.savedDirs);
        this.applyBtnEl.disabled = !dirty;
        this.applyBtnEl.classList.toggle("dirty", dirty);
        if (dirty) this.applyStatusEl.textContent = "unsaved";
    }

    async apply() {
        if (!this.title) return;
        this.applyBtnEl.disabled = true;
        this.applyStatusEl.textContent = "restarting session…";
        try {
            const r = await fetch(`/api/instances/${encodeURIComponent(this.title)}/permissions`, {
                method: "PATCH",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({
                    permission_mode: this.permission_mode,
                    add_dirs: this.dirs,
                }),
            });
            if (!r.ok) {
                this.applyStatusEl.textContent = `apply failed: ${r.status} ${await r.text()}`;
                this.applyBtnEl.disabled = false;
                return;
            }
            const inst = await r.json();
            this.permission_mode = inst.permission_mode || "acceptEdits";
            this.dirs = (inst.add_dirs || []).slice();
            this.savedMode = this.permission_mode;
            this.savedDirs = this.dirs.slice();
            this.modeEl.value = this.permission_mode;
            this.renderDirs();
            this.applyStatusEl.textContent = "applied · session restarted";
            this.applyBtnEl.classList.remove("dirty");
        } catch (e) {
            this.applyStatusEl.textContent = `error: ${e}`;
            this.applyBtnEl.disabled = false;
        }
    }
}

function sameStringList(a, b) {
    if (a.length !== b.length) return false;
    for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
    return true;
}

// --- Toolbar buttons ------------------------------------------------------

$("#mode-agent").addEventListener("click", () => {
    alert("Mode switching mid-session isn't implemented yet — set the mode when creating the instance.");
});
$("#mode-plan").addEventListener("click", () => {
    alert("Mode switching mid-session isn't implemented yet — set the mode when creating the instance.");
});
$("#btn-pause").addEventListener("click", () => {
    alert("Pause/Resume not yet supported by the backend.");
});
$("#btn-resume").addEventListener("click", () => {
    alert("Pause/Resume not yet supported by the backend.");
});
$("#btn-kill").addEventListener("click", async () => {
    if (!currentTitle) return;
    if (!confirm(`Kill instance "${currentTitle}"?`)) return;
    const title = currentTitle;
    await fetch(`/api/instances/${encodeURIComponent(title)}`, { method: "DELETE" });
    evictStream(title);
    deselectInstance();
    await loadInstances();
});

// --- Rename via toolbar title ---------------------------------------------

async function commitRename() {
    if (!currentTitle || !currentInst) return;
    const proposed = $("#toolbar-title").value.trim();
    const newDisplay = (!proposed || proposed === currentTitle) ? null : proposed;
    if ((currentInst.display_title || null) === newDisplay) return;
    const r = await fetch(`/api/instances/${encodeURIComponent(currentTitle)}/rename`, {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ display_title: newDisplay }),
    });
    if (!r.ok) {
        appendNote(`rename failed: ${r.status}`);
        $("#toolbar-title").value = displayName(currentInst);
        return;
    }
    const updated = await r.json();
    currentInst = updated;
    await loadInstances();
}

$("#toolbar-title").addEventListener("blur", commitRename);
$("#toolbar-title").addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
        e.preventDefault();
        $("#toolbar-title").blur();
    } else if (e.key === "Escape") {
        e.preventDefault();
        if (currentInst) $("#toolbar-title").value = displayName(currentInst);
        $("#toolbar-title").blur();
    }
});

// --- New-instance dialog --------------------------------------------------

$("#btn-new").addEventListener("click", () => $("#new-dialog").showModal());

$("#new-form").addEventListener("submit", async (e) => {
    const form = e.target;
    const btn = e.submitter;
    if (!btn || btn.value !== "create") return;
    e.preventDefault();
    const data = Object.fromEntries(new FormData(form));
    const addDirsRaw = data.add_dirs || "";
    delete data.add_dirs;
    const add_dirs = addDirsRaw
        .split(/\r?\n/)
        .map((l) => l.trim())
        .filter((l) => l.length > 0);
    const body = { ...data, add_dirs };
    const r = await fetch("/api/instances", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
    });
    if (!r.ok) {
        alert(`create failed (${r.status}): ${await r.text()}`);
        return;
    }
    const inst = await r.json();
    $("#new-dialog").close();
    form.reset();
    await loadInstances();
    selectInstance(inst);
});

// --- Prompt form (Enter sends, Shift+Enter newline) -----------------------

$("#prompt-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    if (!currentTitle) return;
    const text = $("#prompt-input").value.trim();
    if (!text) return;
    $("#prompt-input").value = "";
    const r = await fetch(`/api/instances/${encodeURIComponent(currentTitle)}/send`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ text }),
    });
    if (!r.ok) appendNote(`send failed: ${r.status}`);
});

$("#prompt-input").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        $("#prompt-form").requestSubmit();
    }
});

// --- Init -----------------------------------------------------------------

checkAuth();
loadInstances();
setInterval(loadInstances, 5000);
setInterval(checkAuth, 5000);
