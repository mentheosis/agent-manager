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
        this._dropMode = null;  // 'reorder' | 'reparent'
        this._expandedTeams = new Set();  // Track which teams are expanded
        this._unsubscribers = [];  // Track stream subscriptions for cleanup
    }

    connectedCallback() {
        this.id = 'sidebar';
        this.innerHTML = `
            <div id="sidebar-header">
                <h1><img src="/favicon.svg" alt=""> <span class="sidebar-brand-text">Agent Manager</span></h1>
                <button id="btn-new" type="button">+ New</button>
                <button id="sidebar-collapse" type="button" title="Hide sidebar" aria-label="Hide sidebar">
                    <svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor">
                        <path d="M10.5 3L5.5 8l5 5" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/>
                    </svg>
                </button>
            </div>
            <div id="instance-list"></div>
            <div id="sidebar-mini">
                <button id="sidebar-mini-expand" type="button" title="Show sidebar" aria-label="Show sidebar">
                    <svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor">
                        <path d="M5.5 3L10.5 8l-5 5" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/>
                    </svg>
                </button>
                <div id="sidebar-mini-list"></div>
            </div>
        `;

        this.querySelector('#btn-new').addEventListener('click', () => {
            this.dispatchEvent(new CustomEvent('open-new-dialog', { bubbles: true }));
        });

        this.querySelector('#sidebar-collapse').addEventListener('click', () => {
            this.dispatchEvent(new CustomEvent('close-sidebar', { bubbles: true }));
        });

        this.querySelector('#sidebar-mini-expand').addEventListener('click', () => {
            this.dispatchEvent(new CustomEvent('open-sidebar', { bubbles: true }));
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

        const miniList = this.querySelector('#sidebar-mini-list');
        miniList.innerHTML = '';

        // Build a map of parent -> children for grouping
        const childrenMap = new Map();
        for (const inst of this._instances) {
            if (inst.parent) {
                if (!childrenMap.has(inst.parent)) {
                    childrenMap.set(inst.parent, []);
                }
                childrenMap.get(inst.parent).push(inst);
            }
        }

        // Render instances, grouping children under their parent
        const rendered = new Set();
        for (const inst of this._instances) {
            if (rendered.has(inst.title)) continue;

            // Skip if this is a child (it will be rendered under its parent)
            if (inst.parent) continue;

            const item = this.createInstanceItem(inst, childrenMap.get(inst.title));
            list.appendChild(item);
            rendered.add(inst.title);

            // Render children if this is a loop instance and expanded
            if (inst.instance_type === 'loop') {
                const children = childrenMap.get(inst.title) || [];
                const isExpanded = this._expandedTeams.has(inst.title);

                if (isExpanded) {
                    for (const child of children) {
                        const childItem = this.createInstanceItem(child);
                        list.appendChild(childItem);
                        rendered.add(child.title);
                    }
                }
            }

            const miniItem = this.createMiniItem(inst);
            miniList.appendChild(miniItem);
        }

        // Also add any orphaned children that might have been missed
        for (const inst of this._instances) {
            if (!rendered.has(inst.title)) {
                const item = this.createInstanceItem(inst);
                list.appendChild(item);

                const miniItem = this.createMiniItem(inst);
                miniList.appendChild(miniItem);
            }
        }

        this.updateSelection();
    }

    createInstanceItem(inst, children = null) {
        const item = document.createElement('div');
        item.className = 'instance-item';
        item.dataset.title = inst.title;
        item.dataset.instanceType = inst.instance_type || 'claude';
        item.dataset.parent = inst.parent || '';
        item.dataset.agentPreset = inst.agent_preset || '';
        item.draggable = true;

        // Get stream for status
        const stream = streamManager.get(inst.title);
        const status = stream?.status || inst.status || 'creating';
        item.classList.add(status);

        // Add visual distinction for loop instances
        if (inst.instance_type === 'loop') {
            item.classList.add('loop-instance');
        }
        // Indent children under their parent
        if (inst.parent) {
            item.classList.add('child-instance');
        }

        const presetBadge = inst.agent_preset
            ? `<span class="preset-badge preset-${inst.agent_preset}">${inst.agent_preset}</span>`
            : (inst.instance_type === 'loop' ? '<span class="preset-badge preset-loop">team</span>' : '');

        // Collapse/expand arrow for loop instances
        const isLoop = inst.instance_type === 'loop';
        const childCount = children?.length || 0;
        const isExpanded = this._expandedTeams.has(inst.title);
        const expandArrow = isLoop && childCount > 0
            ? `<button class="team-expand-btn ${isExpanded ? 'expanded' : ''}" type="button" title="${isExpanded ? 'Collapse' : 'Expand'} team">
                 <svg width="12" height="12" viewBox="0 0 12 12"><path d="M4 3L8 6L4 9" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>
               </button>`
            : (isLoop ? '<span class="team-expand-placeholder"></span>' : '');
        const childBadge = isLoop && childCount > 0
            ? `<span class="child-count">${childCount}</span>`
            : '';

        item.innerHTML = `
            <div style="display:flex;align-items:center;gap:6px">
                ${expandArrow}
                <span class="status-dot ${status}"></span>
                <span class="instance-title">${this.escapeHtml(this.displayName(inst))}</span>
                ${presetBadge}
                ${childBadge}
            </div>
            <div class="instance-path" title="${this.escapeHtml(inst.path)}">${this.escapeHtml(inst.path)}</div>
            <div class="instance-meta">
                <span class="status-label ${status}">${status}</span>
                ${inst.permission_mode && inst.permission_mode !== 'acceptEdits' ? `<span>· ${inst.permission_mode}</span>` : ''}
            </div>
        `;

        // Subscribe to stream updates for status changes (no replay needed).
        // Also keeps the mini-strip dot in sync.
        const unsub = stream.subscribe((event) => {
            if (event.type === 'status') {
                item.classList.remove('creating', 'loading', 'ready', 'running', 'paused', 'error', 'deleted');
                item.classList.add(event.status);
                const dot = item.querySelector('.status-dot');
                const label = item.querySelector('.status-label');
                dot.className = `status-dot ${event.status}`;
                label.className = `status-label ${event.status}`;
                label.textContent = event.status;

                // Sync mini-strip dot
                const miniItem = this.querySelector(`.mini-item[data-title="${CSS.escape(inst.title)}"]`);
                if (miniItem) {
                    miniItem.className = `mini-item ${event.status}`;
                    const miniDot = miniItem.querySelector('.status-dot');
                    if (miniDot) miniDot.className = `status-dot ${event.status}`;
                }
            }
        }, { replay: false });
        this._unsubscribers.push(unsub);

        // Click to select
        item.addEventListener('click', (e) => {
            // Don't select if clicking the expand button
            if (e.target.closest('.team-expand-btn')) return;

            this.dispatchEvent(new CustomEvent('instance-selected', {
                bubbles: true,
                detail: { instance: inst }
            }));
        });

        // Expand/collapse handler for loop instances
        const expandBtn = item.querySelector('.team-expand-btn');
        if (expandBtn) {
            expandBtn.addEventListener('click', (e) => {
                e.stopPropagation();
                this.toggleTeamExpanded(inst.title);
            });
        }

        // Drag events
        item.addEventListener('dragstart', (e) => this.onDragStart(e, inst));
        item.addEventListener('dragend', () => this.onDragEnd());

        return item;
    }

    /**
     * Set deleting status for an instance and its children.
     * This shows immediate visual feedback while the delete is in progress.
     */
    setDeleting(title, children = []) {
        const allTitles = [title, ...children];
        for (const t of allTitles) {
            const item = this.querySelector(`.instance-item[data-title="${CSS.escape(t)}"]`);
            if (item) {
                item.classList.remove('creating', 'loading', 'ready', 'running', 'paused', 'error');
                item.classList.add('deleting');
                const dot = item.querySelector('.status-dot');
                const label = item.querySelector('.status-label');
                if (dot) dot.className = 'status-dot deleting';
                if (label) {
                    label.className = 'status-label deleting';
                    label.textContent = 'deleting';
                }
            }
            const miniItem = this.querySelector(`.mini-item[data-title="${CSS.escape(t)}"]`);
            if (miniItem) {
                miniItem.className = 'mini-item deleting';
                const miniDot = miniItem.querySelector('.status-dot');
                if (miniDot) miniDot.className = 'status-dot deleting';
            }
        }
    }

    toggleTeamExpanded(title) {
        if (this._expandedTeams.has(title)) {
            this._expandedTeams.delete(title);
        } else {
            this._expandedTeams.add(title);
        }
        this.render();
    }

    displayName(inst) {
        return inst.display_title || inst.title;
    }

    escapeHtml(str) {
        const div = document.createElement('div');
        div.textContent = str;
        return div.innerHTML;
    }

    createMiniItem(inst) {
        const stream = streamManager.get(inst.title);
        const status = stream?.status || inst.status || 'creating';

        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = `mini-item ${status}`;
        btn.dataset.title = inst.title;
        btn.title = this.displayName(inst);
        btn.setAttribute('aria-label', this.displayName(inst));
        btn.innerHTML = `<span class="status-dot ${status}"></span>`;

        btn.addEventListener('click', () => {
            this.dispatchEvent(new CustomEvent('instance-selected', {
                bubbles: true,
                detail: { instance: inst },
            }));
        });

        return btn;
    }

    updateSelection() {
        const items = this.querySelectorAll('.instance-item');
        for (const item of items) {
            item.classList.toggle('active', item.dataset.title === this._selectedTitle);
        }
        const miniItems = this.querySelectorAll('.mini-item');
        for (const item of miniItems) {
            item.classList.toggle('active', item.dataset.title === this._selectedTitle);
        }
    }

    // Drag-to-reorder handlers
    onDragStart(e, inst) {
        // Prevent dragging loop instances
        if (inst.instance_type === 'loop') {
            e.preventDefault();
            return;
        }

        this._draggedItem = inst;
        this._dropMode = null;
        e.currentTarget.classList.add('dragging');
        e.dataTransfer.effectAllowed = 'move';
        e.dataTransfer.setData('text/plain', inst.title);
    }

    /**
     * Check if dragged item can be dropped into target as reparent.
     */
    canReparentTo(targetInst) {
        if (!this._draggedItem) return false;
        const dragged = this._draggedItem;

        // Can't drop into self
        if (targetInst.title === dragged.title) return false;

        // Target must be a loop instance
        if (targetInst.instance_type !== 'loop') return false;

        // Orchestrator agents can't be moved out of their team
        if (dragged.agent_preset === 'orchestrator' && dragged.parent !== targetInst.title) {
            return false;
        }

        // Already a child of this loop
        if (dragged.parent === targetInst.title) return false;

        return true;
    }

    onDragEnd() {
        this._draggedItem = null;
        this._dropMode = null;
        const items = this.querySelectorAll('.instance-item');
        for (const item of items) {
            item.classList.remove('dragging', 'drop-before', 'drop-after', 'drop-target-group', 'drop-invalid');
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
            item.classList.remove('drop-before', 'drop-after', 'drop-target-group', 'drop-invalid');
        }

        // Find target instance data
        const targetInst = this._instances.find(i => i.title === target.dataset.title);
        if (!targetInst) return;

        // Check if dropping INTO a loop instance (reparent)
        if (targetInst.instance_type === 'loop') {
            const rect = target.getBoundingClientRect();
            // Use center 50% of the item as "drop into" zone
            const zoneTop = rect.top + rect.height * 0.25;
            const zoneBottom = rect.top + rect.height * 0.75;

            if (e.clientY >= zoneTop && e.clientY <= zoneBottom) {
                // Drop INTO the loop
                if (this.canReparentTo(targetInst)) {
                    target.classList.add('drop-target-group');
                    this._dropMode = 'reparent';
                } else {
                    target.classList.add('drop-invalid');
                    this._dropMode = null;
                }
                return;
            }
        }

        // Show reorder hint (top/bottom zones)
        this._dropMode = 'reorder';
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
            target.classList.remove('drop-before', 'drop-after', 'drop-target-group', 'drop-invalid');
        }
    }

    async onDrop(e) {
        if (!this._draggedItem) return;
        e.preventDefault();

        const target = e.target.closest('.instance-item');
        if (!target || target.dataset.title === this._draggedItem.title) return;

        const dropMode = this._dropMode;
        const draggedTitle = this._draggedItem.title;
        const draggedHadParent = this._draggedItem.parent;

        // Clear hints first
        this.onDragEnd();

        if (dropMode === 'reparent') {
            // Reparent into the loop instance
            const targetTitle = target.dataset.title;
            try {
                await api.reparentInstance(draggedTitle, targetTitle);
                this.dispatchEvent(new CustomEvent('instances-reordered', { bubbles: true }));
            } catch (err) {
                console.error('Failed to reparent', err);
            }
            return;
        }

        // Standard reorder - but also check if we need to remove from parent
        // If the agent had a parent and is being dropped outside its parent,
        // we should remove it from the team (unless it's an orchestrator)
        if (draggedHadParent && this._draggedItem?.agent_preset !== 'orchestrator') {
            const targetInst = this._instances.find(i => i.title === target.dataset.title);
            // If dropping outside the parent's children area, remove from team
            if (targetInst && targetInst.parent !== draggedHadParent && targetInst.title !== draggedHadParent) {
                try {
                    await api.reparentInstance(draggedTitle, null);
                    this.dispatchEvent(new CustomEvent('instances-reordered', { bubbles: true }));
                } catch (err) {
                    console.error('Failed to remove from team', err);
                }
                return;
            }
        }

        const titles = this._instances.map(i => i.title);
        const fromIndex = titles.indexOf(draggedTitle);
        const toIndex = titles.indexOf(target.dataset.title);

        if (fromIndex === -1 || toIndex === -1) return;

        // Remove from old position
        titles.splice(fromIndex, 1);

        // Insert at new position
        const rect = target.getBoundingClientRect();
        const midY = rect.top + rect.height / 2;
        const insertIndex = e.clientY < midY ? toIndex : toIndex + 1;
        const adjustedIndex = insertIndex > fromIndex ? insertIndex - 1 : insertIndex;
        titles.splice(adjustedIndex, 0, draggedTitle);

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
