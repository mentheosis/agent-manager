/**
 * Terminal pane component - output area, status bar, and prompt form.
 */

import * as api from '../lib/api.js';
import { streamManager } from '../lib/streams.js';

class AmTerminalPane extends HTMLElement {
    constructor() {
        super();
        this._instance = null;
        this._unsubscribe = null;
        this._wasNearBottom = true;  // Track scroll position for auto-scroll
    }

    connectedCallback() {
        this.innerHTML = `
            <div id="output-area" aria-live="polite"></div>
            <div id="status-bar">
                <div class="status-icon">
                    <div class="spinner" aria-hidden="true"></div>
                    <span class="status-dot" aria-hidden="true"></span>
                </div>
                <span class="status-text">idle</span>
                <span class="status-spacer"></span>
                <span class="status-model" hidden title="Active model reported by the SDK"></span>
                <span class="status-totals">
                    <span class="totals-tokens" title="cumulative input / output tokens">0 in / 0 out</span>
                    <span class="totals-cache" title="cached input tokens (read / created)" hidden>0 cached</span>
                    <span class="totals-cost" title="cumulative session cost in USD">$0.0000</span>
                    <span class="totals-turns">0 turns</span>
                </span>
            </div>
            <form id="prompt-form">
                <textarea
                    id="prompt-input"
                    placeholder="Send a prompt (Enter to send, Shift+Enter for newline)…"
                    rows="3"
                    disabled></textarea>
                <button type="submit" id="send-btn" disabled>Send</button>
            </form>
        `;

        this.setupPromptForm();
    }

    disconnectedCallback() {
        if (this._unsubscribe) {
            this._unsubscribe();
            this._unsubscribe = null;
        }
    }

    setupPromptForm() {
        const form = this.querySelector('#prompt-form');
        const input = this.querySelector('#prompt-input');
        const btn = this.querySelector('#send-btn');

        form.addEventListener('submit', (e) => {
            e.preventDefault();
            this.submitPrompt();
        });

        input.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault();
                this.submitPrompt();
            }
        });
    }

    async submitPrompt() {
        if (!this._instance) return;

        const input = this.querySelector('#prompt-input');
        const text = input.value.trim();
        if (!text) return;

        input.value = '';

        try {
            await api.sendPrompt(this._instance.title, text);
        } catch (err) {
            this.appendNote(`Error: ${err.message}`);
        }
    }

    get instance() {
        return this._instance;
    }

    set instance(inst) {
        // Unsubscribe from old stream
        if (this._unsubscribe) {
            this._unsubscribe();
            this._unsubscribe = null;
        }

        this._instance = inst;
        this.clearOutput();

        if (inst) {
            // Subscribe to stream (replays buffered events)
            const stream = streamManager.get(inst.title);
            this._unsubscribe = stream.subscribe((event) => this.handleEvent(event, stream));

            this.updateStatusBar(stream);

            // Scroll to bottom after replaying history
            this.scrollToBottom();
        }
    }

    handleEvent(event, stream) {
        if (event.type === 'status') {
            this.updateStatusBar(stream);
            if (event.status === 'ready' && !this.hasMeaningfulOutput()) {
                this.appendNote('Claude is ready. Send a prompt to get started.');
            }
            return;
        }

        if (event.type === 'system_init') {
            this.updateStatusBar(stream);
            // Fall through to render
        }

        if (event.type === 'result') {
            this.updateStatusBar(stream);
            // Fall through to render
        }

        if (event.type === 'connection') {
            if (event.status === 'closed') {
                this.appendNote('Connection closed');
            } else if (event.status === 'error') {
                this.appendNote('Connection error');
            }
            return;
        }

        if (event.type === 'user_prompt') {
            this.startUserTurn(event);
            this.autoScroll();
            return;
        }

        // Everything else goes into current turn body
        this.appendEventToCurrentTurn(event);
    }

    updateStatusBar(stream) {
        const status = stream?.status || 'creating';
        const spinner = this.querySelector('.spinner');
        const dot = this.querySelector('.status-dot');
        const text = this.querySelector('.status-text');

        if (status === 'running') {
            spinner.style.display = '';
            dot.style.display = 'none';
        } else {
            spinner.style.display = 'none';
            dot.style.display = '';
        }

        dot.className = `status-dot ${status}`;
        text.className = `status-text ${status}`;
        text.textContent = this.statusLabel(status);

        // Model chip
        const modelEl = this.querySelector('.status-model');
        if (stream?.activeModel) {
            modelEl.textContent = stream.activeModel;
            modelEl.hidden = false;
        } else {
            modelEl.hidden = true;
        }

        // Totals
        const t = stream?.totals || { cost: 0, input_tokens: 0, output_tokens: 0, cache_read: 0, cache_creation: 0, turns: 0 };
        this.querySelector('.totals-cost').textContent = `$${t.cost.toFixed(4)}`;
        this.querySelector('.totals-tokens').textContent = `${this.formatNum(t.input_tokens)} in / ${this.formatNum(t.output_tokens)} out`;

        const cacheTotal = t.cache_read + t.cache_creation;
        const cacheEl = this.querySelector('.totals-cache');
        if (cacheTotal > 0) {
            cacheEl.hidden = false;
            cacheEl.textContent = `${this.formatNum(cacheTotal)} cached`;
        } else {
            cacheEl.hidden = true;
        }

        this.querySelector('.totals-turns').textContent = `${t.turns} ${t.turns === 1 ? 'turn' : 'turns'}`;
    }

    statusLabel(status) {
        switch (status) {
            case 'running': return 'working…';
            case 'ready': return 'idle';
            case 'creating':
            case 'loading': return 'starting…';
            case 'paused': return 'paused';
            case 'error': return 'error';
            case 'deleted': return 'deleted';
            default: return status;
        }
    }

    formatNum(n) {
        if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
        if (n >= 1_000) return (n / 1_000).toFixed(1) + 'k';
        return String(n);
    }

    clearOutput() {
        this.querySelector('#output-area').innerHTML = '';
    }

    // Check if user is scrolled near the bottom (within 100px)
    isNearBottom() {
        const output = this.querySelector('#output-area');
        if (!output) return true;
        const threshold = 100;
        return output.scrollHeight - output.scrollTop - output.clientHeight < threshold;
    }

    // Scroll to bottom of output area
    scrollToBottom() {
        const output = this.querySelector('#output-area');
        if (output) {
            output.scrollTop = output.scrollHeight;
        }
    }

    // Auto-scroll if user was already near bottom
    autoScroll() {
        if (this._wasNearBottom) {
            this.scrollToBottom();
        }
    }

    // Call before appending content to check scroll position
    checkScrollPosition() {
        this._wasNearBottom = this.isNearBottom();
    }

    hasMeaningfulOutput() {
        const output = this.querySelector('#output-area');
        return output.querySelector('.turn-user, .event:not(.event-system_init)') !== null;
    }

    appendNote(text) {
        this.checkScrollPosition();
        const output = this.querySelector('#output-area');
        const div = document.createElement('div');
        div.className = 'note';
        div.textContent = text;
        output.appendChild(div);
        this.autoScroll();
    }

    startUserTurn(event) {
        this.checkScrollPosition();
        const output = this.querySelector('#output-area');
        const turn = document.createElement('div');
        turn.className = 'turn turn-user open';

        const text = event.text ?? '';
        const isLong = text.length > 200;

        if (isLong) {
            turn.classList.add('turn-long-prompt');
        }

        const header = document.createElement('div');
        header.className = 'turn-header';

        header.appendChild(this.makeToggleButton(turn));
        header.appendChild(this.makeLabelSpan('user', event.ts));

        const pre = document.createElement('pre');
        if (isLong) {
            let truncated = text.slice(0, 200);
            const lastSpace = truncated.lastIndexOf(' ');
            if (lastSpace > 150) truncated = truncated.slice(0, lastSpace);
            truncated += '…';

            pre.textContent = truncated;
            pre.dataset.fullText = text;
            pre.dataset.truncatedText = truncated;

            const toggle = document.createElement('button');
            toggle.type = 'button';
            toggle.className = 'prompt-toggle';
            toggle.textContent = 'show more';
            toggle.addEventListener('click', () => {
                const isExpanded = pre.classList.toggle('prompt-expanded');
                pre.textContent = isExpanded ? pre.dataset.fullText : pre.dataset.truncatedText;
                toggle.textContent = isExpanded ? 'show less' : 'show more';

                if (isExpanded) {
                    requestAnimationFrame(() => {
                        requestAnimationFrame(() => {
                            header.style.setProperty('--header-height', header.offsetHeight + 'px');
                        });
                    });
                } else {
                    header.style.removeProperty('--header-height');
                }
            });

            header.appendChild(pre);
            header.appendChild(toggle);
        } else {
            pre.textContent = text;
            header.appendChild(pre);
        }

        const body = document.createElement('div');
        body.className = 'turn-body';

        turn.appendChild(header);
        turn.appendChild(body);
        output.appendChild(turn);

        return turn;
    }

    makeToggleButton(turn) {
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'turn-toggle';
        btn.setAttribute('aria-label', 'Toggle turn');
        btn.textContent = '▾';
        btn.addEventListener('click', () => {
            turn.classList.toggle('open');
        });
        return btn;
    }

    makeLabelSpan(text, ts, toggleTarget = null) {
        const span = document.createElement('span');
        span.className = 'label';

        if (toggleTarget) {
            span.appendChild(this.makeToggleButton(toggleTarget));
        }

        const labelText = document.createElement('span');
        labelText.textContent = text;
        span.appendChild(labelText);

        if (ts) {
            const time = document.createElement('time');
            time.className = 'ts';
            time.dateTime = ts;
            time.textContent = this.formatTimestamp(ts);
            span.appendChild(time);
        }

        return span;
    }

    formatTimestamp(iso) {
        try {
            return new Date(iso).toLocaleTimeString();
        } catch {
            return '';
        }
    }

    getOrCreateCurrentTurnBody() {
        const output = this.querySelector('#output-area');
        const last = output.lastElementChild;
        if (last && last.classList.contains('turn')) {
            return last.querySelector(':scope > .turn-body');
        }

        // Create a session turn for pre-prompt events
        const turn = document.createElement('div');
        turn.className = 'turn turn-session open';

        const header = document.createElement('div');
        header.className = 'turn-header';
        header.appendChild(this.makeToggleButton(turn));
        header.appendChild(this.makeLabelSpan('session'));

        const body = document.createElement('div');
        body.className = 'turn-body';

        turn.appendChild(header);
        turn.appendChild(body);
        output.appendChild(turn);

        return body;
    }

    appendEventToCurrentTurn(event) {
        this.checkScrollPosition();
        const body = this.getOrCreateCurrentTurnBody();
        const el = this.createEventElement(event);
        body.appendChild(el);
        this.autoScroll();
    }

    createEventElement(event) {
        const el = document.createElement('div');
        el.className = `event event-${event.type}`;

        if (this.defaultOpenForEvent(event.type)) {
            el.classList.add('open');
        }

        const labelEl = this.makeLabelSpan(this.labelFor(event), event.ts, el);
        el.appendChild(labelEl);

        const bodyText = (event.type === 'system_init')
            ? JSON.stringify(event.data ?? {}, null, 2)
            : this.bodyFor(event);

        // Preview for tool events
        if (event.type === 'tool_use' || event.type === 'tool_result') {
            const preview = document.createElement('span');
            preview.className = 'event-preview';
            preview.textContent = this.shortPreview(bodyText);
            labelEl.appendChild(preview);
        }

        const bodyEl = document.createElement('pre');
        bodyEl.className = 'event-body';
        bodyEl.textContent = bodyText;
        el.appendChild(bodyEl);

        return el;
    }

    defaultOpenForEvent(type) {
        return type !== 'tool_use' && type !== 'tool_result' && type !== 'system_init';
    }

    labelFor(event) {
        switch (event.type) {
            case 'assistant_text': return 'assistant';
            case 'thinking': return 'thinking';
            case 'tool_use': return `tool · ${event.name || 'call'}`;
            case 'tool_result': return `tool · result`;
            case 'result': return `result${event.is_error ? ' (error)' : ''}`;
            case 'error': return 'error';
            case 'system_init': return 'system · init';
            default: return event.type;
        }
    }

    bodyFor(event) {
        switch (event.type) {
            case 'assistant_text':
            case 'thinking':
                return event.text ?? '';
            case 'tool_use':
                return JSON.stringify(event.input ?? {}, null, 2);
            case 'tool_result':
                return event.output ?? '';
            case 'result':
                return [
                    event.subtype && `subtype: ${event.subtype}`,
                    event.duration_ms != null && `duration: ${event.duration_ms}ms`,
                    event.num_turns != null && `turns: ${event.num_turns}`,
                    event.total_cost_usd != null && `cost: $${event.total_cost_usd.toFixed(4)}`,
                    event.session_id && `session: ${event.session_id}`,
                ].filter(Boolean).join('\n');
            case 'error':
                return event.message ?? JSON.stringify(event);
            default:
                return JSON.stringify(event, null, 2);
        }
    }

    shortPreview(text, max = 80) {
        const compact = text.replace(/\s+/g, ' ').trim();
        if (compact.length <= max) return compact;
        return compact.slice(0, max) + '…';
    }

    enablePrompt() {
        this.querySelector('#prompt-input').disabled = false;
        this.querySelector('#send-btn').disabled = false;
    }

    disablePrompt() {
        this.querySelector('#prompt-input').disabled = true;
        this.querySelector('#send-btn').disabled = true;
    }
}

customElements.define('am-terminal-pane', AmTerminalPane);
