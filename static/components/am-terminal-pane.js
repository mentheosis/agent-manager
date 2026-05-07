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
        this._replaying = false;     // True while bulk-rendering history; suppresses mid-render scrolls
        this._filters = {
            assistant_text: true,
            thinking: true,
            tool_use: true,
            result: true,
            system_init: true,
            error: true,
        };
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
            <div class="prompt-resize-handle" title="Drag to resize"></div>
            <form id="prompt-form">
                <textarea
                    id="prompt-input"
                    placeholder="Send a prompt (Enter to send, Shift+Enter for newline)…"
                    rows="3"
                    disabled></textarea>
                <button type="submit" id="send-btn" disabled>Send</button>
                <button type="button" id="cancel-btn" hidden>Cancel</button>
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
        const cancelBtn = this.querySelector('#cancel-btn');
        const resizeHandle = this.querySelector('.prompt-resize-handle');

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

        cancelBtn.addEventListener('click', () => this.cancelOperation());

        // Resize handle - drag to resize textarea
        let startY = 0;
        let startHeight = 0;

        const onMouseMove = (e) => {
            const delta = startY - e.clientY;
            const newHeight = Math.max(44, Math.min(400, startHeight + delta));
            input.style.height = `${newHeight}px`;
        };

        const onMouseUp = () => {
            document.removeEventListener('mousemove', onMouseMove);
            document.removeEventListener('mouseup', onMouseUp);
            resizeHandle.classList.remove('dragging');
        };

        resizeHandle.addEventListener('mousedown', (e) => {
            e.preventDefault();
            startY = e.clientY;
            startHeight = input.offsetHeight;
            resizeHandle.classList.add('dragging');
            document.addEventListener('mousemove', onMouseMove);
            document.addEventListener('mouseup', onMouseUp);
        });

        // Touch support for mobile
        resizeHandle.addEventListener('touchstart', (e) => {
            const touch = e.touches[0];
            startY = touch.clientY;
            startHeight = input.offsetHeight;
            resizeHandle.classList.add('dragging');
        }, { passive: true });

        resizeHandle.addEventListener('touchmove', (e) => {
            const touch = e.touches[0];
            const delta = startY - touch.clientY;
            const newHeight = Math.max(44, Math.min(400, startHeight + delta));
            input.style.height = `${newHeight}px`;
        }, { passive: true });

        resizeHandle.addEventListener('touchend', () => {
            resizeHandle.classList.remove('dragging');
        });

        // Listen for filter changes from toolbar
        document.addEventListener('filter-changed', (e) => {
            this._filters = { ...e.detail.filters };
            this.applyFilters();
        });
    }

    applyFilters() {
        const output = this.querySelector('#output-area');
        if (!output) return;

        // Toggle CSS classes on output area to show/hide event types
        for (const [type, visible] of Object.entries(this._filters)) {
            output.classList.toggle(`hide-${type}`, !visible);
        }
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

    async cancelOperation() {
        if (!this._instance) return;

        const cancelBtn = this.querySelector('#cancel-btn');
        cancelBtn.disabled = true;
        cancelBtn.textContent = 'Cancelling...';

        try {
            await api.abortInstance(this._instance.title);
        } catch (err) {
            this.appendNote(`Cancel failed: ${err.message}`);
        } finally {
            cancelBtn.disabled = false;
            cancelBtn.textContent = 'Cancel';
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
            const stream = streamManager.get(inst.title);

            // Render whatever history has already arrived (suppress auto-scroll
            // so partial content doesn't cause a visible incremental scroll).
            this._replaying = true;
            for (const event of stream.eventHistory) {
                this.handleEvent(event, stream);
            }
            this._replaying = false;

            if (stream.historyComplete) {
                // All history received — subscribe for live events only and scroll.
                this._unsubscribe = stream.subscribe(
                    (event) => this.handleEvent(event, stream),
                    { replay: false }
                );
                this.updateStatusBar(stream);
                this.scrollToBottom();
            } else {
                // History still in-flight from the server.  Use a loading handler
                // that suppresses auto-scroll until the history_end sentinel arrives,
                // then scrolls once and switches to the normal live handler.
                this._unsubscribe = stream.subscribe(
                    (event) => this._handleOnLoad(event, stream),
                    { replay: false }
                );
                this.updateStatusBar(stream);
            }
        }
    }

    // Used during initial load when history events are still arriving from the
    // server.  Renders events silently (no auto-scroll) until history_end, then
    // scrolls once and hands off to the normal live handler.
    _handleOnLoad(event, stream) {
        if (event.type === 'history_end') {
            this.scrollToBottom();
            // Capture which subscription we're replacing so a reconnect that
            // fires between now and the setTimeout can't tear down the wrong one.
            const capturedUnsub = this._unsubscribe;
            // Defer the subscription swap so we're not modifying the listener
            // set from inside the emit() loop that called this function.
            setTimeout(() => {
                // Only swap if the subscription hasn't changed (e.g. instance switch
                // or a reconnect that already re-subscribed via handleEvent).
                if (this._unsubscribe === capturedUnsub) {
                    if (this._unsubscribe) this._unsubscribe();
                    this._unsubscribe = stream.subscribe(
                        (ev) => this.handleEvent(ev, stream),
                        { replay: false }
                    );
                }
            }, 0);
            return;
        }
        // Render without auto-scroll — history is still arriving.
        this._replaying = true;
        this.handleEvent(event, stream);
        this._replaying = false;
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
                this.appendNote('Connection lost — reconnecting…');
            } else if (event.status === 'error') {
                this.appendNote('Connection error — reconnecting…');
            } else if (event.status === 'reconnected') {
                // The stream has successfully reconnected and replayed history.
                // Clear the terminal and re-render from the fresh event history.
                // Preserve scroll position - only scroll to bottom if user was already there.
                const wasNearBottom = this.isNearBottom();
                const stream = streamManager.get(this._instance.title);
                this.clearOutput();
                this._replaying = true;
                for (const ev of stream.eventHistory) {
                    this.handleEvent(ev, stream);
                }
                this._replaying = false;
                this.updateStatusBar(stream);
                if (wasNearBottom) {
                    this.scrollToBottom();
                }
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
        const cancelBtn = this.querySelector('#cancel-btn');

        if (status === 'running') {
            spinner.style.display = '';
            dot.style.display = 'none';
            cancelBtn.hidden = false;
        } else {
            spinner.style.display = 'none';
            dot.style.display = '';
            cancelBtn.hidden = true;
        }

        dot.className = `status-dot ${status}`;
        text.className = `status-text ${status}`;
        text.textContent = this.statusLabel(status, this._instance?.instance_type);

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

    statusLabel(status, instanceType = null) {
        // Loop instances have slightly different labels
        if (instanceType === 'loop') {
            switch (status) {
                case 'running': return 'orchestrating…';
                case 'ready': return 'idle';
                case 'paused': return 'paused';
                default: break;  // Fall through to default handling
            }
        }

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
        if (output) output.scrollTop = output.scrollHeight;
    }

    // Auto-scroll if user was already near bottom (no-op during history replay)
    autoScroll() {
        if (!this._replaying && this._wasNearBottom) {
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

        // Scroll-to button
        const scrollBtn = document.createElement('button');
        scrollBtn.type = 'button';
        scrollBtn.className = 'scroll-to-btn';
        scrollBtn.textContent = 'scroll to';
        scrollBtn.title = 'Scroll this turn to the top';
        scrollBtn.addEventListener('click', () => {
            turn.scrollIntoView({ behavior: 'smooth', block: 'start' });
        });

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
            header.appendChild(scrollBtn);
            header.appendChild(toggle);
        } else {
            pre.textContent = text;
            header.appendChild(pre);
            header.appendChild(scrollBtn);
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

    makeLabelSpan(text, ts, toggleTarget = null, statusPlaceholder = false) {
        const span = document.createElement('span');
        span.className = 'label';

        if (toggleTarget) {
            span.appendChild(this.makeToggleButton(toggleTarget));
        }

        const labelText = document.createElement('span');
        labelText.textContent = text;
        span.appendChild(labelText);

        // Status indicator placeholder (inserted before timestamp)
        if (statusPlaceholder) {
            const status = document.createElement('span');
            status.className = 'tool-status pending';
            status.title = 'Pending';
            span.appendChild(status);
        }

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

        // Handle tool_result specially - nest it under the matching tool_use
        if (event.type === 'tool_result' && event.tool_id) {
            const toolUse = this.querySelector(`.event-tool_use[data-tool-id="${CSS.escape(event.tool_id)}"]`);
            if (toolUse) {
                const resultEl = this.createToolResultElement(event);
                toolUse.appendChild(resultEl);

                // Update tool_use status indicator
                const statusEl = toolUse.querySelector('.tool-status');
                if (statusEl) {
                    statusEl.classList.remove('pending');
                    statusEl.classList.add(event.is_error ? 'error' : 'success');
                    statusEl.title = event.is_error ? 'Error' : 'Success';
                }

                this.autoScroll();
                return;
            }
            // Fall through to normal append if tool_use not found
        }

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

        // Add tool ID for matching results
        if (event.type === 'tool_use' && event.id) {
            el.dataset.toolId = event.id;
        }

        // For tool_use, add status indicator before timestamp
        const hasStatusIndicator = event.type === 'tool_use';
        const labelEl = this.makeLabelSpan(this.labelFor(event), event.ts, el, hasStatusIndicator);

        el.appendChild(labelEl);

        const bodyText = (event.type === 'system_init')
            ? JSON.stringify(event.data ?? {}, null, 2)
            : this.bodyFor(event);

        // Preview for collapsed events (not tool_result since it's nested now)
        if (event.type === 'tool_use' || event.type === 'result') {
            const preview = document.createElement('span');
            preview.className = 'event-preview';
            // For tool_use, show a more useful preview based on the tool type
            if (event.type === 'tool_use') {
                preview.textContent = this.toolUsePreview(event);
            } else {
                preview.textContent = this.shortPreview(bodyText);
            }
            labelEl.appendChild(preview);
        }

        const bodyEl = document.createElement('pre');
        bodyEl.className = 'event-body';
        bodyEl.textContent = bodyText;
        el.appendChild(bodyEl);

        return el;
    }

    createToolResultElement(event) {
        const el = document.createElement('div');
        el.className = `event-nested tool-result ${event.is_error ? 'error' : 'success'}`;

        const labelEl = document.createElement('span');
        labelEl.className = 'nested-label';

        const labelText = document.createElement('span');
        labelText.textContent = event.is_error ? '✗ error' : '✓ result';
        labelEl.appendChild(labelText);

        // Add timestamp if available
        if (event.ts) {
            const time = document.createElement('time');
            time.className = 'ts';
            time.dateTime = event.ts;
            time.textContent = this.formatTimestamp(event.ts);
            labelEl.appendChild(time);
        }

        const bodyEl = document.createElement('pre');
        bodyEl.className = 'nested-body';
        bodyEl.textContent = event.output ?? '';

        el.appendChild(labelEl);
        el.appendChild(bodyEl);

        return el;
    }

    defaultOpenForEvent(type) {
        return type !== 'tool_use' && type !== 'tool_result' && type !== 'system_init' && type !== 'result';
    }

    labelFor(event) {
        switch (event.type) {
            case 'assistant_text': return 'assistant';
            case 'thinking': return 'thinking';
            case 'tool_use': return `tool · ${event.name || 'call'}`;
            case 'tool_result': return `tool · result`;
            case 'result': return `result${event.is_error ? ' (error)' : ''}`;
            case 'error': return 'error';
            case 'aborted': return 'cancelled';
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
            case 'result': {
                const usage = event.usage || event.data?.usage;
                return [
                    event.subtype && `subtype: ${event.subtype}`,
                    event.duration_ms != null && `duration: ${event.duration_ms}ms`,
                    event.num_turns != null && `turns: ${event.num_turns}`,
                    usage?.input_tokens != null && `input tokens: ${usage.input_tokens.toLocaleString()}`,
                    usage?.output_tokens != null && `output tokens: ${usage.output_tokens.toLocaleString()}`,
                    (usage?.cache_read_input_tokens || usage?.cache_read) && `cache read: ${(usage.cache_read_input_tokens || usage.cache_read).toLocaleString()}`,
                    (usage?.cache_creation_input_tokens || usage?.cache_creation) && `cache creation: ${(usage.cache_creation_input_tokens || usage.cache_creation).toLocaleString()}`,
                    event.total_cost_usd != null && `cost: $${event.total_cost_usd.toFixed(4)}`,
                    event.session_id && `session: ${event.session_id}`,
                ].filter(Boolean).join('\n');
            }
            case 'error':
            case 'aborted':
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

    toolUsePreview(event) {
        const input = event.input || {};
        const name = event.name || '';

        // Tool-specific previews
        switch (name) {
            case 'Bash':
                return this.shortPreview(input.command || '', 60);
            case 'Read':
                return input.file_path || '';
            case 'Write':
                return input.file_path || '';
            case 'Edit':
                return input.file_path || '';
            case 'Glob':
                return input.pattern || '';
            case 'Grep':
                return `${input.pattern || ''} ${input.path ? 'in ' + input.path : ''}`.trim();
            case 'Agent':
                return input.description || this.shortPreview(input.prompt || '', 50);
            default:
                // Generic: show first string value or JSON preview
                const firstVal = Object.values(input).find(v => typeof v === 'string');
                if (firstVal) return this.shortPreview(firstVal, 60);
                return this.shortPreview(JSON.stringify(input), 60);
        }
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
