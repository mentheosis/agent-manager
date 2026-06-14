/**
 * Permissions panel component - runtime settings (model, mode, directories).
 */

import * as api from '../lib/api.js';

class AmPermissionsPanel extends HTMLElement {
    constructor() {
        super();
        this._title = null;
        this._activeModel = null;  // Model from current running session
        this.provider = 'claude';
        this.workingDir = '';  // The instance's working directory
        this.permission_mode = 'acceptEdits';
        this.model = '';
        this.dirs = [];
        this.memoryFile = '';  // Path to memory file
        this.savedMode = 'acceptEdits';
        this.savedModel = '';
        this.savedDirs = [];
        this.savedMemoryFile = '';
    }

    connectedCallback() {
        this.className = 'permissions-panel';
        this.innerHTML = `
            <div class="perm-section">
                <label class="perm-label">Model</label>
                <select class="perm-model">
                    <option value="">Provider default</option>
                </select>
                <div class="perm-model-info">
                    <span class="model-current" hidden>Current session: <code class="model-current-value"></code></span>
                    <span class="model-pending" hidden>Pending restart</span>
                </div>
            </div>

            <div class="perm-section">
                <label class="perm-label perm-mode-label">Permission mode</label>
                <select class="perm-mode">
                    <option value="default">default</option>
                    <option value="acceptEdits">acceptEdits</option>
                    <option value="plan">plan</option>
                    <option value="bypassPermissions">bypassPermissions</option>
                </select>
            </div>

            <div class="perm-section">
                <label class="perm-label">Allowed directories</label>
                <ul class="dirs-list"></ul>
                <div class="dir-add-row">
                    <input class="dir-add-input" type="text" placeholder="/absolute/path" autocomplete="off" spellcheck="false">
                    <button type="button" class="dir-add-btn">Add</button>
                </div>
                <small class="hint">Each directory must also be mounted into the container via <code>docker-compose.local.yml</code>.</small>
            </div>

            <div class="perm-section">
                <label class="perm-label">Memory file</label>
                <div class="memory-file-row">
                    <input class="memory-file-input" type="text" placeholder="/path/to/memory.md" autocomplete="off" spellcheck="false">
                    <button type="button" class="memory-file-clear-btn" title="Clear">Clear</button>
                </div>
                <small class="hint">Contents are injected as persistent context: appended to Claude's system prompt, or passed as Codex's <code>developer_instructions</code>. Cacheable, so edits are picked up between turns without re-billing the tokens.</small>
            </div>

            <div class="perm-actions">
                <button type="button" class="perm-apply-btn">Restart and apply</button>
                <span class="perm-apply-status"></span>
            </div>

            <small class="hint">Applying restarts the provider session. Conversation history is preserved via session resume; any in-flight turn will be cancelled.</small>
        `;

        this.setupEventListeners();
    }

    setupEventListeners() {
        const modelEl = this.querySelector('.perm-model');
        const modeEl = this.querySelector('.perm-mode');
        const addBtn = this.querySelector('.dir-add-btn');
        const addInput = this.querySelector('.dir-add-input');
        const applyBtn = this.querySelector('.perm-apply-btn');
        const memoryFileInput = this.querySelector('.memory-file-input');
        const memoryFileClearBtn = this.querySelector('.memory-file-clear-btn');

        modelEl.addEventListener('change', () => {
            this.model = modelEl.value;
            this.refreshDirty();
            this.updateModelInfo();
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

        memoryFileInput.addEventListener('input', () => {
            this.memoryFile = memoryFileInput.value;
            this.refreshDirty();
        });

        memoryFileClearBtn.addEventListener('click', () => {
            this.memoryFile = '';
            memoryFileInput.value = '';
            this.refreshDirty();
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
            const inst = await api.fetchInstance(title);
            const providerName = inst.provider || 'claude';
            const [provider, models] = await Promise.all([
                api.fetchProvider(providerName).catch(() => null),
                api.fetchModels(providerName),
            ]);
            const defaultMode = provider?.runtime_options?.default_permission_mode || this.defaultModeForProvider(providerName);

            this.provider = providerName;
            this.workingDir = inst.path || '';
            this.permission_mode = this.resolvePermissionMode(provider, inst.permission_mode || defaultMode);
            this.model = inst.model || '';
            this.dirs = (inst.add_dirs || []).slice();
            this.memoryFile = inst.memory_file || '';
            this.savedMode = this.permission_mode;
            this.savedModel = this.model;
            this.savedDirs = this.dirs.slice();
            this.savedMemoryFile = this.memoryFile;

            this.populatePermissionModes(provider, this.permission_mode);
            this.updateProviderLabels(provider);
            this.populateModelDropdown(models, this.model);
            this.renderDirs();
            this.querySelector('.memory-file-input').value = this.memoryFile;
            this.updateModelInfo();

            statusEl.textContent = '';
            this.refreshDirty();  // Update button state (always enabled)
        } catch (e) {
            statusEl.textContent = `error: ${e.message}`;
        }
    }

    populatePermissionModes(provider, currentMode) {
        const el = this.querySelector('.perm-mode');
        const modes = provider?.runtime_options?.permission_modes || ['default', 'acceptEdits', 'plan', 'bypassPermissions'];
        const defaultMode = provider?.runtime_options?.default_permission_mode || modes[0] || '';
        el.innerHTML = '';

        for (const mode of modes) {
            const opt = document.createElement('option');
            opt.value = mode;
            opt.textContent = mode;
            el.appendChild(opt);
        }

        if (currentMode && !el.querySelector(`option[value="${CSS.escape(currentMode)}"]`)) {
            const opt = document.createElement('option');
            opt.value = currentMode;
            opt.textContent = `${currentMode} (custom)`;
            el.appendChild(opt);
        }

        el.value = currentMode || defaultMode;
        el.disabled = modes.length === 0;
    }

    resolvePermissionMode(provider, currentMode) {
        const modes = provider?.runtime_options?.permission_modes || [];
        const defaultMode = provider?.runtime_options?.default_permission_mode || this.defaultModeForProvider(this.provider);
        if (!currentMode) return defaultMode;
        if (modes.length > 0 && !modes.includes(currentMode)) return defaultMode;
        return currentMode;
    }

    defaultModeForProvider(providerName) {
        return providerName === 'codex' ? 'workspace-write' : 'acceptEdits';
    }

    updateProviderLabels(provider) {
        const modeLabel = this.querySelector('.perm-mode-label');
        modeLabel.textContent = provider?.provider === 'codex' ? 'Sandbox mode' : 'Permission mode';
    }

    populateModelDropdown(models, currentModel) {
        const el = this.querySelector('.perm-model');
        el.innerHTML = '<option value="">Provider default</option>';

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

        // Always show working directory first (not removable)
        if (this.workingDir) {
            const li = document.createElement('li');
            li.className = 'dir-working';

            const span = document.createElement('span');
            span.className = 'dir-path';
            span.textContent = this.workingDir;

            const label = document.createElement('span');
            label.className = 'dir-label';
            label.textContent = 'working dir';

            li.appendChild(span);
            li.appendChild(label);
            list.appendChild(li);
        }

        // Show additional directories (removable)
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
            || !this.sameStringList(this.dirs, this.savedDirs)
            || this.memoryFile !== this.savedMemoryFile;

        const applyBtn = this.querySelector('.perm-apply-btn');
        const statusEl = this.querySelector('.perm-apply-status');

        // Button always enabled - allows restart even without changes
        applyBtn.disabled = false;
        applyBtn.classList.toggle('dirty', dirty);
        applyBtn.textContent = dirty ? 'Restart and apply' : 'Restart session';
        if (dirty) statusEl.textContent = 'unsaved';
    }

    sameStringList(a, b) {
        if (a.length !== b.length) return false;
        for (let i = 0; i < a.length; i++) {
            if (a[i] !== b[i]) return false;
        }
        return true;
    }

    setActiveModel(model) {
        this._activeModel = model || null;
        this.updateModelInfo();
    }

    updateModelInfo() {
        const currentEl = this.querySelector('.model-current');
        const currentValueEl = this.querySelector('.model-current-value');
        const pendingEl = this.querySelector('.model-pending');

        const configuredModel = this.model;  // What's selected in dropdown (may be unsaved)
        const activeModel = this._activeModel;  // What the running session uses

        // Show current session model
        if (activeModel) {
            currentValueEl.textContent = activeModel;
            currentEl.hidden = false;
        } else {
            currentEl.hidden = true;
        }

        // Show "pending restart" if configured differs from active
        // (and there's an active session to compare against)
        const configuredEffective = configuredModel || 'default';
        const activeEffective = activeModel || '';
        const modelsDiffer = activeModel && configuredModel && configuredModel !== activeModel;
        const defaultChanged = activeModel && !configuredModel;  // Configured is "default" but we have an active model

        // Only show pending if user explicitly changed to a different model
        pendingEl.hidden = !modelsDiffer;
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
                memory_file: this.memoryFile || null,
            });

            this.permission_mode = this.resolvePermissionMode(
                { runtime_options: { permission_modes: [...this.querySelector('.perm-mode').options].map((opt) => opt.value) } },
                inst.permission_mode || this.defaultModeForProvider(this.provider),
            );
            this.model = inst.model || '';
            this.dirs = (inst.add_dirs || []).slice();
            this.memoryFile = inst.memory_file || '';
            this.savedMode = this.permission_mode;
            this.savedModel = this.model;
            this.savedDirs = this.dirs.slice();
            this.savedMemoryFile = this.memoryFile;

            this.querySelector('.perm-mode').value = this.permission_mode;
            this.querySelector('.perm-model').value = this.model;
            this.querySelector('.memory-file-input').value = this.memoryFile;
            this.renderDirs();

            statusEl.textContent = 'applied · session restarted';

            // Clear active model since session restarted - will be updated on next stream event
            this._activeModel = null;
            this.updateModelInfo();
            this.refreshDirty();
        } catch (e) {
            statusEl.textContent = `error: ${e.message}`;
            applyBtn.disabled = false;
        }
    }
}

customElements.define('am-permissions-panel', AmPermissionsPanel);
