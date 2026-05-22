/**
 * Centralized API calls for agent-manager.
 */

const BASE = '/api';

export async function fetchModels() {
    const r = await fetch(`${BASE}/models`);
    return r.ok ? r.json() : [];
}

export async function fetchInstances() {
    const r = await fetch(`${BASE}/instances`);
    if (!r.ok) throw new Error(`Failed to fetch instances: ${r.status}`);
    return r.json();
}

export async function fetchInstance(title) {
    const r = await fetch(`${BASE}/instances/${encodeURIComponent(title)}`);
    if (!r.ok) throw new Error(`Failed to fetch instance: ${r.status}`);
    return r.json();
}

export async function createInstance({ name, path, permission_mode, model, add_dirs, settings_json }) {
    const r = await fetch(`${BASE}/instances`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, path, permission_mode, model, add_dirs, settings_json }),
    });
    if (!r.ok) {
        const text = await r.text();
        throw new Error(`Failed to create instance: ${r.status} ${text}`);
    }
    return r.json();
}

export async function deleteInstance(title) {
    const r = await fetch(`${BASE}/instances/${encodeURIComponent(title)}`, {
        method: 'DELETE',
    });
    if (!r.ok) throw new Error(`Failed to delete instance: ${r.status}`);
}

export async function renameInstance(title, displayTitle) {
    const r = await fetch(`${BASE}/instances/${encodeURIComponent(title)}/rename`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ display_title: displayTitle }),
    });
    if (!r.ok) throw new Error(`Failed to rename instance: ${r.status}`);
    return r.json();
}

export async function reorderInstances(titles) {
    const r = await fetch(`${BASE}/instances/reorder`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ titles }),
    });
    if (!r.ok) throw new Error(`Failed to reorder instances: ${r.status}`);
    return r.json();
}

export async function updatePermissions(title, { permission_mode, model, add_dirs }) {
    const r = await fetch(`${BASE}/instances/${encodeURIComponent(title)}/permissions`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ permission_mode, model, add_dirs }),
    });
    if (!r.ok) {
        const text = await r.text();
        throw new Error(`Failed to update permissions: ${r.status} ${text}`);
    }
    return r.json();
}

export async function sendPrompt(title, text, images = null) {
    const body = { text };
    if (images && images.length > 0) {
        body.images = images;  // [{media_type: "image/png", data: "base64..."}]
    }
    const r = await fetch(`${BASE}/instances/${encodeURIComponent(title)}/send`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
    });
    if (!r.ok) throw new Error(`Failed to send prompt: ${r.status}`);
}

export async function abortInstance(title) {
    const r = await fetch(`${BASE}/instances/${encodeURIComponent(title)}/abort`, {
        method: 'POST',
    });
    if (!r.ok) throw new Error(`Failed to abort: ${r.status}`);
    return r.json();
}

export async function fetchDiff(title) {
    const r = await fetch(`${BASE}/instances/${encodeURIComponent(title)}/diff`);
    if (!r.ok) throw new Error(`Failed to fetch diff: ${r.status}`);
    return r.json();
}

export async function fetchGitStatus(title) {
    const r = await fetch(`${BASE}/instances/${encodeURIComponent(title)}/git-status`);
    if (!r.ok) throw new Error(`Failed to fetch git status: ${r.status}`);
    return r.json();
}

export async function fetchFiles(title, endpoint) {
    const r = await fetch(`${BASE}/instances/${encodeURIComponent(title)}/${endpoint}`);
    if (!r.ok) throw new Error(`Failed to fetch files: ${r.status}`);
    return r.json();
}

export async function saveFile(title, endpoint, path, content) {
    const r = await fetch(`${BASE}/instances/${encodeURIComponent(title)}/${endpoint}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ path, content }),
    });
    if (!r.ok) {
        const text = await r.text();
        throw new Error(`Failed to save file: ${r.status} ${text}`);
    }
    return r.json();
}

export async function checkAuth() {
    const r = await fetch(`${BASE}/auth/status`);
    if (!r.ok) return { authed: false };
    return r.json();
}

export async function startLogin() {
    const r = await fetch(`${BASE}/auth/login`, { method: 'POST' });
    if (!r.ok) throw new Error(`Failed to start login: ${r.status}`);
    return r.json();
}

export async function sendLoginInput(sessionId, data) {
    const r = await fetch(`${BASE}/auth/login/${encodeURIComponent(sessionId)}/input`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ data }),
    });
    if (!r.ok) throw new Error(`Failed to send login input: ${r.status}`);
}

export async function cancelLogin(sessionId) {
    await fetch(`${BASE}/auth/login/${encodeURIComponent(sessionId)}`, { method: 'DELETE' });
}

export function loginWebSocketUrl(sessionId) {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    return `${proto}://${location.host}${BASE}/auth/login/${encodeURIComponent(sessionId)}`;
}

export function eventsWebSocketUrl(title, sinceSeq = -1) {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    const query = sinceSeq >= 0 ? `?since_seq=${sinceSeq}` : '';
    return `${proto}://${location.host}${BASE}/instances/${encodeURIComponent(title)}/events${query}`;
}

export async function reparentInstance(title, parentTitle) {
    const r = await fetch(`${BASE}/instances/${encodeURIComponent(title)}/reparent`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ parent: parentTitle }),
    });
    if (!r.ok) {
        const text = await r.text();
        throw new Error(`Failed to reparent instance: ${r.status} ${text}`);
    }
    return r.json();
}

export async function fetchChildren(title) {
    const r = await fetch(`${BASE}/instances/${encodeURIComponent(title)}/children`);
    if (!r.ok) throw new Error(`Failed to fetch children: ${r.status}`);
    return r.json();
}

export async function updateTask(title, task) {
    const r = await fetch(`${BASE}/instances/${encodeURIComponent(title)}/task`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ task }),
    });
    if (!r.ok) throw new Error(`Failed to update task: ${r.status}`);
    return r.json();
}

export async function updateFolder(title, folder) {
    const r = await fetch(`${BASE}/instances/${encodeURIComponent(title)}/folder`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ folder }),
    });
    if (!r.ok) throw new Error(`Failed to update folder: ${r.status}`);
    return r.json();
}

export async function fetchFolders() {
    const r = await fetch(`${BASE}/folders`);
    if (!r.ok) return [];
    return r.json();
}
