/**
 * Team Management Panel - shown for loop instances on the Conversation tab.
 * Displays team members, their status, and orchestration controls.
 */

import * as api from '../lib/api.js';
import { streamManager } from '../lib/streams.js';

class AmTeamPanel extends HTMLElement {
    constructor() {
        super();
        this._instance = null;
        this._children = [];
        this._pollInterval = null;
        this._streamUnsubscribers = [];
    }

    connectedCallback() {
        this.id = 'team-panel';
        this.render();
    }

    disconnectedCallback() {
        this.cleanup();
    }

    get instance() {
        return this._instance;
    }

    set instance(inst) {
        this.cleanup();
        this._instance = inst;

        if (inst && inst.instance_type === 'loop') {
            this.classList.add('visible');
            this.loadChildren();
            this.startPolling();
        } else {
            this.classList.remove('visible');
            this._children = [];
            this.render();
        }
    }

    cleanup() {
        if (this._pollInterval) {
            clearInterval(this._pollInterval);
            this._pollInterval = null;
        }
        for (const unsub of this._streamUnsubscribers) {
            unsub();
        }
        this._streamUnsubscribers = [];
    }

    startPolling() {
        // Poll for children updates every 5 seconds
        this._pollInterval = setInterval(() => this.loadChildren(), 5000);
    }

    async loadChildren() {
        if (!this._instance) return;

        try {
            const resp = await fetch(`/api/instances/${encodeURIComponent(this._instance.title)}/children`);
            if (resp.ok) {
                this._children = await resp.json();
                this.render();
                this.subscribeToStreams();
            }
        } catch (e) {
            console.error('Failed to load children', e);
        }
    }

    subscribeToStreams() {
        // Unsubscribe from old streams
        for (const unsub of this._streamUnsubscribers) {
            unsub();
        }
        this._streamUnsubscribers = [];

        // Subscribe to each child's stream for status updates
        for (const child of this._children) {
            const stream = streamManager.get(child.title);
            const unsub = stream.subscribe((event) => {
                if (event.type === 'status') {
                    this.updateChildStatus(child.title, event.status);
                }
            }, { replay: false });
            this._streamUnsubscribers.push(unsub);
        }
    }

    updateChildStatus(title, status) {
        const card = this.querySelector(`.team-member[data-title="${title}"]`);
        if (!card) return;

        // Update status classes
        card.classList.remove('ready', 'running', 'error', 'creating');
        card.classList.add(status);

        // Update status dot and label
        const dot = card.querySelector('.status-dot');
        const label = card.querySelector('.member-status');
        if (dot) {
            dot.className = `status-dot ${status}`;
        }
        if (label) {
            label.textContent = status;
            label.className = `member-status ${status}`;
        }
    }

    render() {
        if (!this._instance || this._instance.instance_type !== 'loop') {
            this.innerHTML = '';
            return;
        }

        const task = this._instance.task || '';
        const childrenHtml = this._children.map(child => this.renderMemberCard(child)).join('');

        this.innerHTML = `
            <div class="team-panel-header">
                <h3>Team</h3>
                <span class="member-count">${this._children.length} members</span>
            </div>

            <div class="team-section">
                <label class="section-label">Task</label>
                <textarea id="task-input" placeholder="Describe the task for this team...">${this.escapeHtml(task)}</textarea>
                <button id="btn-save-task" class="btn-secondary" type="button">Save Task</button>
            </div>

            <div class="team-section">
                <label class="section-label">Members</label>
                <div id="team-members">
                    ${childrenHtml || '<div class="no-members">No team members yet</div>'}
                </div>
                <button id="btn-add-member" class="btn-secondary" type="button">+ Add Agent</button>
            </div>

            <div class="team-section">
                <label class="section-label">Orchestration</label>
                <div class="orchestration-controls">
                    <button id="btn-start" class="btn-primary" type="button">Start</button>
                    <button id="btn-pause" class="btn-secondary" type="button">Pause</button>
                </div>
            </div>
        `;

        this.setupEventListeners();
    }

    renderMemberCard(child) {
        const stream = streamManager.get(child.title);
        const status = stream?.status || child.status || 'creating';
        const preset = child.agent_preset || 'coder';
        const displayName = child.display_title || child.title;

        return `
            <div class="team-member ${status}" data-title="${this.escapeHtml(child.title)}">
                <div class="member-header">
                    <span class="status-dot ${status}"></span>
                    <span class="member-name">${this.escapeHtml(displayName)}</span>
                    <span class="preset-badge ${preset}">${preset}</span>
                </div>
                <div class="member-meta">
                    <span class="member-status ${status}">${status}</span>
                    <span class="member-path" title="${this.escapeHtml(child.path)}">${this.escapeHtml(this.shortenPath(child.path))}</span>
                </div>
                <button class="btn-remove" data-title="${this.escapeHtml(child.title)}" title="Remove from team">&times;</button>
            </div>
        `;
    }

    setupEventListeners() {
        // Save task
        const saveTaskBtn = this.querySelector('#btn-save-task');
        if (saveTaskBtn) {
            saveTaskBtn.addEventListener('click', () => this.saveTask());
        }

        // Add member
        const addMemberBtn = this.querySelector('#btn-add-member');
        if (addMemberBtn) {
            addMemberBtn.addEventListener('click', () => this.addMember());
        }

        // Remove member buttons
        this.querySelectorAll('.btn-remove').forEach(btn => {
            btn.addEventListener('click', (e) => {
                e.stopPropagation();
                this.removeMember(btn.dataset.title);
            });
        });

        // Member card click - select that instance
        this.querySelectorAll('.team-member').forEach(card => {
            card.addEventListener('click', () => {
                const title = card.dataset.title;
                const child = this._children.find(c => c.title === title);
                if (child) {
                    this.dispatchEvent(new CustomEvent('instance-selected', {
                        bubbles: true,
                        detail: { instance: child }
                    }));
                }
            });
        });

        // Start orchestration
        const startBtn = this.querySelector('#btn-start');
        if (startBtn) {
            startBtn.addEventListener('click', () => this.startOrchestration());
        }

        // Pause orchestration
        const pauseBtn = this.querySelector('#btn-pause');
        if (pauseBtn) {
            pauseBtn.addEventListener('click', () => this.pauseOrchestration());
        }
    }

    async saveTask() {
        if (!this._instance) return;

        const textarea = this.querySelector('#task-input');
        const task = textarea.value.trim();

        try {
            const resp = await fetch(`/api/instances/${encodeURIComponent(this._instance.title)}/task`, {
                method: 'PATCH',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ task })
            });
            if (resp.ok) {
                this._instance.task = task;
                // Show brief feedback
                const btn = this.querySelector('#btn-save-task');
                btn.textContent = 'Saved!';
                setTimeout(() => { btn.textContent = 'Save Task'; }, 1500);
            } else {
                throw new Error(await resp.text());
            }
        } catch (e) {
            alert(`Failed to save task: ${e.message}`);
        }
    }

    async removeMember(title) {
        if (!confirm(`Remove "${title}" from this team?`)) return;

        try {
            const resp = await fetch(`/api/instances/${encodeURIComponent(title)}/reparent`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ parent: null })
            });
            if (resp.ok) {
                await this.loadChildren();
            } else {
                const err = await resp.json();
                throw new Error(err.detail || 'Failed to remove');
            }
        } catch (e) {
            alert(`Failed to remove member: ${e.message}`);
        }
    }

    addMember() {
        // For now, show a simple prompt. In Phase 4, this will open a proper dialog.
        const title = prompt('Enter the title of the agent to add to this team:');
        if (!title) return;

        this.addMemberByTitle(title.trim());
    }

    async addMemberByTitle(title) {
        if (!this._instance) return;

        try {
            const resp = await fetch(`/api/instances/${encodeURIComponent(title)}/reparent`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ parent: this._instance.title })
            });
            if (resp.ok) {
                await this.loadChildren();
            } else {
                const err = await resp.json();
                throw new Error(err.detail || 'Failed to add');
            }
        } catch (e) {
            alert(`Failed to add member: ${e.message}`);
        }
    }

    async startOrchestration() {
        if (!this._instance) return;

        const btn = this.querySelector('#btn-start');
        const originalText = btn.textContent;
        btn.disabled = true;
        btn.textContent = 'Starting...';

        try {
            const resp = await fetch(`/api/instances/${encodeURIComponent(this._instance.title)}/orchestrator/start`, {
                method: 'POST',
            });
            if (!resp.ok) {
                const err = await resp.json().catch(() => ({}));
                throw new Error(err.detail || `HTTP ${resp.status}`);
            }
            const data = await resp.json();
            console.log('Orchestrator started:', data);
            btn.textContent = 'Running';
        } catch (e) {
            alert(`Failed to start orchestrator: ${e.message}`);
            btn.textContent = originalText;
        } finally {
            btn.disabled = false;
        }
    }

    async pauseOrchestration() {
        if (!this._instance) return;

        const btn = this.querySelector('#btn-pause');
        btn.disabled = true;

        try {
            const resp = await fetch(`/api/instances/${encodeURIComponent(this._instance.title)}/orchestrator/stop`, {
                method: 'POST',
            });
            if (!resp.ok) {
                const err = await resp.json().catch(() => ({}));
                throw new Error(err.detail || `HTTP ${resp.status}`);
            }
            console.log('Orchestrator stopped');
            const startBtn = this.querySelector('#btn-start');
            if (startBtn) startBtn.textContent = 'Start';
        } catch (e) {
            alert(`Failed to stop orchestrator: ${e.message}`);
        } finally {
            btn.disabled = false;
        }
    }

    shortenPath(path) {
        if (!path) return '';
        const parts = path.split('/');
        if (parts.length > 3) {
            return '.../' + parts.slice(-2).join('/');
        }
        return path;
    }

    escapeHtml(str) {
        if (!str) return '';
        const div = document.createElement('div');
        div.textContent = str;
        return div.innerHTML;
    }
}

customElements.define('am-team-panel', AmTeamPanel);
