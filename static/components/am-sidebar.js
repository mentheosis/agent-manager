/**
 * Sidebar component - instance list with drag-to-reorder.
 */

import * as api from '../lib/api.js';
import { streamManager } from '../lib/streams.js';

class AmSidebar extends HTMLElement {
    constructor() {
        super();
        this._instances = [];
        this._selectedTitle = null;
        this._draggedItem = null;
        this._unsubscribers = [];  // Track stream subscriptions for cleanup
    }

    connectedCallback() {
        this.id = 'sidebar';
        this.innerHTML = `
            <div id="sidebar-header">
                <h1><img src="/favicon.svg" alt=""> Agent Manager</h1>
                <button id="btn-new" type="button">+ New</button>
                <button id="sidebar-collapse" type="button" title="Hide sidebar" aria-label="Hide sidebar">
                    <svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor">
                        <path d="M10.5 3L5.5 8l5 5" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/>
                    </svg>
                </button>
            </div>
            <div id="instance-list"></div>
        `;

        this.querySelector('#btn-new').addEventListener('click', () => {
            this.dispatchEvent(new CustomEvent('open-new-dialog', { bubbles: true }));
        });

        this.querySelector('#sidebar-collapse').addEventListener('click', () => {
            this.dispatchEvent(new CustomEvent('close-sidebar', { bubbles: true }));
        });

        // Drag-to-reorder listeners on the list container
        const list = this.querySelector('#instance-list');
        list.addEventListener('dragover', (e) => this.onDragOver(e));
        list.addEventListener('dragleave', (e) => this.onDragLeave(e));
        list.addEventListener('drop', (e) => this.onDrop(e));
    }

    disconnectedCallback() {
        // Clean up all subscriptions
        for (const unsub of this._unsubscribers) {
            unsub();
        }
        this._unsubscribers = [];
    }

    get instances() {
        return this._instances;
    }

    set instances(value) {
        this._instances = value || [];
        this.render();
    }

    get selectedTitle() {
        return this._selectedTitle;
    }

    set selectedTitle(value) {
        this._selectedTitle = value;
        this.updateSelection();
    }

    render() {
        // Clean up old subscriptions
        for (const unsub of this._unsubscribers) {
            unsub();
        }
        this._unsubscribers = [];

        const list = this.querySelector('#instance-list');
        list.innerHTML = '';

        for (const inst of this._instances) {
            const item = this.createInstanceItem(inst);
            list.appendChild(item);
        }

        this.updateSelection();
    }

    createInstanceItem(inst) {
        const item = document.createElement('div');
        item.className = 'instance-item';
        item.dataset.title = inst.title;
        item.draggable = true;

        // Get stream for status
        const stream = streamManager.get(inst.title);
        const status = stream?.status || inst.status || 'creating';
        item.classList.add(status);

        item.innerHTML = `
            <div style="display:flex;align-items:center;gap:6px">
                <span class="status-dot ${status}"></span>
                <span class="instance-title">${this.escapeHtml(this.displayName(inst))}</span>
                <button class="instance-delete" type="button" title="Delete">×</button>
            </div>
            <div class="instance-path" title="${this.escapeHtml(inst.path)}">${this.escapeHtml(inst.path)}</div>
            <div class="instance-meta">
                <span class="status-label ${status}">${status}</span>
                ${inst.permission_mode && inst.permission_mode !== 'acceptEdits' ? `<span>· ${inst.permission_mode}</span>` : ''}
            </div>
        `;

        // Subscribe to stream updates for status changes (no replay needed)
        const unsub = stream.subscribe((event) => {
            if (event.type === 'status') {
                item.classList.remove('creating', 'loading', 'ready', 'running', 'paused', 'error', 'deleted');
                item.classList.add(event.status);
                const dot = item.querySelector('.status-dot');
                const label = item.querySelector('.status-label');
                dot.className = `status-dot ${event.status}`;
                label.className = `status-label ${event.status}`;
                label.textContent = event.status;
            }
        }, { replay: false });
        this._unsubscribers.push(unsub);

        // Click to select
        item.addEventListener('click', (e) => {
            if (e.target.classList.contains('instance-delete')) return;
            this.dispatchEvent(new CustomEvent('instance-selected', {
                bubbles: true,
                detail: { instance: inst }
            }));
        });

        // Delete button
        item.querySelector('.instance-delete').addEventListener('click', async (e) => {
            e.stopPropagation();
            if (!confirm(`Delete "${this.displayName(inst)}"?`)) return;
            try {
                await api.deleteInstance(inst.title);
                this.dispatchEvent(new CustomEvent('instance-deleted', {
                    bubbles: true,
                    detail: { title: inst.title }
                }));
            } catch (err) {
                alert(`Failed to delete: ${err.message}`);
            }
        });

        // Drag events
        item.addEventListener('dragstart', (e) => this.onDragStart(e, inst));
        item.addEventListener('dragend', () => this.onDragEnd());

        return item;
    }

    displayName(inst) {
        return inst.display_title || inst.title;
    }

    escapeHtml(str) {
        const div = document.createElement('div');
        div.textContent = str;
        return div.innerHTML;
    }

    updateSelection() {
        const items = this.querySelectorAll('.instance-item');
        for (const item of items) {
            item.classList.toggle('active', item.dataset.title === this._selectedTitle);
        }
    }

    // Drag-to-reorder handlers
    onDragStart(e, inst) {
        this._draggedItem = inst;
        e.currentTarget.classList.add('dragging');
        e.dataTransfer.effectAllowed = 'move';
        e.dataTransfer.setData('text/plain', inst.title);
    }

    onDragEnd() {
        this._draggedItem = null;
        const items = this.querySelectorAll('.instance-item');
        for (const item of items) {
            item.classList.remove('dragging', 'drop-before', 'drop-after');
        }
    }

    onDragOver(e) {
        if (!this._draggedItem) return;
        e.preventDefault();
        e.dataTransfer.dropEffect = 'move';

        const target = e.target.closest('.instance-item');
        if (!target || target.dataset.title === this._draggedItem.title) return;

        // Clear previous hints
        const items = this.querySelectorAll('.instance-item');
        for (const item of items) {
            item.classList.remove('drop-before', 'drop-after');
        }

        // Show drop hint
        const rect = target.getBoundingClientRect();
        const midY = rect.top + rect.height / 2;
        if (e.clientY < midY) {
            target.classList.add('drop-before');
        } else {
            target.classList.add('drop-after');
        }
    }

    onDragLeave(e) {
        const target = e.target.closest('.instance-item');
        if (target) {
            target.classList.remove('drop-before', 'drop-after');
        }
    }

    async onDrop(e) {
        if (!this._draggedItem) return;
        e.preventDefault();

        const target = e.target.closest('.instance-item');
        if (!target || target.dataset.title === this._draggedItem.title) return;

        const titles = this._instances.map(i => i.title);
        const fromIndex = titles.indexOf(this._draggedItem.title);
        const toIndex = titles.indexOf(target.dataset.title);

        if (fromIndex === -1 || toIndex === -1) return;

        // Remove from old position
        titles.splice(fromIndex, 1);

        // Insert at new position
        const rect = target.getBoundingClientRect();
        const midY = rect.top + rect.height / 2;
        const insertIndex = e.clientY < midY ? toIndex : toIndex + 1;
        const adjustedIndex = insertIndex > fromIndex ? insertIndex - 1 : insertIndex;
        titles.splice(adjustedIndex, 0, this._draggedItem.title);

        // Clear hints
        this.onDragEnd();

        // Save new order
        try {
            await api.reorderInstances(titles);
            this.dispatchEvent(new CustomEvent('instances-reordered', { bubbles: true }));
        } catch (err) {
            console.error('Failed to reorder', err);
        }
    }
}

customElements.define('am-sidebar', AmSidebar);
