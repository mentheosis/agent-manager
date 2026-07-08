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
        this._historyLoadToken = 0;   // Invalidates async history rendering after instance switches
        this._pendingImages = [];    // Images to send with next prompt [{media_type, data}]
        this._filters = {
            assistant_text: true,
            thinking: true,
            tool_use: true,
            result: true,
            system_init: true,
            error: true,
        };
        this._onResize = () => this.updatePromptPlaceholder();
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
                <span class="status-model" hidden title="Active model reported by the provider"></span>
                <span class="status-totals">
                    <span class="totals-tokens" title="cumulative input / output tokens">0 in / 0 out</span>
                    <span class="totals-cache" title="cached input tokens (read / created)" hidden>0 cached</span>
                    <span class="totals-cost" title="cumulative session cost in USD">$0.0000</span>
                    <span class="totals-turns">0 turns</span>
                </span>
            </div>
            <div class="prompt-resize-handle" title="Drag to resize"></div>
            <form id="prompt-form">
                <div id="image-preview-area" hidden></div>
                <textarea
                    id="prompt-input"
                    placeholder="Send a prompt (Enter to send, Shift+Enter for newline, paste images)..."
                    rows="3"
                    disabled></textarea>
                <button type="submit" id="send-btn" disabled>Send</button>
                <button type="button" id="cancel-btn" hidden>Cancel</button>
            </form>
        `;

        this.setupPromptForm();
        this.updatePromptPlaceholder();
    }

    disconnectedCallback() {
        if (this._unsubscribe) {
            this._unsubscribe();
            this._unsubscribe = null;
        }
        window.removeEventListener('resize', this._onResize);
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
            if (this.shouldSubmitOnEnter(e)) {
                e.preventDefault();
                this.submitPrompt();
            }
        });

        // Handle image paste
        input.addEventListener('paste', (e) => {
            const items = e.clipboardData?.items;
            if (!items) return;

            for (const item of items) {
                if (item.type.startsWith('image/')) {
                    e.preventDefault();
                    const file = item.getAsFile();
                    if (file) this.addPendingImage(file);
                    return;  // Only handle first image
                }
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

        window.addEventListener('resize', this._onResize);
    }

    shouldSubmitOnEnter(event) {
        if (event.key !== 'Enter' || event.shiftKey || event.isComposing) return false;
        if (this.isMobileInputMode()) {
            return event.ctrlKey || event.metaKey;
        }
        return true;
    }

    isMobileInputMode() {
        const coarsePointer = window.matchMedia?.('(pointer: coarse)').matches;
        const narrowViewport = window.matchMedia?.('(max-width: 900px)').matches;
        const mobileUserAgent = /Android|iPhone|iPad|iPod/i.test(navigator.userAgent || '');
        return mobileUserAgent || (coarsePointer && narrowViewport);
    }

    updatePromptPlaceholder() {
        const input = this.querySelector('#prompt-input');
        if (!input) return;
        input.placeholder = this.isMobileInputMode()
            ? 'Send a prompt (Return for newline, tap Send to submit, paste images)...'
            : 'Send a prompt (Enter to send, Shift+Enter for newline, paste images)...';
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

        // Need either text or images
        if (!text && this._pendingImages.length === 0) return;

        input.value = '';

        // Prepare images for sending (strip _preview field)
        const images = this._pendingImages.map(({ media_type, data }) => ({ media_type, data }));
        this.clearPendingImages();

        try {
            await api.sendPrompt(this._instance.title, text, images.length > 0 ? images : null);
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
        const loadToken = ++this._historyLoadToken;
        // Unsubscribe from old stream
        if (this._unsubscribe) {
            this._unsubscribe();
            this._unsubscribe = null;
        }

        this._instance = inst;
        this.clearOutput();

        if (inst) {
            const stream = streamManager.get(inst.title);
            this.showHistoryLoading(inst);
            this.updateStatusBar(stream);

            this.renderInitialHistory(stream, loadToken);
        }
    }

    async renderInitialHistory(stream, loadToken) {
        await this.nextFrame();
        if (loadToken !== this._historyLoadToken || !this._instance) return;

        const initialHistory = stream.eventHistory.slice();
        this.clearOutput();
        this._replaying = true;

        let sliceStartedAt = performance.now();
        const batchSize = 75;
        for (let i = 0; i < initialHistory.length; i++) {
            if (loadToken !== this._historyLoadToken || stream.title !== this._instance?.title) {
                this._replaying = false;
                return;
            }

            this.handleEvent(initialHistory[i], stream);

            const batchBoundary = (i + 1) % batchSize === 0;
            const timeSliceExpired = performance.now() - sliceStartedAt > 16;
            if (batchBoundary || timeSliceExpired) {
                await this.nextFrame();
                sliceStartedAt = performance.now();
            }
        }

        // Events that arrived while the snapshot was rendering are buffered by
        // the stream. Render them before subscribing so there is no display gap.
        const missedEvents = stream.eventHistory.slice(initialHistory.length);
        for (const event of missedEvents) {
            if (loadToken !== this._historyLoadToken || stream.title !== this._instance?.title) {
                this._replaying = false;
                return;
            }
            this.handleEvent(event, stream);
        }

        this._replaying = false;
        if (loadToken !== this._historyLoadToken || stream.title !== this._instance?.title) return;

        this.updateStatusBar(stream);
        if (stream.historyComplete) {
            this.subscribeLive(stream);
            this.scrollToBottom();
        } else {
            this._unsubscribe = stream.subscribe(
                (event) => this._handleOnLoad(event, stream),
                { replay: false }
            );
            if (!this.hasMeaningfulOutput()) {
                this.showHistoryLoading(this._instance);
            }
        }
    }

    nextFrame() {
        return new Promise((resolve) => requestAnimationFrame(resolve));
    }

    showHistoryLoading(inst) {
        const output = this.querySelector('#output-area');
        if (!output) return;
        output.innerHTML = '';
        const loading = document.createElement('div');
        loading.className = 'history-loading';
        loading.setAttribute('role', 'status');
        loading.setAttribute('aria-live', 'polite');

        const spinner = document.createElement('div');
        spinner.className = 'history-loading-spinner';
        spinner.setAttribute('aria-hidden', 'true');
        loading.appendChild(spinner);

        const text = document.createElement('span');
        const label = inst?.display_title || inst?.title || 'conversation';
        text.textContent = `Loading ${label}...`;
        loading.appendChild(text);

        output.appendChild(loading);
    }

    switchToLiveHandler(stream) {
        const capturedUnsub = this._unsubscribe;
        setTimeout(() => {
            if (this._unsubscribe === capturedUnsub && this._instance?.title === stream.title) {
                if (this._unsubscribe) this._unsubscribe();
                this.subscribeLive(stream);
            }
        }, 0);
    }

    subscribeLive(stream) {
        this._unsubscribe = stream.subscribe(
            (ev) => this.handleEvent(ev, stream),
            { replay: false }
        );
    }

    // Used during initial load when history events are still arriving from the
    // server.  Renders events silently (no auto-scroll) until history_end, then
    // scrolls once and hands off to the normal live handler.
    _handleOnLoad(event, stream) {
        if (event.type === 'history_end') {
            this.scrollToBottom();
            this.switchToLiveHandler(stream);
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
                this.appendNote(`${this.providerLabel()} is ready. Send a prompt to get started.`);
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
        const t = stream?.totals || { cost: 0, input_tokens: 0, output_tokens: 0, cache_read: 0, cache_creation: 0, cost_estimated: false, turns: 0 };
        this.querySelector('.totals-cost').textContent = `${t.cost_estimated ? '~' : ''}$${t.cost.toFixed(4)}`;
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

    providerLabel() {
        const provider = this._instance?.provider || this._instance?.instance_type || 'agent';
        switch (provider) {
            case 'claude': return 'Claude';
            case 'codex': return 'Codex';
            case 'loop': return 'Agent';
            default: return 'Agent';
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
        const images = event.images || [];
        const collapsedLimit = this.collapsedPromptLimit();
        const isLong = text.length > collapsedLimit;

        if (isLong) {
            turn.classList.add('turn-long-prompt');
        }

        const header = document.createElement('div');
        header.className = 'turn-header';

        header.appendChild(this.makeToggleButton(turn));

        // Build label with optional image count
        const labelText = images.length > 0 ? `user · ${images.length} image${images.length > 1 ? 's' : ''}` : 'user';
        header.appendChild(this.makeLabelSpan(labelText, event.ts));

        // Render image thumbnails if present
        if (images.length > 0) {
            const imageRow = document.createElement('div');
            imageRow.className = 'user-prompt-images';
            for (const img of images) {
                const imgEl = document.createElement('img');
                imgEl.src = `data:${img.media_type};base64,${img.data}`;
                imgEl.alt = 'Attached image';
                imgEl.className = 'user-prompt-image';
                imgEl.title = 'Click to enlarge';
                imgEl.addEventListener('click', () => this.showImageModal(imgEl.src));
                imageRow.appendChild(imgEl);
            }
            header.appendChild(imageRow);
        }

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
            const truncated = this.truncatePromptText(text, collapsedLimit);

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

    collapsedPromptLimit() {
        return window.matchMedia('(max-width: 768px)').matches ? 50 : 200;
    }

    truncatePromptText(text, limit) {
        let truncated = text.slice(0, limit);
        const lastSpace = truncated.lastIndexOf(' ');
        if (lastSpace > Math.floor(limit * 0.75)) {
            truncated = truncated.slice(0, lastSpace);
        }
        return truncated + '…';
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
            const toolUses = this.querySelectorAll(`.event-tool_use[data-tool-id="${CSS.escape(event.tool_id)}"]`);
            const toolUse = toolUses[toolUses.length - 1];
            if (toolUse) {
                const resultEl = this.createToolResultElement(event);
                const rawContent = toolUse.querySelector(':scope > .tool-raw-details > .tool-raw-content');
                if (rawContent) {
                    rawContent.appendChild(resultEl);
                } else {
                    toolUse.appendChild(resultEl);
                }

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
            if (this.isOrphanTranscriptMcpResult(event)) {
                this.autoScroll();
                return;
            }
            // Fall through to normal append if tool_use not found
        }

        const body = this.getOrCreateCurrentTurnBody();
        if (this.isDuplicateAssistantText(body, event) || this.isDuplicateResult(body, event)) {
            this.autoScroll();
            return;
        }
        const el = this.createEventElement(event);
        body.appendChild(el);
        this.autoScroll();
    }

    isDuplicateAssistantText(body, event) {
        if (event.type !== 'assistant_text') return false;
        const text = event.text ?? '';
        if (!text) return false;
        const last = body.lastElementChild;
        if (!last || !last.classList.contains('event-assistant_text')) return false;
        const lastBody = last.querySelector(':scope > .event-body');
        return lastBody?.textContent === text;
    }

    isDuplicateResult(body, event) {
        if (event.type !== 'result') return false;
        const last = body.lastElementChild;
        return Boolean(last?.classList.contains('event-result'));
    }

    createEventElement(event) {
        const el = document.createElement('div');
        el.className = `event event-${event.type}`;

        if (this.defaultOpenForEvent(event.type) || this.isSummaryToolUse(event)) {
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

        const toolSummaryText = this.summaryTextForToolUse(event);
        if (event.type === 'tool_use' && toolSummaryText) {
            const displayEl = document.createElement('pre');
            displayEl.className = 'tool-display-text';
            displayEl.textContent = toolSummaryText;
            el.appendChild(displayEl);
        }

        if (this.isSummaryToolUse(event)) {
            el.appendChild(this.createToolRawDetails(bodyText));
        } else {
            const bodyEl = event.type === 'artifact'
                ? this.createArtifactBody(event)
                : document.createElement('pre');
            bodyEl.className = event.type === 'artifact'
                ? 'event-body artifact-body'
                : 'event-body';
            if (event.type !== 'artifact') {
                bodyEl.textContent = bodyText;
            }
            el.appendChild(bodyEl);
        }

        return el;
    }

    createToolRawDetails(bodyText) {
        const wrapper = document.createElement('div');
        wrapper.className = 'tool-raw-details';

        const button = document.createElement('button');
        button.type = 'button';
        button.className = 'tool-raw-toggle';
        button.textContent = 'show details';
        button.addEventListener('click', () => {
            const open = wrapper.classList.toggle('open');
            button.textContent = open ? 'hide details' : 'show details';
        });

        const content = document.createElement('div');
        content.className = 'tool-raw-content';

        const bodyEl = document.createElement('pre');
        bodyEl.className = 'event-body';
        bodyEl.textContent = bodyText;
        content.appendChild(bodyEl);

        wrapper.appendChild(button);
        wrapper.appendChild(content);
        return wrapper;
    }

    createArtifactBody(event) {
        const body = document.createElement('div');
        const url = this.artifactUrl(event);

        if (event.artifact_type === 'image') {
            const link = document.createElement('a');
            link.href = url;
            link.target = '_blank';
            link.rel = 'noopener noreferrer';
            link.className = 'artifact-image-link';

            const img = document.createElement('img');
            img.className = 'artifact-image';
            img.src = url;
            img.alt = event.title || event.path || 'Image artifact';
            img.loading = 'lazy';
            link.appendChild(img);
            body.appendChild(link);
        } else if (event.artifact_type === 'video' || this.isVideoArtifact(event)) {
            const video = document.createElement('video');
            video.className = 'artifact-video';
            video.src = url;
            video.controls = true;
            video.preload = 'metadata';
            video.playsInline = true;
            body.appendChild(video);

            const link = document.createElement('a');
            link.href = url;
            link.target = '_blank';
            link.rel = 'noopener noreferrer';
            link.className = 'artifact-open-link';
            link.textContent = event.title || event.path || 'Open video';
            body.appendChild(link);
        } else {
            const card = document.createElement('a');
            card.href = url;
            card.target = '_blank';
            card.rel = 'noopener noreferrer';
            card.className = 'artifact-file-link';
            card.textContent = event.title || event.path || 'Open artifact';
            body.appendChild(card);
        }

        const meta = document.createElement('div');
        meta.className = 'artifact-meta';
        meta.textContent = [event.mime_type, event.path].filter(Boolean).join(' · ');
        body.appendChild(meta);

        return body;
    }

    artifactUrl(event) {
        const artifactId = encodeURIComponent(event.artifact_id || '');
        const cacheKey = encodeURIComponent(this.artifactCacheKey(event));
        const suffix = cacheKey ? `?v=${cacheKey}` : '';
        if (this._instance?.title) {
            return `/api/instances/${encodeURIComponent(this._instance.title)}/artifacts/${artifactId}${suffix}`;
        }
        return `/api/artifacts/images/${artifactId}${suffix}`;
    }

    artifactCacheKey(event) {
        return String(event.seq ?? event.ts ?? Date.now());
    }

    isVideoArtifact(event) {
        const mime = String(event.mime_type || '').toLowerCase();
        const path = String(event.path || '').toLowerCase();
        return mime.startsWith('video/')
            || /\.(mp4|webm|ogv|ogg|mov|m4v)$/.test(path);
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

    isOrphanTranscriptMcpResult(event) {
        const output = String(event.output ?? '');
        return output.includes("'Ok': {'content':")
            || output.includes('"Ok": {"content":')
            || output.includes('"Ok":{"content":');
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
            case 'artifact':
                if (event.artifact_type === 'image') return 'image';
                if (event.artifact_type === 'video' || this.isVideoArtifact(event)) return 'video';
                return 'artifact';
            case 'result': return `result${event.is_error ? ' (error)' : ''}`;
            case 'command_result': return event.is_error ? 'command · error' : 'command · result';
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
                return this.formatToolInput(event.input);
            case 'tool_result':
                return event.output ?? '';
            case 'artifact':
                return [event.title, event.path].filter(Boolean).join('\n');
            case 'result': {
                const usage = event.usage || event.data?.usage;
                const context = event.context || event.data?.context;
                const rateLimits = event.rate_limits || event.data?.rate_limits;
                const errorDetails = event.error_details || event.data?.error_details;
                return [
                    event.subtype && `subtype: ${event.subtype}`,
                    event.duration_ms != null && `duration: ${event.duration_ms}ms`,
                    event.num_turns != null && `turns: ${event.num_turns}`,
                    context?.total_tokens != null && context?.context_window != null && `context: ${context.total_tokens.toLocaleString()} / ${context.context_window.toLocaleString()} tokens${context.used_percent != null ? ` (${context.used_percent}%)` : ''}`,
                    context?.remaining_tokens != null && `context remaining: ${context.remaining_tokens.toLocaleString()} tokens`,
                    usage?.input_tokens != null && `input tokens: ${usage.input_tokens.toLocaleString()}`,
                    usage?.output_tokens != null && `output tokens: ${usage.output_tokens.toLocaleString()}`,
                    usage?.total_tokens != null && `total tokens: ${usage.total_tokens.toLocaleString()}`,
                    usage?.reasoning_output_tokens != null && `reasoning output tokens: ${usage.reasoning_output_tokens.toLocaleString()}`,
                    (usage?.cached_input_tokens || usage?.cache_read_input_tokens || usage?.cache_read) && `cache read: ${(usage.cached_input_tokens || usage.cache_read_input_tokens || usage.cache_read).toLocaleString()}`,
                    (usage?.cache_creation_input_tokens || usage?.cache_creation) && `cache creation: ${(usage.cache_creation_input_tokens || usage.cache_creation).toLocaleString()}`,
                    rateLimits?.primary?.used_percent != null && `primary limit: ${rateLimits.primary.used_percent}%${rateLimits.primary.resets_at_iso ? `, resets ${rateLimits.primary.resets_at_iso}` : ''}`,
                    rateLimits?.secondary?.used_percent != null && `secondary limit: ${rateLimits.secondary.used_percent}%${rateLimits.secondary.resets_at_iso ? `, resets ${rateLimits.secondary.resets_at_iso}` : ''}`,
                    errorDetails?.message && `error: ${errorDetails.message}`,
                    errorDetails?.code && `error code: ${errorDetails.code}`,
                    errorDetails?.error_type && `error type: ${errorDetails.error_type}`,
                    errorDetails?.param && `error param: ${errorDetails.param}`,
                    errorDetails?.returncode != null && `exit code: ${errorDetails.returncode}`,
                    errorDetails?.stderr_tail && `stderr:\n${errorDetails.stderr_tail}`,
                    event.total_cost_usd != null && `cost: $${event.total_cost_usd.toFixed(4)}`,
                    event.total_cost_usd == null && event.estimated_cost_usd != null && !this.isCodexCumulativeUsage(usage) && `estimated cost: ~$${event.estimated_cost_usd.toFixed(4)}${event.estimated_cost_model ? ` (${event.estimated_cost_model})` : ''}`,
                    event.session_id && `session: ${event.session_id}`,
                ].filter(Boolean).join('\n');
            }
            case 'command_result':
                return [
                    event.command && `command: ${event.command}`,
                    event.message,
                ].filter(Boolean).join('\n');
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
        const normalizedName = name.toLowerCase();
        const summaryText = this.summaryTextForToolUse(event);
        if (summaryText) return this.shortPreview(summaryText, 60);
        if (typeof input === 'string') return this.shortPreview(input, 60);

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
                if (normalizedName.includes('web_search')) {
                    return this.shortPreview(input.query || input.search_query || this.firstStringValue(input), 60);
                }
                // Generic: show first string value or JSON preview
                const firstVal = this.firstStringValue(input);
                if (firstVal) return this.shortPreview(firstVal, 60);
                return this.shortPreview(JSON.stringify(input), 60);
        }
    }

    isSummaryToolUse(event) {
        return event.type === 'tool_use' && Boolean(this.summaryTextForToolUse(event));
    }

    summaryTextForToolUse(event) {
        if (event.type !== 'tool_use') return '';
        if (event.display_text) return event.display_text;

        const input = this.parseToolInput(event.input);
        if (event.name === 'update_goal') {
            if (input?.status === 'complete') return 'Goal marked complete.';
            if (input?.status === 'blocked') return 'Goal marked blocked.';
            return '';
        }
        if (event.name !== 'update_plan' || !input) return '';

        const lines = [];
        if (typeof input.explanation === 'string' && input.explanation.trim()) {
            lines.push(input.explanation.trim());
        }
        if (Array.isArray(input.plan)) {
            for (const item of input.plan) {
                if (!item || typeof item !== 'object') continue;
                if (item.status !== 'in_progress' && item.status !== 'in_progess') continue;
                if (typeof item.step === 'string' && item.step.trim()) {
                    lines.push(`In progress: ${item.step.trim()}`);
                }
            }
        }
        return lines.join('\n') || 'Plan updated.';
    }

    parseToolInput(input) {
        if (input && typeof input === 'object') return input;
        if (typeof input !== 'string') return null;
        try {
            const parsed = JSON.parse(input);
            return parsed && typeof parsed === 'object' ? parsed : null;
        } catch {
            return null;
        }
    }

    formatToolInput(input) {
        if (input == null) return '';
        if (typeof input === 'string') return input;
        return JSON.stringify(input, null, 2);
    }

    firstStringValue(input) {
        if (!input || typeof input !== 'object') return '';
        return Object.values(input).find(v => typeof v === 'string') || '';
    }

    isCodexCumulativeUsage(usage) {
        return usage && (
            Object.hasOwn(usage, 'cached_input_tokens')
            || Object.hasOwn(usage, 'reasoning_output_tokens')
        );
    }

    // --- Image handling ---

    showImageModal(src) {
        // Create or reuse modal
        let modal = document.getElementById('image-modal');
        if (!modal) {
            modal = document.createElement('div');
            modal.id = 'image-modal';
            modal.className = 'image-modal';
            modal.hidden = true;
            modal.innerHTML = `
                <div class="image-modal-backdrop"></div>
                <img class="image-modal-img" src="" alt="Enlarged image">
            `;
            modal.querySelector('.image-modal-backdrop').addEventListener('click', () => {
                modal.hidden = true;
            });
            modal.addEventListener('click', (e) => {
                if (e.target === modal) modal.hidden = true;
            });
            // Close on Escape
            document.addEventListener('keydown', (e) => {
                if (e.key === 'Escape' && !modal.hidden) {
                    modal.hidden = true;
                }
            });
            document.body.appendChild(modal);
        }
        modal.querySelector('.image-modal-img').src = src;
        modal.hidden = false;
    }

    async addPendingImage(file) {
        try {
            const base64 = await this.fileToBase64(file);
            // Remove the data:image/png;base64, prefix
            const data = base64.split(',')[1];
            const img = {
                media_type: file.type,
                data: data,
                // Keep a reference for preview
                _preview: base64
            };
            this._pendingImages.push(img);
            this.renderImagePreviews();
        } catch (err) {
            console.error('Failed to read image:', err);
            this.appendNote(`Failed to read image: ${err.message}`);
        }
    }

    fileToBase64(file) {
        return new Promise((resolve, reject) => {
            const reader = new FileReader();
            reader.onload = () => resolve(reader.result);
            reader.onerror = () => reject(reader.error);
            reader.readAsDataURL(file);
        });
    }

    renderImagePreviews() {
        const previewArea = this.querySelector('#image-preview-area');
        if (!previewArea) return;

        if (this._pendingImages.length === 0) {
            previewArea.hidden = true;
            previewArea.innerHTML = '';
            return;
        }

        previewArea.hidden = false;
        previewArea.innerHTML = '';

        this._pendingImages.forEach((img, index) => {
            const wrapper = document.createElement('div');
            wrapper.className = 'image-preview-item';

            const imgEl = document.createElement('img');
            imgEl.src = img._preview;
            imgEl.alt = `Pasted image ${index + 1}`;

            const removeBtn = document.createElement('button');
            removeBtn.type = 'button';
            removeBtn.className = 'image-remove-btn';
            removeBtn.textContent = '×';
            removeBtn.title = 'Remove image';
            removeBtn.addEventListener('click', () => this.removePendingImage(index));

            wrapper.appendChild(imgEl);
            wrapper.appendChild(removeBtn);
            previewArea.appendChild(wrapper);
        });
    }

    removePendingImage(index) {
        this._pendingImages.splice(index, 1);
        this.renderImagePreviews();
    }

    clearPendingImages() {
        this._pendingImages = [];
        this.renderImagePreviews();
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
