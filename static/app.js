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
    stream = { ws: null, outputDiv, status: null, evicting: false };
    streams.set(title, stream);
    openStreamWs(title, stream);
    return stream;
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
            setWorkingIndicator(event.status === "running");
        }
        if (event.status === "ready" && !hasMeaningfulOutputIn(stream.outputDiv)) {
            appendNoteToStream(stream, "Claude is ready. Send a prompt to get started.");
        }
        renderSidebar();
        return;
    }

    if (event.type === "user_prompt") {
        const turn = startUserTurn(stream, event);
        if (currentTitle === title) turn.scrollIntoView({ block: "end" });
        return;
    }

    // Everything else nests into the current turn (creating a session turn if
    // no user prompt has happened yet).
    const body = getOrCreateCurrentTurnBody(stream);

    let el;
    if (event.type === "system_init") {
        el = document.createElement("details");
        el.className = "event event-system_init";
        const summary = document.createElement("summary");
        summary.appendChild(makeLabelSpan("system · init", event.ts));
        el.appendChild(summary);
        const pre = document.createElement("pre");
        pre.textContent = JSON.stringify(event.data ?? {}, null, 2);
        el.appendChild(pre);
    } else {
        el = document.createElement("div");
        el.className = `event event-${event.type}`;
        el.appendChild(makeLabelSpan(labelFor(event), event.ts));
        const bodyEl = document.createElement("pre");
        bodyEl.textContent = bodyFor(event);
        el.appendChild(bodyEl);
    }

    body.appendChild(el);
    if (currentTitle === title) el.scrollIntoView({ block: "end" });
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

function makeLabelSpan(text, ts) {
    const labelEl = document.createElement("span");
    labelEl.className = "label";
    labelEl.textContent = text;
    if (ts) {
        const tsEl = document.createElement("span");
        tsEl.className = "ts";
        tsEl.textContent = formatTimestamp(ts);
        tsEl.title = ts;
        labelEl.appendChild(tsEl);
    }
    return labelEl;
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

function setWorkingIndicator(visible) {
    const el = $("#working-indicator");
    if (el) el.hidden = !visible;
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
        const live = streams.get(currentTitle)?.status;
        if (live) currentInst.status = live;
        setWorkingIndicator(currentInst.status === "running");
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
    getOrCreateStream(inst.title);
    attachStreamView(inst.title);
    const liveStatus = streams.get(inst.title)?.status ?? inst.status;
    setWorkingIndicator(liveStatus === "running");
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
    setWorkingIndicator(false);
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
}

for (const tab of $$(".tab")) {
    tab.addEventListener("click", () => setActiveTab(tab.dataset.tab));
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
    const r = await fetch("/api/instances", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(data),
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
