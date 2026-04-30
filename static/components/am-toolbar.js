/**
 * Toolbar component - mode selector, title, and action buttons.
 */

import * as api from '../lib/api.js';

class AmToolbar extends HTMLElement {
    constructor() {
        super();
        this._instance = null;
        this._filterOpen = false;
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
        this.id = 'toolbar';
        this.innerHTML = `
            <input id="toolbar-title" type="text" placeholder="Untitled" spellcheck="false">
            <span id="toolbar-type-badge" class="type-badge" hidden></span>
            <div style="flex:1"></div>
            <div class="filter-dropdown">
                <button class="toolbar-btn" id="btn-filter" type="button" title="Filter event types">Filter</button>
                <div class="filter-menu" id="filter-menu">
                    <label class="filter-item">
                        <input type="checkbox" data-type="assistant_text" checked>
                        <span class="filter-label">Assistant</span>
                    </label>
                    <label class="filter-item">
                        <input type="checkbox" data-type="thinking" checked>
                        <span class="filter-label">Thinking</span>
                    </label>
                    <label class="filter-item">
                        <input type="checkbox" data-type="tool_use" checked>
                        <span class="filter-label">Tools</span>
                    </label>
                    <label class="filter-item">
                        <input type="checkbox" data-type="result" checked>
                        <span class="filter-label">Results</span>
                    </label>
                    <label class="filter-item">
                        <input type="checkbox" data-type="system_init" checked>
                        <span class="filter-label">System</span>
                    </label>
                    <label class="filter-item">
                        <input type="checkbox" data-type="error" checked>
                        <span class="filter-label">Errors</span>
                    </label>
                </div>
            </div>
            <button class="toolbar-btn" id="btn-scroll-bottom" type="button" title="Jump to bottom">↓ Bottom</button>
            <button class="toolbar-btn loop-only" id="btn-restart-loop" type="button" title="Restart the orchestration loop">⟳ Restart Loop</button>
            <button class="toolbar-btn agent-only" id="btn-pause" type="button">Pause</button>
            <button class="toolbar-btn agent-only" id="btn-resume" type="button">Resume</button>
            <button class="toolbar-btn danger" id="btn-kill" type="button">Kill</button>
        `;

        this.setupEventListeners();
        this.setupFilterListeners();
    }

    setupEventListeners() {
        // Title input - rename on blur/enter
        const titleInput = this.querySelector('#toolbar-title');
        titleInput.addEventListener('blur', () => this.commitRename());
        titleInput.addEventListener('keydown', (e) => {
            if (e.key === 'Enter') {
                e.preventDefault();
                titleInput.blur();
            } else if (e.key === 'Escape') {
                // Revert to current name
                if (this._instance) {
                    titleInput.value = this._instance.display_title || this._instance.title;
                }
                titleInput.blur();
            }
        });

        // Jump to bottom
        this.querySelector('#btn-scroll-bottom').addEventListener('click', () => {
            this.dispatchEvent(new CustomEvent('scroll-to-bottom', { bubbles: true }));
        });

        // Action buttons
        this.querySelector('#btn-pause').addEventListener('click', () => {
            alert('Pause not implemented yet.');
        });
        this.querySelector('#btn-resume').addEventListener('click', () => {
            alert('Resume not implemented yet.');
        });
        this.querySelector('#btn-restart-loop').addEventListener('click', () => this.restartOrchestrator());
        this.querySelector('#btn-kill').addEventListener('click', () => this.killInstance());
    }

    get instance() {
        return this._instance;
    }

    set instance(inst) {
        this._instance = inst;
        this.update();
    }

    update() {
        const titleInput = this.querySelector('#toolbar-title');
        const typeBadge = this.querySelector('#toolbar-type-badge');
        const isLoop = this._instance?.instance_type === 'loop';

        if (this._instance) {
            titleInput.value = this._instance.display_title || this._instance.title;
            titleInput.disabled = false;

            // Show type badge for loop instances
            if (isLoop) {
                typeBadge.textContent = 'team';
                typeBadge.hidden = false;
            } else if (this._instance.agent_preset) {
                typeBadge.textContent = this._instance.agent_preset;
                typeBadge.hidden = false;
            } else {
                typeBadge.hidden = true;
            }
        } else {
            titleInput.value = '';
            titleInput.disabled = true;
            typeBadge.hidden = true;
        }

        // Show/hide buttons based on instance type
        for (const btn of this.querySelectorAll('.loop-only')) {
            btn.hidden = !isLoop;
        }
        for (const btn of this.querySelectorAll('.agent-only')) {
            btn.hidden = isLoop;
        }
    }

    async commitRename() {
        if (!this._instance) return;

        const titleInput = this.querySelector('#toolbar-title');
        const newTitle = titleInput.value.trim();
        const currentDisplay = this._instance.display_title || this._instance.title;

        if (!newTitle || newTitle === currentDisplay) return;

        try {
            await api.renameInstance(this._instance.title, newTitle);
            this.dispatchEvent(new CustomEvent('instance-renamed', { bubbles: true }));
        } catch (err) {
            alert(`Failed to rename: ${err.message}`);
            titleInput.value = currentDisplay;
        }
    }

    async killInstance() {
        if (!this._instance) return;

        const name = this._instance.display_title || this._instance.title;
        const isTeam = this._instance.instance_type === 'loop';
        const childCount = this._instance.children?.length || 0;

        let confirmMsg = `Delete "${name}"? This will stop the session and remove all history.`;
        if (isTeam && childCount > 0) {
            confirmMsg = `Delete team "${name}" and its ${childCount} member(s)? This will stop all sessions and remove all history.`;
        }

        if (!confirm(confirmMsg)) {
            return;
        }

        // Dispatch deleting event to update UI immediately
        this.dispatchEvent(new CustomEvent('instance-deleting', {
            bubbles: true,
            detail: {
                title: this._instance.title,
                children: isTeam ? this._instance.children : []
            }
        }));

        try {
            await api.deleteInstance(this._instance.title);
            this.dispatchEvent(new CustomEvent('instance-deleted', {
                bubbles: true,
                detail: { title: this._instance.title }
            }));
        } catch (err) {
            alert(`Failed to delete: ${err.message}`);
            // Reload instances to reset status
            this.dispatchEvent(new CustomEvent('instances-reordered', { bubbles: true }));
        }
    }

    async restartOrchestrator() {
        if (!this._instance) return;
        if (this._instance.instance_type !== 'loop') return;

        const btn = this.querySelector('#btn-restart-loop');
        const originalText = btn.textContent;
        btn.disabled = true;
        btn.textContent = 'Restarting...';

        try {
            const r = await fetch(`/api/instances/${encodeURIComponent(this._instance.title)}/orchestrator/restart`, {
                method: 'POST',
            });
            if (!r.ok) {
                const data = await r.json().catch(() => ({}));
                throw new Error(data.detail || `HTTP ${r.status}`);
            }
            const data = await r.json();
            console.log('Orchestrator restarted:', data);
        } catch (err) {
            alert(`Failed to restart orchestrator: ${err.message}`);
        } finally {
            btn.disabled = false;
            btn.textContent = originalText;
        }
    }

    setupFilterListeners() {
        const filterBtn = this.querySelector('#btn-filter');
        const filterMenu = this.querySelector('#filter-menu');

        // Toggle dropdown on button click
        filterBtn.addEventListener('click', (e) => {
            e.stopPropagation();
            this._filterOpen = !this._filterOpen;
            filterMenu.classList.toggle('open', this._filterOpen);
        });

        // Close dropdown when clicking outside
        document.addEventListener('click', (e) => {
            if (this._filterOpen && !this.querySelector('.filter-dropdown').contains(e.target)) {
                this._filterOpen = false;
                filterMenu.classList.remove('open');
            }
        });

        // Handle checkbox changes
        for (const checkbox of this.querySelectorAll('#filter-menu input[type="checkbox"]')) {
            checkbox.addEventListener('change', () => {
                const eventType = checkbox.dataset.type;
                this._filters[eventType] = checkbox.checked;
                this.dispatchFilterEvent();
            });
        }
    }

    dispatchFilterEvent() {
        this.dispatchEvent(new CustomEvent('filter-changed', {
            bubbles: true,
            detail: { filters: { ...this._filters } }
        }));
    }

    get filters() {
        return { ...this._filters };
    }
}

customElements.define('am-toolbar', AmToolbar);
