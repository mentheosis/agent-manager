/**
 * Permissions panel component - SDK settings (model, mode, directories).
 */

import * as api from '../lib/api.js';

class AmPermissionsPanel extends HTMLElement {
    constructor() {
        super();
        this._title = null;
        this.permission_mode = 'acceptEdits';
        this.model = '';
        this.dirs = [];
        this.savedMode = 'acceptEdits';
        this.savedModel = '';
        this.savedDirs = [];
    }

    connectedCallback() {
        this.className = 'permissions-panel';
        this.innerHTML = `
            <div class="perm-section">
                <label class="perm-label">Model</label>
                <select class="perm-model">
                    <option value="">SDK default</option>
                </select>
            </div>

            <div class="perm-section">
                <label class="perm-label">Permission mode</label>
                <select class="perm-mode">
                    <option value="default">default</option>
                    <option value="acceptEdits">acceptEdits</option>
                    <option value="plan">plan</option>
                    <option value="bypassPermissions">bypassPermissions</option>
                </select>
            </div>

            <div class="perm-section">
                <label class="perm-label">Allowed directories (in addition to working dir)</label>
                <ul class="dirs-list"></ul>
                <div class="dir-add-row">
                    <input class="dir-add-input" type="text" placeholder="/absolute/path" autocomplete="off" spellcheck="false">
                    <button type="button" class="dir-add-btn">Add</button>
                </div>
                <small class="hint">Each directory must also be mounted into the container via <code>docker-compose.local.yml</code>.</small>
            </div>

            <div class="perm-actions">
                <button type="button" class="perm-apply-btn">Restart and apply</button>
                <span class="perm-apply-status"></span>
            </div>

            <small class="hint">Applying restarts the SDK session. Conversation history is preserved via session resume; any in-flight turn will be cancelled.</small>
        `;

        this.setupEventListeners();
    }

    setupEventListeners() {
        const modelEl = this.querySelector('.perm-model');
        const modeEl = this.querySelector('.perm-mode');
        const addBtn = this.querySelector('.dir-add-btn');
        const addInput = this.querySelector('.dir-add-input');
        const applyBtn = this.querySelector('.perm-apply-btn');

        modelEl.addEventListener('change', () => {
            this.model = modelEl.value;
            this.refreshDirty();
        });

        modeEl.addEventListener('change', () => {
            this.permission_mode = modeEl.value;
            this.refreshDirty();
        });

        addBtn.addEventListener('click', () => this.addDir());
        addInput.addEventListener('keydown', (e) => {
            if (e.key === 'Enter') {
                e.preventDefault();
                this.addDir();
            }
        });

        applyBtn.addEventListener('click', () => this.apply());
    }

    async load(title) {
        if (!title) return;
        this._title = title;

        const statusEl = this.querySelector('.perm-apply-status');
        const applyBtn = this.querySelector('.perm-apply-btn');

        statusEl.textContent = 'loading…';
        applyBtn.disabled = true;

        try {
            const [inst, models] = await Promise.all([
                api.fetchInstance(title),
                api.fetchModels(),
            ]);

            this.permission_mode = inst.permission_mode || 'acceptEdits';
            this.model = inst.model || '';
            this.dirs = (inst.add_dirs || []).slice();
            this.savedMode = this.permission_mode;
            this.savedModel = this.model;
            this.savedDirs = this.dirs.slice();

            this.querySelector('.perm-mode').value = this.permission_mode;
            this.populateModelDropdown(models, this.model);
            this.renderDirs();

            statusEl.textContent = '';
            applyBtn.disabled = true;
        } catch (e) {
            statusEl.textContent = `error: ${e.message}`;
        }
    }

    populateModelDropdown(models, currentModel) {
        const el = this.querySelector('.perm-model');
        el.innerHTML = '<option value="">SDK default</option>';

        for (const id of models) {
            const opt = document.createElement('option');
            opt.value = id;
            opt.textContent = id;
            el.appendChild(opt);
        }

        // Add current model if not in list
        if (currentModel && !el.querySelector(`option[value="${CSS.escape(currentModel)}"]`)) {
            const opt = document.createElement('option');
            opt.value = currentModel;
            opt.textContent = `${currentModel} (custom)`;
            el.appendChild(opt);
        }

        el.value = currentModel;
    }

    renderDirs() {
        const list = this.querySelector('.dirs-list');
        list.innerHTML = '';

        if (this.dirs.length === 0) {
            const li = document.createElement('li');
            li.className = 'dirs-empty';
            li.textContent = 'Only the working directory is accessible. Add paths below to extend.';
            list.appendChild(li);
            return;
        }

        for (let i = 0; i < this.dirs.length; i++) {
            const li = document.createElement('li');

            const span = document.createElement('span');
            span.className = 'dir-path';
            span.textContent = this.dirs[i];

            const btn = document.createElement('button');
            btn.type = 'button';
            btn.className = 'dir-remove';
            btn.title = 'Remove';
            btn.textContent = '×';
            btn.addEventListener('click', () => {
                this.dirs.splice(i, 1);
                this.renderDirs();
                this.refreshDirty();
            });

            li.appendChild(span);
            li.appendChild(btn);
            list.appendChild(li);
        }
    }

    addDir() {
        const input = this.querySelector('.dir-add-input');
        const statusEl = this.querySelector('.perm-apply-status');
        const path = input.value.trim();

        if (!path) return;
        if (this.dirs.includes(path)) {
            statusEl.textContent = `already in list: ${path}`;
            return;
        }

        this.dirs.push(path);
        input.value = '';
        statusEl.textContent = '';
        this.renderDirs();
        this.refreshDirty();
    }

    refreshDirty() {
        const dirty = this.permission_mode !== this.savedMode
            || this.model !== this.savedModel
            || !this.sameStringList(this.dirs, this.savedDirs);

        const applyBtn = this.querySelector('.perm-apply-btn');
        const statusEl = this.querySelector('.perm-apply-status');

        applyBtn.disabled = !dirty;
        applyBtn.classList.toggle('dirty', dirty);
        if (dirty) statusEl.textContent = 'unsaved';
    }

    sameStringList(a, b) {
        if (a.length !== b.length) return false;
        for (let i = 0; i < a.length; i++) {
            if (a[i] !== b[i]) return false;
        }
        return true;
    }

    async apply() {
        if (!this._title) return;

        const applyBtn = this.querySelector('.perm-apply-btn');
        const statusEl = this.querySelector('.perm-apply-status');

        applyBtn.disabled = true;
        statusEl.textContent = 'restarting session…';

        try {
            const inst = await api.updatePermissions(this._title, {
                permission_mode: this.permission_mode,
                model: this.model || null,
                add_dirs: this.dirs,
            });

            this.permission_mode = inst.permission_mode || 'acceptEdits';
            this.model = inst.model || '';
            this.dirs = (inst.add_dirs || []).slice();
            this.savedMode = this.permission_mode;
            this.savedModel = this.model;
            this.savedDirs = this.dirs.slice();

            this.querySelector('.perm-mode').value = this.permission_mode;
            this.querySelector('.perm-model').value = this.model;
            this.renderDirs();

            statusEl.textContent = 'applied · session restarted';
            applyBtn.classList.remove('dirty');
        } catch (e) {
            statusEl.textContent = `error: ${e.message}`;
            applyBtn.disabled = false;
        }
    }
}

customElements.define('am-permissions-panel', AmPermissionsPanel);
