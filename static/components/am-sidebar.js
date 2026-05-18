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
        this._dropMode = null;  // 'reorder' | 'reparent' | 'folder'
        this._expandedTeams = new Set();  // Track which teams are expanded
        this._expandedFolders = new Set();  // Track which folders are expanded
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
            <div id="folder-context-menu" class="context-menu" hidden>
                <div class="context-menu-header">Move to folder</div>
                <button type="button" class="context-menu-item" data-action="new-folder">+ New Folder</button>
                <button type="button" class="context-menu-item" data-action="no-folder">Remove from folder</button>
                <div class="context-menu-divider"></div>
                <div class="context-menu-folders"></div>
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

        // Context menu for folder management
        list.addEventListener('contextmenu', (e) => this.onContextMenu(e));
        document.addEventListener('click', () => this.hideContextMenu());
        document.addEventListener('contextmenu', (e) => {
            if (!e.target.closest('#instance-list')) {
                this.hideContextMenu();
            }
        });

        // Context menu actions
        const menu = this.querySelector('#folder-context-menu');
        menu.addEventListener('click', (e) => this.onContextMenuAction(e));
    }

    onContextMenu(e) {
        const item = e.target.closest('.instance-item');
        if (!item) return;

        const inst = this._instances.find(i => i.title === item.dataset.title);
        if (!inst || inst.instance_type === 'loop') return;  // Can't folder teams

        e.preventDefault();
        this._contextTarget = inst;

        const menu = this.querySelector('#folder-context-menu');
        const foldersDiv = menu.querySelector('.context-menu-folders');

        // Build folder list
        const folders = new Set();
        for (const i of this._instances) {
            if (i.folder) folders.add(i.folder);
        }

        foldersDiv.innerHTML = '';
        for (const folder of [...folders].sort()) {
            const btn = document.createElement('button');
            btn.type = 'button';
            btn.className = 'context-menu-item';
            btn.dataset.action = 'move-to-folder';
            btn.dataset.folder = folder;
            btn.textContent = `📁 ${folder}`;
            if (inst.folder === folder) {
                btn.classList.add('active');
            }
            foldersDiv.appendChild(btn);
        }

        // Show/hide "Remove from folder" based on whether instance has folder
        const removeBtn = menu.querySelector('[data-action="no-folder"]');
        removeBtn.hidden = !inst.folder;

        // Position and show menu
        menu.style.left = `${e.clientX}px`;
        menu.style.top = `${e.clientY}px`;
        menu.hidden = false;

        // Ensure menu stays in viewport
        requestAnimationFrame(() => {
            const rect = menu.getBoundingClientRect();
            if (rect.right > window.innerWidth) {
                menu.style.left = `${window.innerWidth - rect.width - 8}px`;
            }
            if (rect.bottom > window.innerHeight) {
                menu.style.top = `${window.innerHeight - rect.height - 8}px`;
            }
        });
    }

    hideContextMenu() {
        const menu = this.querySelector('#folder-context-menu');
        if (menu) menu.hidden = true;
        this._contextTarget = null;
    }

    async onContextMenuAction(e) {
        const btn = e.target.closest('.context-menu-item');
        if (!btn || !this._contextTarget) return;

        const action = btn.dataset.action;
        const inst = this._contextTarget;

        this.hideContextMenu();

        if (action === 'new-folder') {
            const name = prompt('Enter folder name:');
            if (!name || !name.trim()) return;
            try {
                await api.updateFolder(inst.title, name.trim());
                this._expandedFolders.add(name.trim());
                this.dispatchEvent(new CustomEvent('instances-reordered', { bubbles: true }));
            } catch (err) {
                console.error('Failed to create folder', err);
            }
        } else if (action === 'no-folder') {
            try {
                await api.updateFolder(inst.title, null);
                this.dispatchEvent(new CustomEvent('instances-reordered', { bubbles: true }));
            } catch (err) {
                console.error('Failed to remove from folder', err);
            }
        } else if (action === 'move-to-folder') {
            const folder = btn.dataset.folder;
            try {
                await api.updateFolder(inst.title, folder);
                this._expandedFolders.add(folder);
                this.dispatchEvent(new CustomEvent('instances-reordered', { bubbles: true }));
            } catch (err) {
                console.error('Failed to move to folder', err);
            }
        }
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

        // Build a map of parent -> children for team grouping
        const childrenMap = new Map();
        for (const inst of this._instances) {
            if (inst.parent) {
                if (!childrenMap.has(inst.parent)) {
                    childrenMap.set(inst.parent, []);
                }
                childrenMap.get(inst.parent).push(inst);
            }
        }

        // Build folder info (instances per folder) without changing order
        const folderInstances = new Map();  // folder name -> instances
        for (const inst of this._instances) {
            if (inst.parent) continue;  // Children are rendered under their team
            if (inst.folder) {
                if (!folderInstances.has(inst.folder)) {
                    folderInstances.set(inst.folder, []);
                }
                folderInstances.get(inst.folder).push(inst);
            }
        }

        const rendered = new Set();
        const renderedFolders = new Set();

        // Render in order - when we hit a foldered instance, render its folder first
        for (const inst of this._instances) {
            if (inst.parent) continue;  // Children are rendered under their team
            if (rendered.has(inst.title)) continue;

            if (inst.folder) {
                // Render folder header if not already rendered
                if (!renderedFolders.has(inst.folder)) {
                    const instances = folderInstances.get(inst.folder) || [];
                    const folderEl = this.createFolderItem(inst.folder, instances);
                    list.appendChild(folderEl);
                    renderedFolders.add(inst.folder);

                    // Render folder contents if expanded
                    const isExpanded = this._expandedFolders.has(inst.folder);
                    if (isExpanded) {
                        for (const folderInst of instances) {
                            this.renderInstanceWithChildren(list, folderInst, childrenMap, rendered);
                        }
                        // Add drop zone at end of folder (for adding to folder)
                        const dropZone = this.createFolderDropZone(inst.folder);
                        list.appendChild(dropZone);
                        // Add exit zone after folder (for removing from folder)
                        const exitZone = this.createFolderExitZone(inst.folder);
                        list.appendChild(exitZone);
                    } else {
                        // Mark all folder instances as rendered even if collapsed
                        for (const folderInst of instances) {
                            rendered.add(folderInst.title);
                        }
                    }
                }
            } else {
                // Unfoldered instance - render directly
                this.renderInstanceWithChildren(list, inst, childrenMap, rendered);
            }
        }

        // Render mini list (all top-level, no folders)
        for (const inst of this._instances) {
            if (inst.parent) continue;
            const miniItem = this.createMiniItem(inst);
            miniList.appendChild(miniItem);
        }

        // Also add any orphaned children that might have been missed
        for (const inst of this._instances) {
            if (!rendered.has(inst.title)) {
                const item = this.createInstanceItem(inst);
                list.appendChild(item);
            }
        }

        this.updateSelection();
    }

    renderInstanceWithChildren(list, inst, childrenMap, rendered) {
        if (rendered.has(inst.title)) return;

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
    }

    createFolderItem(name, instances) {
        const item = document.createElement('div');
        item.className = 'folder-item';
        item.dataset.folder = name;
        item.draggable = true;

        const isExpanded = this._expandedFolders.has(name);
        const count = instances.length;

        item.innerHTML = `
            <button class="folder-toggle ${isExpanded ? 'expanded' : ''}" type="button" title="${isExpanded ? 'Collapse' : 'Expand'} folder">
                <svg width="12" height="12" viewBox="0 0 12 12"><path d="M4 3L8 6L4 9" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>
            </button>
            <span class="folder-icon">📁</span>
            <span class="folder-name">${this.escapeHtml(name)}</span>
            <span class="folder-count">${count}</span>
        `;

        item.querySelector('.folder-toggle').addEventListener('click', (e) => {
            e.stopPropagation();
            this.toggleFolderExpanded(name);
        });

        // Click folder name to expand/collapse
        item.addEventListener('click', () => {
            this.toggleFolderExpanded(name);
        });

        // Drag to reorder folder
        item.addEventListener('dragstart', (e) => {
            this._draggedFolder = name;
            this._draggedItem = null;
            item.classList.add('dragging');
            e.dataTransfer.effectAllowed = 'move';
            e.dataTransfer.setData('text/plain', `folder:${name}`);
        });

        item.addEventListener('dragend', () => {
            this._draggedFolder = null;
            item.classList.remove('dragging');
            this.onDragEnd();
        });

        // Make folder a drop target with zones: top edge (before), center (into), bottom edge (after)
        item.addEventListener('dragover', (e) => {
            if (!this._draggedItem) return;
            e.preventDefault();
            e.stopPropagation();

            // Clear previous indicators
            item.classList.remove('drop-target-folder', 'drop-before', 'drop-after', 'drop-invalid');

            const rect = item.getBoundingClientRect();
            const zoneHeight = rect.height * 0.3;
            const y = e.clientY - rect.top;

            if (y < zoneHeight) {
                // Top edge - reorder before folder
                item.classList.add('drop-before');
                this._dropMode = 'reorder-before-folder';
            } else if (y > rect.height - zoneHeight) {
                // Bottom edge - reorder after folder
                item.classList.add('drop-after');
                this._dropMode = 'reorder-after-folder';
            } else {
                // Center - drop into folder
                if (this._draggedItem.instance_type === 'loop') {
                    item.classList.add('drop-invalid');
                    this._dropMode = null;
                } else {
                    item.classList.add('drop-target-folder');
                    this._dropMode = 'folder';
                }
            }
        });

        item.addEventListener('dragleave', () => {
            item.classList.remove('drop-target-folder', 'drop-before', 'drop-after', 'drop-invalid');
        });

        item.addEventListener('drop', async (e) => {
            e.preventDefault();
            e.stopPropagation();

            const dropMode = this._dropMode;
            const draggedItem = this._draggedItem;
            const draggedTitle = draggedItem?.title;
            const draggedFolder = draggedItem?.folder;
            const draggedType = draggedItem?.instance_type;

            // Clear drag state early
            item.classList.remove('drop-target-folder', 'drop-before', 'drop-after', 'drop-invalid');
            this.onDragEnd();

            if (!draggedItem) return;

            // Handle reorder before/after folder
            if (dropMode === 'reorder-before-folder' || dropMode === 'reorder-after-folder') {
                // Find the first/last instance in this folder to determine position
                const folderInstances = this._instances.filter(i => i.folder === name && !i.parent);
                if (folderInstances.length === 0) return;

                const anchorTitle = dropMode === 'reorder-before-folder'
                    ? folderInstances[0].title
                    : folderInstances[folderInstances.length - 1].title;

                // Remove from any folder - dropping before/after a folder means outside of it
                if (draggedFolder) {
                    try {
                        await api.updateFolder(draggedTitle, null);
                    } catch (err) {
                        console.error('Failed to remove from folder', err);
                        return;
                    }
                }

                // Reorder
                const titles = this._instances.map(i => i.title);
                const fromIndex = titles.indexOf(draggedTitle);
                let toIndex = titles.indexOf(anchorTitle);

                if (fromIndex === -1 || toIndex === -1) return;

                titles.splice(fromIndex, 1);
                toIndex = titles.indexOf(anchorTitle);
                const insertIndex = dropMode === 'reorder-before-folder' ? toIndex : toIndex + 1;
                titles.splice(insertIndex, 0, draggedTitle);

                try {
                    await api.reorderInstances(titles);
                    this.dispatchEvent(new CustomEvent('instances-reordered', { bubbles: true }));
                } catch (err) {
                    console.error('Failed to reorder', err);
                }
                return;
            }

            // Handle drop into folder
            if (draggedType === 'loop') return;

            try {
                await api.updateFolder(draggedTitle, name);
                this.dispatchEvent(new CustomEvent('instances-reordered', { bubbles: true }));
            } catch (err) {
                console.error('Failed to move to folder', err);
            }
        });

        return item;
    }

    createFolderDropZone(folderName) {
        const zone = document.createElement('div');
        zone.className = 'folder-drop-zone';
        zone.dataset.folder = folderName;

        zone.addEventListener('dragover', (e) => {
            if (!this._draggedItem) return;
            // Can't drop teams into folders
            if (this._draggedItem.instance_type === 'loop') {
                zone.classList.add('drop-invalid');
                return;
            }
            e.preventDefault();
            e.stopPropagation();
            zone.classList.add('drop-active');
            this._dropMode = 'folder-end';
        });

        zone.addEventListener('dragleave', () => {
            zone.classList.remove('drop-active', 'drop-invalid');
        });

        zone.addEventListener('drop', async (e) => {
            e.preventDefault();
            e.stopPropagation();
            zone.classList.remove('drop-active', 'drop-invalid');

            if (!this._draggedItem || this._draggedItem.instance_type === 'loop') return;

            const draggedTitle = this._draggedItem.title;
            const draggedHadFolder = this._draggedItem.folder;
            this.onDragEnd();

            // Update folder if needed
            if (draggedHadFolder !== folderName) {
                try {
                    await api.updateFolder(draggedTitle, folderName);
                    this._expandedFolders.add(folderName);
                    this.dispatchEvent(new CustomEvent('instances-reordered', { bubbles: true }));
                } catch (err) {
                    console.error('Failed to move to folder', err);
                }
                return;
            }

            // Already in this folder - reorder to end of folder
            const folderInstances = this._instances.filter(i => i.folder === folderName && !i.parent);
            if (folderInstances.length === 0) return;

            const lastInFolder = folderInstances[folderInstances.length - 1];
            if (lastInFolder.title === draggedTitle) return; // Already at end

            // Reorder: move dragged item to after the last item in folder
            const titles = this._instances.map(i => i.title);
            const fromIndex = titles.indexOf(draggedTitle);
            const toIndex = titles.indexOf(lastInFolder.title);

            if (fromIndex === -1 || toIndex === -1) return;

            titles.splice(fromIndex, 1);
            const newToIndex = titles.indexOf(lastInFolder.title);
            titles.splice(newToIndex + 1, 0, draggedTitle);

            try {
                await api.reorderInstances(titles);
                this.dispatchEvent(new CustomEvent('instances-reordered', { bubbles: true }));
            } catch (err) {
                console.error('Failed to reorder', err);
            }
        });

        return zone;
    }

    createFolderExitZone(folderName) {
        const zone = document.createElement('div');
        zone.className = 'folder-exit-zone';
        zone.dataset.folder = folderName;

        zone.addEventListener('dragover', (e) => {
            if (!this._draggedItem) return;
            // Only show as active if dragging FROM this folder
            if (this._draggedItem.folder !== folderName) return;
            // Can't remove teams from folders (they can't be in folders anyway)
            if (this._draggedItem.instance_type === 'loop') return;

            e.preventDefault();
            e.stopPropagation();
            zone.classList.add('drop-active');
            this._dropMode = 'folder-exit';
        });

        zone.addEventListener('dragleave', () => {
            zone.classList.remove('drop-active');
        });

        zone.addEventListener('drop', async (e) => {
            e.preventDefault();
            e.stopPropagation();
            zone.classList.remove('drop-active');

            if (!this._draggedItem) return;
            if (this._draggedItem.folder !== folderName) return;
            if (this._draggedItem.instance_type === 'loop') return;

            const draggedTitle = this._draggedItem.title;
            this.onDragEnd();

            // Remove from folder
            try {
                await api.updateFolder(draggedTitle, null);
                this.dispatchEvent(new CustomEvent('instances-reordered', { bubbles: true }));
            } catch (err) {
                console.error('Failed to remove from folder', err);
            }
        });

        return zone;
    }

    toggleFolderExpanded(name) {
        if (this._expandedFolders.has(name)) {
            this._expandedFolders.delete(name);
        } else {
            this._expandedFolders.add(name);
        }
        this.render();
    }

    createInstanceItem(inst, children = null) {
        const item = document.createElement('div');
        item.className = 'instance-item';
        item.dataset.title = inst.title;
        item.dataset.instanceType = inst.instance_type || 'claude';
        item.dataset.parent = inst.parent || '';
        item.dataset.agentPreset = inst.agent_preset || '';
        item.dataset.folder = inst.folder || '';
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

        // Show exit zone for the folder this item is in
        if (inst.folder) {
            const exitZone = this.querySelector(`.folder-exit-zone[data-folder="${CSS.escape(inst.folder)}"]`);
            if (exitZone) {
                exitZone.classList.add('visible');
            }
        }
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
        this._draggedFolder = null;
        this._dropMode = null;
        const items = this.querySelectorAll('.instance-item, .folder-item');
        for (const item of items) {
            item.classList.remove('dragging', 'drop-before', 'drop-after', 'drop-target-group', 'drop-invalid');
        }
        const dropZones = this.querySelectorAll('.folder-drop-zone, .folder-exit-zone');
        for (const zone of dropZones) {
            zone.classList.remove('drop-active', 'drop-invalid', 'visible');
        }
    }

    onDragOver(e) {
        if (!this._draggedItem && !this._draggedFolder) return;
        e.preventDefault();
        e.dataTransfer.dropEffect = 'move';

        // Clear previous hints
        const items = this.querySelectorAll('.instance-item, .folder-item');
        for (const item of items) {
            item.classList.remove('drop-before', 'drop-after', 'drop-target-group', 'drop-invalid');
        }

        // Handle folder drag
        if (this._draggedFolder) {
            const target = e.target.closest('.instance-item, .folder-item');
            if (!target) return;
            // Can't drop on itself
            if (target.classList.contains('folder-item') && target.dataset.folder === this._draggedFolder) return;

            this._dropMode = 'reorder-folder';
            const rect = target.getBoundingClientRect();
            const midY = rect.top + rect.height / 2;
            if (e.clientY < midY) {
                target.classList.add('drop-before');
            } else {
                target.classList.add('drop-after');
            }
            return;
        }

        // Handle instance drag
        const target = e.target.closest('.instance-item');
        if (!target || target.dataset.title === this._draggedItem.title) return;

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
        // Handle folder reorder
        if (this._draggedFolder) {
            e.preventDefault();
            const target = e.target.closest('.instance-item, .folder-item');
            if (!target) {
                this.onDragEnd();
                return;
            }

            const folderName = this._draggedFolder;
            this.onDragEnd();

            // Get all instances in this folder (in order)
            const folderInstances = this._instances.filter(i => i.folder === folderName && !i.parent);
            if (folderInstances.length === 0) return;

            // Find drop position
            let targetTitle, insertBefore;
            if (target.classList.contains('folder-item')) {
                // Dropping on another folder - find first instance in that folder
                const targetFolderName = target.dataset.folder;
                const targetFolderInst = this._instances.find(i => i.folder === targetFolderName && !i.parent);
                if (targetFolderInst) {
                    targetTitle = targetFolderInst.title;
                    insertBefore = true; // Always insert before the target folder's instances
                } else {
                    return;
                }
            } else {
                // Dropping on an instance
                targetTitle = target.dataset.title;
                const rect = target.getBoundingClientRect();
                insertBefore = e.clientY < rect.top + rect.height / 2;
            }

            // Build new title order
            const folderTitles = new Set(folderInstances.map(i => i.title));
            const titles = this._instances.map(i => i.title).filter(t => !folderTitles.has(t));
            const targetIndex = titles.indexOf(targetTitle);
            if (targetIndex === -1) return;

            const insertIndex = insertBefore ? targetIndex : targetIndex + 1;
            titles.splice(insertIndex, 0, ...folderInstances.map(i => i.title));

            try {
                await api.reorderInstances(titles);
                this.dispatchEvent(new CustomEvent('instances-reordered', { bubbles: true }));
            } catch (err) {
                console.error('Failed to reorder folder', err);
            }
            return;
        }

        if (!this._draggedItem) return;
        e.preventDefault();

        const target = e.target.closest('.instance-item');
        if (!target || target.dataset.title === this._draggedItem.title) return;

        const dropMode = this._dropMode;
        const draggedTitle = this._draggedItem.title;
        const draggedHadParent = this._draggedItem.parent;
        const draggedHadFolder = this._draggedItem.folder;
        const draggedInstanceType = this._draggedItem.instance_type;
        const draggedPreset = this._draggedItem.agent_preset;

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

        // Check if we need to change folder
        const targetInst = this._instances.find(i => i.title === target.dataset.title);
        const targetFolder = targetInst?.folder || null;

        // If dragging to a different folder (or out of a folder), update folder
        if (draggedHadFolder !== targetFolder && draggedInstanceType !== 'loop') {
            try {
                await api.updateFolder(draggedTitle, targetFolder);
                this.dispatchEvent(new CustomEvent('instances-reordered', { bubbles: true }));
            } catch (err) {
                console.error('Failed to update folder', err);
            }
            return;
        }

        // Standard reorder - but also check if we need to remove from parent
        // If the agent had a parent and is being dropped outside its parent,
        // we should remove it from the team (unless it's an orchestrator)
        if (draggedHadParent && draggedPreset !== 'orchestrator') {
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
