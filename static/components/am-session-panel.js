/**
 * Session-management panel.
 *
 * Surfaces the per-instance context-pressure session manager: enable toggle,
 * soft/hard split thresholds, context-window override, a live pressure bar, the
 * current handoff phase, and the checkpoint history. Mirrors the API contract in
 * server.py (`GET/PUT .../session`, `POST .../session/split`).
 *
 * Split-decision precedence (server-side): hard > manual > soft. The "Split now"
 * button arms a *manual* split that fires when the agent is next idle.
 */

import * as api from '../lib/api.js';

// HandoffPhase (handoff.py) — IntEnum values.
const PHASE_NAMES = {
    0: 'Idle',
    1: 'Wrapping up',
    2: 'Transitioning',
    3: 'Restoring',
};

// Effective defaults (session_manager.py) — shown as input placeholders when 0.
// Keep in sync with DEFAULT_*_CONTEXT_PERCENTAGE in session_manager.py.
const DEFAULTS = {
    soft_context_percentage: 85,
    hard_context_percentage: 95,
    context_window_size: 200000,
    split_cooldown_sec: 60,
    checkpoint_timeout_sec: 180,
};

const POLL_MS = 3000;

class AmSessionPanel extends HTMLElement {
    constructor() {
        super();
        this._title = null;
        this._pollTimer = null;
        this._saving = false;
    }

    connectedCallback() {
        this.innerHTML = `
            <div class="pane-toolbar">
                <span class="pane-label">session management</span>
                <button type="button" class="pane-refresh-btn" data-act="refresh">Refresh</button>
            </div>
            <div class="sess-body">
                <div class="sess-msg" data-role="msg" hidden></div>

                <section class="sess-card">
                    <label class="sess-toggle">
                        <input type="checkbox" data-field="enabled">
                        <span>Enable context-pressure session management</span>
                    </label>
                    <p class="sess-hint">When enabled, the agent auto-splits into a fresh session as context fills,
                    carrying a checkpoint forward. Disabled by default per instance.</p>
                </section>

                <section class="sess-card" data-role="config">
                    <h3>Thresholds</h3>
                    <div class="sess-grid">
                        <label>Soft split %
                            <input type="number" min="0" max="100" data-field="soft_context_percentage" placeholder="${DEFAULTS.soft_context_percentage}">
                        </label>
                        <label>Hard split %
                            <input type="number" min="0" max="100" data-field="hard_context_percentage" placeholder="${DEFAULTS.hard_context_percentage}">
                        </label>
                        <label>Context window (tokens)
                            <input type="number" min="0" step="1000" data-field="context_window_size" placeholder="${DEFAULTS.context_window_size}">
                        </label>
                        <label>Split cooldown (s)
                            <input type="number" min="0" data-field="split_cooldown_sec" placeholder="${DEFAULTS.split_cooldown_sec}">
                        </label>
                        <label>Checkpoint timeout (s)
                            <input type="number" min="0" data-field="checkpoint_timeout_sec" placeholder="${DEFAULTS.checkpoint_timeout_sec}">
                        </label>
                    </div>
                    <p class="sess-hint">A value of 0 uses the default shown. Settings apply in-process — no restart.</p>
                    <div class="sess-actions">
                        <button type="button" class="sess-btn" data-act="save">Save settings</button>
                        <button type="button" class="sess-btn sess-btn-accent" data-act="split">Split now</button>
                    </div>
                </section>

                <section class="sess-card" data-role="status">
                    <h3>Status</h3>
                    <div class="sess-status-row">
                        <span class="sess-chip" data-role="session-chip">Session #1</span>
                        <span class="sess-chip" data-role="handoff-chip" hidden></span>
                        <span class="sess-chip sess-chip-warn" data-role="cooldown-chip" hidden>cooldown</span>
                        <span class="sess-chip sess-chip-warn" data-role="manual-chip" hidden>manual split pending</span>
                    </div>
                    <div class="sess-pressure" data-role="pressure">
                        <div class="sess-bar">
                            <div class="sess-bar-fill" data-role="bar-fill"></div>
                            <div class="sess-bar-mark" data-role="bar-soft"></div>
                            <div class="sess-bar-mark sess-bar-mark-hard" data-role="bar-hard"></div>
                        </div>
                        <div class="sess-pressure-text" data-role="pressure-text"></div>
                    </div>
                </section>

                <section class="sess-card">
                    <h3>Checkpoints</h3>
                    <div class="sess-checkpoints" data-role="checkpoints"></div>
                </section>
            </div>
        `;

        this.querySelector('[data-act="refresh"]').addEventListener('click', () => {
            if (this._title) this.load(this._title, true);
        });
        this.querySelector('[data-act="save"]').addEventListener('click', () => this._save());
        this.querySelector('[data-act="split"]').addEventListener('click', () => this._split());

        // Toggling enable saves immediately (it's the primary control).
        this.querySelector('[data-field="enabled"]').addEventListener('change', () => this._save());
    }

    disconnectedCallback() {
        this._stopPolling();
    }

    async load(title, force = false) {
        if (!force && title === this._title) {
            this._startPolling();
            return;
        }
        this._title = title;
        this._setMsg('');
        try {
            const info = await api.getSession(title);
            this._renderConfig(info);
            this._renderStatus(info);
        } catch (e) {
            this._setMsg(`error: ${e.message}`, true);
        }
        this._startPolling();
    }

    // --- config (only written from server on load/save; never clobbered by polling) ---
    _renderConfig(info) {
        const c = info.config || {};
        // Remember the server's truth so a rejected (unsafe) toggle can be reverted.
        this._serverEnabled = !!c.enabled;
        this._set('enabled', !!c.enabled, 'checked');
        for (const f of ['soft_context_percentage', 'hard_context_percentage',
                         'context_window_size', 'split_cooldown_sec', 'checkpoint_timeout_sec']) {
            // 0 means "default" — leave the field blank so the placeholder shows.
            this._set(f, c[f] ? String(c[f]) : '');
        }
    }

    _gatherConfig() {
        const num = (f) => parseInt(this.querySelector(`[data-field="${f}"]`).value, 10) || 0;
        return {
            enabled: this.querySelector('[data-field="enabled"]').checked,
            soft_context_percentage: num('soft_context_percentage'),
            hard_context_percentage: num('hard_context_percentage'),
            context_window_size: num('context_window_size'),
            split_cooldown_sec: num('split_cooldown_sec'),
            checkpoint_timeout_sec: num('checkpoint_timeout_sec'),
        };
    }

    // Reject configs that would brick the instance in an immediate-split loop.
    // Motivated by a real incident: hard=2% of a 200k window = 4k tokens, which is
    // *below* the unavoidable baseline cost of a fresh session (system prompt + tool
    // schemas). Every new session is then born over the limit and splits the instant
    // it goes idle — an infinite loop that repeatedly wipes the agent's memory.
    // Returns an error string when unsafe, or null when the config is fine.
    _validateConfig(cfg) {
        if (!cfg.enabled) return null;  // disabled => monitor is off => always safe
        const win = cfg.context_window_size || DEFAULTS.context_window_size;
        const hardPct = cfg.hard_context_percentage || DEFAULTS.hard_context_percentage;
        const softPct = cfg.soft_context_percentage || DEFAULTS.soft_context_percentage;
        // A fresh session's baseline (system prompt + tool definitions) reliably
        // exceeds this; a hard threshold at/under it guarantees an instant split.
        const BASELINE_TOKENS = 30000;
        const hardTokens = Math.floor((hardPct / 100) * win);
        if (hardTokens <= BASELINE_TOKENS) {
            return `Hard split at ${hardPct}% of ${win.toLocaleString()} tokens = `
                + `${hardTokens.toLocaleString()} tokens — at or below the ~`
                + `${BASELINE_TOKENS.toLocaleString()}-token baseline every fresh session uses. `
                + `Each new session would be born over the limit and split immediately (infinite loop). `
                + `Raise the hard %, raise the context window (or set it to 0 to auto-detect from the model), `
                + `or leave session management disabled.`;
        }
        if (softPct >= hardPct) {
            return `Soft split % (${softPct}) must be below hard split % (${hardPct}) — `
                + `otherwise the soft and hard triggers fire together.`;
        }
        return null;
    }

    async _save() {
        if (!this._title || this._saving) return;
        const cfg = this._gatherConfig();
        const unsafe = this._validateConfig(cfg);
        if (unsafe) {
            // Revert an unsafe enable toggle to the server's last-known state; keep
            // the user's typed numbers so they can correct them.
            this._set('enabled', this._serverEnabled, 'checked');
            this._setMsg(unsafe, true);
            return;
        }
        this._saving = true;
        this._setMsg('saving…');
        try {
            const info = await api.putSessionConfig(this._title, cfg);
            this._renderConfig(info);
            this._renderStatus(info);
            this._setMsg('saved', false, 1500);
        } catch (e) {
            this._setMsg(`error: ${e.message}`, true);
        } finally {
            this._saving = false;
        }
    }

    async _split() {
        if (!this._title) return;
        try {
            const res = await api.requestSplit(this._title);
            this._setMsg(res.status === 'manual_split_armed'
                ? 'manual split armed — fires when the agent is next idle' : 'split requested', false, 4000);
            this.load(this._title, true);
        } catch (e) {
            // 400 here means session management is not enabled for this instance.
            this._setMsg(`error: ${e.message}`, true);
        }
    }

    // --- status (poll-safe; read-only sections only) ---
    _renderStatus(info) {
        const enabled = !!info.enabled;
        this.querySelector('[data-role="config"]').classList.toggle('sess-disabled', !enabled);

        // Current session number.
        const sess = info.current_session ?? info.state?.current_session ?? 1;
        this.querySelector('[data-role="session-chip"]').textContent = `Session #${sess}`;

        // Handoff phase.
        const handoffChip = this.querySelector('[data-role="handoff-chip"]');
        const inProgress = !!info.handoff_in_progress;
        const phase = info.handoff?.phase;
        if (inProgress && phase != null && phase !== 0) {
            handoffChip.hidden = false;
            handoffChip.classList.add('sess-chip-active');
            handoffChip.textContent = `handoff: ${PHASE_NAMES[phase] || `phase ${phase}`}`;
        } else {
            handoffChip.hidden = true;
            handoffChip.classList.remove('sess-chip-active');
        }

        // Pressure bar + flags.
        const p = info.pressure;
        const pressureEl = this.querySelector('[data-role="pressure"]');
        if (enabled && p) {
            pressureEl.hidden = false;
            const used = Math.max(0, Math.min(100, p.used_percentage || 0));
            const fill = this.querySelector('[data-role="bar-fill"]');
            fill.style.width = `${used}%`;
            fill.classList.toggle('over-hard', !!p.should_hard_split || used >= (p.hard_threshold || 100));
            fill.classList.toggle('over-soft', !p.should_hard_split && used >= (p.soft_threshold || 100));
            this.querySelector('[data-role="bar-soft"]').style.left = `${p.soft_threshold || 0}%`;
            this.querySelector('[data-role="bar-hard"]').style.left = `${p.hard_threshold || 0}%`;
            const tokens = (p.total_context_tokens || 0).toLocaleString();
            const win = (p.context_window_size || 0).toLocaleString();
            this.querySelector('[data-role="pressure-text"]').textContent =
                `${used.toFixed(1)}%  ·  ${tokens} / ${win} tokens  ·  soft ${p.soft_threshold}% / hard ${p.hard_threshold}%`;
            this.querySelector('[data-role="cooldown-chip"]').hidden = !p.in_cooldown;
            this.querySelector('[data-role="manual-chip"]').hidden = !p.manual_split_pending;
        } else {
            pressureEl.hidden = true;
            this.querySelector('[data-role="cooldown-chip"]').hidden = true;
            this.querySelector('[data-role="manual-chip"]').hidden = true;
        }

        // Checkpoints (newest first).
        const checks = info.state?.checkpoints || [];
        const el = this.querySelector('[data-role="checkpoints"]');
        el.textContent = '';
        if (!checks.length) {
            const empty = document.createElement('div');
            empty.className = 'sess-empty';
            empty.textContent = 'No checkpoints yet.';
            el.appendChild(empty);
            return;
        }
        for (const c of [...checks].reverse()) {
            const row = document.createElement('div');
            row.className = 'sess-ckpt';

            const head = document.createElement('div');
            head.className = 'sess-ckpt-head';
            const num = document.createElement('span');
            num.className = 'sess-ckpt-num';
            num.textContent = `#${c.session}`;
            const trig = document.createElement('span');
            trig.className = 'sess-ckpt-trigger';
            trig.textContent = c.trigger || '';
            const ts = document.createElement('span');
            ts.className = 'sess-ckpt-ts';
            ts.textContent = this._fmtTime(c.timestamp);
            head.append(num, trig, ts);

            const path = document.createElement('div');
            path.className = 'sess-ckpt-path';
            path.textContent = c.path || '';

            row.append(head, path);
            el.appendChild(row);
        }
    }

    _fmtTime(iso) {
        if (!iso) return '';
        const d = new Date(iso);
        return isNaN(d) ? iso : d.toLocaleString();
    }

    // --- polling ---
    _startPolling() {
        this._stopPolling();
        this._pollTimer = setInterval(() => this._poll(), POLL_MS);
    }

    _stopPolling() {
        if (this._pollTimer) {
            clearInterval(this._pollTimer);
            this._pollTimer = null;
        }
    }

    async _poll() {
        // Only poll while this pane is the active tab and we're not mid-save.
        if (!this._title || this._saving || !this.classList.contains('active')) return;
        try {
            const info = await api.getSession(this._title);
            this._renderStatus(info);
        } catch {
            // transient; next tick retries
        }
    }

    // --- helpers ---
    _set(field, value, prop = 'value') {
        const el = this.querySelector(`[data-field="${field}"]`);
        if (el) el[prop] = value;
    }

    _setMsg(text, isError = false, clearAfter = 0) {
        const el = this.querySelector('[data-role="msg"]');
        if (!el) return;
        el.textContent = text;
        el.hidden = !text;
        el.classList.toggle('sess-msg-error', isError);
        if (this._msgTimer) { clearTimeout(this._msgTimer); this._msgTimer = null; }
        if (text && clearAfter > 0) {
            this._msgTimer = setTimeout(() => { el.hidden = true; el.textContent = ''; }, clearAfter);
        }
    }
}

customElements.define('am-session-panel', AmSessionPanel);
