/**
 * File editor component - reusable editor for settings/plans/memory tabs.
 * Supports an optional SDK settings panel as the first tab.
 */

import * as api from '../lib/api.js';
import { streamManager } from '../lib/streams.js';
import './am-permissions-panel.js';

const PERMISSIONS_TAB_INDEX = -2;

class AmFileEditor extends HTMLElement {
    constructor() {
        super();
        this._title = null;
        this.files = [];
        this.activeIndex = -1;
        this.dirty = false;
        this.savedContent = '';
    }

    connectedCallback() {
        this.classList.add('file-editor-pane');
        this.endpoint = this.dataset.endpoint;
        this.hasPermissionsTab = this.dataset.hasPermissions === 'true';

        this.innerHTML = `
            <div class="file-tabs"></div>
            <div class="file-toolbar">
                <span class="file-path"></span>
                <span class="file-status"></span>
                <button type="button" class="file-refresh-btn">Refresh</button>
                <button type="button" class="file-save-btn" disabled>Save</button>
            </div>
            <textarea class="file-editor" spellcheck="false" placeholder="${this.getPlaceholder()}"></textarea>
            ${this.hasPermissionsTab ? '<am-permissions-panel hidden></am-permissions-panel>' : ''}
        `;

        this.setupEventListeners();
    }

    getPlaceholder() {
        switch (this.endpoint) {
            case 'rules':
                return 'Select a file above...';
            case 'plans':
                return 'No plan files yet - agent writes them to .claude/plans/ when you run /plan.';
            case 'memory':
                return 'Memory files appear here as Claude writes them.';
            default:
                return '';
        }
    }

    setupEventListeners() {
        this.querySelector('.file-refresh-btn').addEventListener('click', () => {
            this.load(this._title, { force: true });
        });

        this.querySelector('.file-save-btn').addEventListener('click', () => {
            this.save();
        });

        const editor = this.querySelector('.file-editor');
        editor.addEventListener('input', () => this.onEdit());
        editor.addEventListener('keydown', (e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === 's') {
                e.preventDefault();
                this.save();
            }
        });
    }

    async load(title, { force = false } = {}) {
        if (!title) return;
        if (!force && title === this._title && this.files.length) return;
        this._title = title;

        const editor = this.querySelector('.file-editor');
        editor.disabled = true;
        editor.value = 'loading...';

        try {
            const data = await api.fetchFiles(title, this.endpoint);
            this.files = data.files || [];
            this.renderTabs();
            editor.disabled = false;

            if (this.activeIndex === PERMISSIONS_TAB_INDEX) {
                // Stay on permissions tab
                return;
            }

            // Default to SDK Settings tab if this pane has it
            if (this.hasPermissionsTab && this.activeIndex < 0) {
                await this.selectPermissions();
                return;
            }

            if (this.files.length) {
                this.selectFile(this.activeIndex >= 0 && this.activeIndex < this.files.length ? this.activeIndex : 0);
            } else {
                editor.value = '';
                this.querySelector('.file-path').textContent = data.directory ? `(empty) ${data.directory}` : '(no files)';
                this.querySelector('.file-status').textContent = '';
                this.querySelector('.file-save-btn').disabled = true;
            }
        } catch (e) {
            editor.value = `error: ${e.message}`;
        }
    }

    renderTabs() {
        const tabsEl = this.querySelector('.file-tabs');
        tabsEl.innerHTML = '';

        // SDK Settings tab first if enabled
        if (this.hasPermissionsTab) {
            const btn = document.createElement('button');
            btn.type = 'button';
            btn.className = 'file-tab perm-tab';
            btn.classList.toggle('active', this.activeIndex === PERMISSIONS_TAB_INDEX);
            btn.textContent = 'SDK Settings';
            btn.addEventListener('click', () => this.selectPermissions());
            tabsEl.appendChild(btn);
        }

        // File tabs
        for (let i = 0; i < this.files.length; i++) {
            const f = this.files[i];
            const btn = document.createElement('button');
            btn.type = 'button';
            btn.className = 'file-tab';
            btn.classList.toggle('active', i === this.activeIndex);
            if (f.exists === false) btn.classList.add('missing');
            btn.textContent = f.name;
            btn.addEventListener('click', () => this.selectFile(i));
            tabsEl.appendChild(btn);
        }
    }

    showFileView() {
        this.querySelector('.file-toolbar').hidden = false;
        this.querySelector('.file-editor').hidden = false;
        const permPanel = this.querySelector('am-permissions-panel');
        if (permPanel) permPanel.hidden = true;
    }

    showPermissionsView() {
        this.querySelector('.file-toolbar').hidden = true;
        this.querySelector('.file-editor').hidden = true;
        const permPanel = this.querySelector('am-permissions-panel');
        if (permPanel) permPanel.hidden = false;
    }

    selectFile(index) {
        if (this.dirty && !confirm('Discard unsaved changes?')) return;

        this.activeIndex = index;
        this.showFileView();

        const f = this.files[index];
        const editor = this.querySelector('.file-editor');
        const pathEl = this.querySelector('.file-path');
        const statusEl = this.querySelector('.file-status');
        const saveBtn = this.querySelector('.file-save-btn');

        editor.value = f.content || '';
        this.savedContent = editor.value;
        pathEl.textContent = f.path;
        statusEl.className = 'file-status';
        statusEl.textContent = f.exists === false ? '(file does not exist yet)' : 'saved';
        statusEl.classList.add('saved');
        saveBtn.disabled = true;
        saveBtn.classList.remove('dirty');
        this.dirty = false;

        this.renderTabs();
    }

    async selectPermissions() {
        if (this.dirty && !confirm('Discard unsaved changes?')) return;

        this.activeIndex = PERMISSIONS_TAB_INDEX;
        this.showPermissionsView();
        this.renderTabs();

        const permPanel = this.querySelector('am-permissions-panel');
        if (permPanel) {
            await permPanel.load(this._title);

            // Pass active model from stream
            const stream = streamManager.get(this._title);
            if (stream?.activeModel) {
                permPanel.setActiveModel(stream.activeModel);
            }
        }
    }

    onEdit() {
        const editor = this.querySelector('.file-editor');
        const isDirty = editor.value !== this.savedContent;
        if (isDirty === this.dirty) return;

        this.dirty = isDirty;
        const saveBtn = this.querySelector('.file-save-btn');
        const statusEl = this.querySelector('.file-status');

        saveBtn.disabled = !isDirty;
        saveBtn.classList.toggle('dirty', isDirty);

        statusEl.className = 'file-status';
        if (isDirty) {
            statusEl.textContent = 'unsaved';
            statusEl.classList.add('unsaved');
        } else {
            statusEl.textContent = 'saved';
            statusEl.classList.add('saved');
        }

        // Mark active tab dirty
        const tabs = this.querySelectorAll('.file-tab');
        const activeTab = tabs[this.hasPermissionsTab ? this.activeIndex + 1 : this.activeIndex];
        if (activeTab) activeTab.classList.toggle('dirty', isDirty);
    }

    async save() {
        if (this.activeIndex < 0 || !this._title) return;

        const f = this.files[this.activeIndex];
        const editor = this.querySelector('.file-editor');
        const content = editor.value;
        const statusEl = this.querySelector('.file-status');
        const saveBtn = this.querySelector('.file-save-btn');

        try {
            await api.saveFile(this._title, this.endpoint, f.path, content);

            f.content = content;
            f.exists = true;
            this.savedContent = content;
            this.dirty = false;

            saveBtn.disabled = true;
            saveBtn.classList.remove('dirty');
            statusEl.className = 'file-status saved';
            statusEl.textContent = 'saved';

            const tabs = this.querySelectorAll('.file-tab');
            const activeTab = tabs[this.hasPermissionsTab ? this.activeIndex + 1 : this.activeIndex];
            if (activeTab) {
                activeTab.classList.remove('dirty', 'missing');
            }
        } catch (e) {
            statusEl.className = 'file-status error';
            statusEl.textContent = `save failed: ${e.message}`;
        }
    }
}

customElements.define('am-file-editor', AmFileEditor);
