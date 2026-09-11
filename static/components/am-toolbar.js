/**
 * Toolbar component - mode selector, title, and action buttons.
 */

import * as api from '../lib/api.js';
import { teamFilters } from './teams/view-config.js';
import { queueFilters } from './task_queues/view-config.js';
import {
    ensureNotificationPermission,
    idleNotificationsEnabled,
    notificationsSupported,
    setIdleNotificationsEnabled,
} from '../lib/notifications.js';

class AmToolbar extends HTMLElement {
    constructor() {
        super();
        this._instance = null;
        this._filterOpen = false;
        this._queueFilters = Object.fromEntries(Object.keys(queueFilters).map(key => [key,true]));
        this._teamFilters = {user_prompt: true, assistant_text: true, tool_use: true, result: true, status: true, controller: true, error: true};
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
            <button class="toolbar-btn" id="btn-notify-idle" type="button" title="Notify when this agent becomes idle">Notify</button>
            <button class="toolbar-btn" id="btn-scroll-bottom" type="button" title="Jump to bottom">↓ Bottom</button>
            <button class="toolbar-btn loop-only" id="btn-restart-loop" type="button" title="Restart the orchestration loop">⟳ Restart Loop</button>
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
        this.querySelector('#btn-notify-idle').addEventListener('click', () => this.toggleIdleNotifications());

        this.querySelector('#btn-restart-loop').addEventListener('click', () => this.restartOrchestrator());
        this.querySelector('#btn-kill').addEventListener('click', () => this.killInstance());
    }

    get instance() {
        return this._instance;
    }

    set instance(inst) {
        this._instance = inst;
        this.update();
        this.renderFilters();
        this.dispatchFilterEvent();
    }

    update() {
        const titleInput = this.querySelector('#toolbar-title');
        const typeBadge = this.querySelector('#toolbar-type-badge');
        const isLoop = this._instance?.instance_type === 'loop' && !this.isQueueView();

        if (this._instance) {
            titleInput.value = this._instance.display_title || this._instance.title;
            titleInput.disabled = false;

            // Show type badge for loop instances
            if (this.isQueueView()) {
                typeBadge.textContent = 'task queue';
                typeBadge.hidden = false;
            } else if (isLoop) {
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

        this.updateNotifyButton();
        const managed = this.isQueueView() || !!this._instance?.queue_attempt;
        this.querySelector('#btn-kill').hidden = !!this._instance?.queue_attempt;
        this.querySelector('#btn-kill').textContent = this.isQueueView() ? 'Delete controller' : 'Kill';
        this.querySelector('#btn-notify-idle').hidden = this.isQueueView();

        // Show/hide buttons based on instance type
        for (const btn of this.querySelectorAll('.loop-only')) {
            btn.hidden = !isLoop;
        }
        for (const btn of this.querySelectorAll('.agent-only')) {
            btn.hidden = isLoop;
        }
    }

    updateNotifyButton() {
        const btn = this.querySelector('#btn-notify-idle');
        const title = this._instance?.title;
        const enabled = idleNotificationsEnabled(title);
        btn.disabled = !this._instance || !notificationsSupported();
        btn.classList.toggle('active', enabled);
        btn.textContent = enabled ? 'Notify On' : 'Notify';
        if (!notificationsSupported()) {
            btn.title = 'Browser notifications are not supported here';
        } else if (enabled) {
            btn.title = 'Disable idle notification for this conversation';
        } else {
            btn.title = 'Notify when this agent becomes idle';
        }
    }

    async toggleIdleNotifications() {
        if (!this._instance) return;
        const title = this._instance.title;
        const enabled = idleNotificationsEnabled(title);

        if (enabled) {
            setIdleNotificationsEnabled(title, false);
            this.updateNotifyButton();
            return;
        }

        const permission = await ensureNotificationPermission();
        if (permission !== 'granted') {
            alert(permission === 'unsupported'
                ? 'Browser notifications are not supported here.'
                : 'Notification permission was not granted.');
            this.updateNotifyButton();
            return;
        }

        setIdleNotificationsEnabled(title, true);
        this.updateNotifyButton();
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
        const isQueue = this._instance.controller_mode === 'task_queue';
        const isTeam = this._instance.instance_type === 'loop' && !isQueue;
        const childCount = this._instance.children?.length || 0;

        let confirmMsg = `Delete "${name}"? This will stop the session and remove all history.`;
        if (isQueue) {
            confirmMsg = `Delete controller "${name}"? This stops its workers and preserves task, attempt and worker conversation history.`;
        } else if (isTeam && childCount > 0) {
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
        if (this.isQueueView()) return;
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

        // Delegate changes because the menu changes with the selected view.
        filterMenu.addEventListener('change', (event) => {
            const checkbox = event.target;
            if (!checkbox.dataset.type) return;
            const filters = this.isQueueView() ? this._queueFilters : this.isTeamView() ? this._teamFilters : this._filters;
            filters[checkbox.dataset.type] = checkbox.checked;
            this.dispatchFilterEvent();
        });
    }

    isQueueView() { return this._instance?.controller_mode === 'task_queue'; }

    isTeamView() {
        return this._instance?.kind === 'loop' || this._instance?.instance_type === 'loop';
    }

    renderFilters() {
        const labels = this.isQueueView() ? queueFilters : this.isTeamView()
            ? teamFilters
            : {assistant_text: 'Assistant', thinking: 'Thinking', tool_use: 'Tools', result: 'Results', system_init: 'System', error: 'Errors'};
        const filters = this.filters;
        this.querySelector('#filter-menu').innerHTML = Object.entries(labels).map(([type, label]) =>
            `<label class="filter-item"><input type="checkbox" data-type="${type}" ${filters[type] ? 'checked' : ''}><span class="filter-label">${label}</span></label>`
        ).join('');
    }

    dispatchFilterEvent() {
        this.dispatchEvent(new CustomEvent('filter-changed', {
            bubbles: true,
            detail: { scope: this.isQueueView() ? 'task_queue' : this.isTeamView() ? 'team' : 'agent', filters: this.filters }
        }));
    }

    get filters() {
        return { ...(this.isQueueView() ? this._queueFilters : this.isTeamView() ? this._teamFilters : this._filters) };
    }
}

customElements.define('am-toolbar', AmToolbar);
