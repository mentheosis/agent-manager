/**
 * New instance dialog component - creates either an agent or a team.
 */

import * as api from '../lib/api.js';

class AmNewDialog extends HTMLElement {
    constructor() {
        super();
        this._mode = 'agent';  // 'agent' | 'team' | 'batch'
        this.providers = [];
    }

    connectedCallback() {
        this.innerHTML = `
            <dialog id="new-dialog">
                <div class="dialog-content">
                    <h2>Create New</h2>

                    <div class="mode-selector">
                        <button type="button" class="mode-btn active" data-mode="agent">
                            <span class="mode-icon">🤖</span>
                            <span class="mode-label">Agent</span>
                            <span class="mode-desc">Single coding agent</span>
                        </button>
                        <button type="button" class="mode-btn" data-mode="team">
                            <span class="mode-icon">👥</span>
                            <span class="mode-label">Team</span>
                            <span class="mode-desc">Orchestrated group</span>
                        </button>
                        <button type="button" class="mode-btn" data-mode="batch">
                            <span class="mode-icon">📁</span>
                            <span class="mode-label">Batch</span>
                            <span class="mode-desc">From YAML directory</span>
                        </button>
                    </div>

                    <!-- Agent Form -->
                    <form id="agent-form" class="mode-form active">
                        <label>
                            Provider
                            <select name="provider">
                                <option value="claude" selected>Claude Code</option>
                            </select>
                        </label>
                        <div class="provider-auth-row">
                            <span class="provider-auth-status"></span>
                            <button type="button" class="provider-login-btn">Log in</button>
                        </div>
                        <label>
                            Name
                            <input name="name" autocomplete="off" placeholder="My cool project">
                            <small class="hint">Used as the display label. Can be set in YAML instead.</small>
                        </label>
                        <label>
                            Working directory
                            <input name="path" value="~" autocomplete="off" placeholder="~/my-project">
                            <small class="hint">Can be set in YAML instead.</small>
                        </label>
                        <label>
                            Permission mode
                            <select name="permission_mode">
                                <option value="acceptEdits" selected>acceptEdits</option>
                            </select>
                        </label>
                        <label>
                            Model
                            <select name="model">
                                <option value="" selected>Default</option>
                            </select>
                        </label>
                        <label>
                            Additional allowed directories (one per line, optional)
                            <textarea name="add_dirs" rows="2" autocomplete="off" spellcheck="false" placeholder="/path/to/other-project"></textarea>
                        </label>
                        <details class="yaml-config-section">
                            <summary>Advanced Config (YAML)</summary>
                            <textarea name="agent_yaml" class="yaml-input" rows="10" spellcheck="false" placeholder="# Optional YAML config (overrides form fields)
name: my-agent
path: /path/to/workspace
provider: claude
permission_mode: acceptEdits
model: claude-sonnet-4-20250514
memory_file: /path/to/AGENTS.md
add_dirs:
  - /path/to/other/repo
permissions:
  allow:
    - 'Bash(npm *)'
    - 'WebFetch'"></textarea>
                            <div class="yaml-hint">Claude permissions are merged into .claude/settings.json</div>
                        </details>
                        <menu>
                            <button type="button" class="btn-cancel">Cancel</button>
                            <button type="submit" class="btn-primary">Create Agent</button>
                        </menu>
                    </form>

                    <!-- Team Form -->
                    <form id="team-form" class="mode-form">
                        <p class="form-hint">Define your team using YAML configuration:</p>
                        <textarea id="team-yaml" class="yaml-input" spellcheck="false" placeholder="title: my-team
path: /path/to/workspace
provider: claude
model: claude-sonnet-4-20250514  # optional
memory_file: /path/to/team-memory.md  # optional
task: Build feature X with tests
agents:
  - name: coder-1
    path: /path/to/repo
    provider: claude
    preset: coder
    model: claude-opus-4-20250514  # optional
    memory_file: /path/to/coder-memory.md  # optional
  - name: researcher
    path: /path/to/docs
    provider: claude
    preset: researcher"></textarea>
                        <div id="yaml-error" class="yaml-error"></div>
                        <menu>
                            <button type="button" class="btn-cancel">Cancel</button>
                            <button type="submit" class="btn-primary">Create Team</button>
                        </menu>
                    </form>

                    <!-- Batch Form -->
                    <form id="batch-form" class="mode-form">
                        <p class="form-hint">Create one agent per YAML file in a directory:</p>
                        <label>
                            Directory containing YAML configs
                            <input name="directory" autocomplete="off" placeholder="~/agent-configs" required>
                            <small class="hint">Each .yaml or .yml file will create one agent</small>
                        </label>
                        <div id="batch-preview" class="batch-preview" hidden>
                            <div class="batch-preview-header">Found <span class="batch-count">0</span> YAML files:</div>
                            <ul class="batch-file-list"></ul>
                        </div>
                        <div id="batch-error" class="yaml-error"></div>
                        <menu>
                            <button type="button" class="btn-cancel">Cancel</button>
                            <button type="button" class="btn-secondary batch-scan-btn">Scan Directory</button>
                            <button type="submit" class="btn-primary" disabled>Create Agents</button>
                        </menu>
                    </form>
                </div>
            </dialog>
        `;

        this.setupEventListeners();
    }

    setupEventListeners() {
        // Mode selector
        this.querySelectorAll('.mode-btn').forEach(btn => {
            btn.addEventListener('click', () => this.setMode(btn.dataset.mode));
        });

        // Cancel buttons
        this.querySelectorAll('.btn-cancel').forEach(btn => {
            btn.addEventListener('click', () => this.close());
        });

        // Agent form submit
        this.querySelector('#agent-form').addEventListener('submit', (e) => {
            e.preventDefault();
            this.createAgent();
        });

        this.querySelector('select[name="provider"]').addEventListener('change', async (e) => {
            await this.applyProvider(e.target.value);
            this.syncAgentYamlFromFields();
        });
        this.setupAgentYamlSync();

        this.querySelector('.provider-login-btn').addEventListener('click', () => {
            const provider = this.querySelector('select[name="provider"]').value || 'claude';
            this.querySelector('#new-dialog').close();
            this.dispatchEvent(new CustomEvent('open-login-dialog', {
                bubbles: true,
                detail: { provider },
            }));
        });

        // Team form submit
        this.querySelector('#team-form').addEventListener('submit', (e) => {
            e.preventDefault();
            this.createTeam();
        });

        // Batch form
        this.querySelector('.batch-scan-btn').addEventListener('click', () => {
            this.scanBatchDirectory();
        });
        this.querySelector('#batch-form').addEventListener('submit', (e) => {
            e.preventDefault();
            this.createBatch();
        });

        // Close on backdrop click
        this.querySelector('#new-dialog').addEventListener('click', (e) => {
            if (e.target.id === 'new-dialog') {
                this.close();
            }
        });

        // Escape to close
        this.querySelector('#new-dialog').addEventListener('keydown', (e) => {
            if (e.key === 'Escape') {
                this.close();
            }
        });
    }

    setMode(mode) {
        this._mode = mode;

        // Update mode buttons
        this.querySelectorAll('.mode-btn').forEach(btn => {
            btn.classList.toggle('active', btn.dataset.mode === mode);
        });

        // Update forms
        this.querySelectorAll('.mode-form').forEach(form => {
            form.classList.toggle('active', form.id === `${mode}-form`);
        });

        // Focus first input
        const activeForm = this.querySelector(`.mode-form.active`);
        const firstInput = activeForm?.querySelector('input, textarea');
        if (firstInput) {
            setTimeout(() => firstInput.focus(), 50);
        }
    }

    setupAgentYamlSync() {
        const form = this.querySelector('#agent-form');
        const fields = [
            'input[name="name"]',
            'input[name="path"]',
            'select[name="permission_mode"]',
            'select[name="model"]',
            'textarea[name="add_dirs"]',
        ];
        for (const selector of fields) {
            const field = form.querySelector(selector);
            if (!field) continue;
            field.addEventListener('input', () => this.syncAgentYamlFromFields());
            field.addEventListener('change', () => this.syncAgentYamlFromFields());
        }
    }

    syncAgentYamlFromFields() {
        const yamlInput = this.querySelector('textarea[name="agent_yaml"]');
        if (!yamlInput) return;

        let existing = {};
        if (yamlInput.value.trim()) {
            try {
                existing = this.parseAgentYaml(yamlInput.value);
            } catch {
                existing = {};
            }
        }

        const config = { ...existing };
        const value = (selector) => this.querySelector(selector)?.value?.trim() || '';

        const name = value('input[name="name"]');
        const path = value('input[name="path"]');
        const provider = value('select[name="provider"]') || 'claude';
        const permissionMode = value('select[name="permission_mode"]');
        const model = value('select[name="model"]');
        const addDirs = (this.querySelector('textarea[name="add_dirs"]')?.value || '')
            .split(/\r?\n/)
            .map((line) => line.trim())
            .filter(Boolean);

        if (name) config.name = name;
        else delete config.name;
        if (path) config.path = path;
        else delete config.path;
        config.provider = provider;
        if (permissionMode) config.permission_mode = permissionMode;
        else delete config.permission_mode;
        if (model) config.model = model;
        else delete config.model;
        if (addDirs.length > 0) config.add_dirs = addDirs;
        else config.add_dirs = [];

        yamlInput.value = this.serializeAgentYaml(config);
    }

    serializeAgentYaml(config) {
        const lines = [];
        const pushScalar = (key) => {
            if (config[key] !== undefined && config[key] !== null && String(config[key]).trim() !== '') {
                lines.push(`${key}: ${this.yamlScalar(config[key])}`);
            }
        };

        for (const key of ['name', 'path', 'provider', 'permission_mode', 'model', 'memory_file']) {
            pushScalar(key);
        }

        if (Array.isArray(config.add_dirs) && config.add_dirs.length > 0) {
            lines.push('add_dirs:');
            for (const dir of config.add_dirs) {
                lines.push(`  - ${this.yamlScalar(dir)}`);
            }
        }

        const known = new Set(['name', 'path', 'provider', 'permission_mode', 'model', 'memory_file', 'add_dirs', 'permissions']);
        for (const [key, value] of Object.entries(config)) {
            if (known.has(key) || value === undefined || value === null || value === '') continue;
            if (Array.isArray(value) || typeof value === 'object') continue;
            lines.push(`${key}: ${this.yamlScalar(value)}`);
        }

        const allow = config.permissions?.allow || [];
        const deny = config.permissions?.deny || [];
        if (allow.length > 0 || deny.length > 0) {
            lines.push('permissions:');
            if (allow.length > 0) {
                lines.push('  allow:');
                for (const permission of allow) {
                    lines.push(`    - ${this.yamlScalar(permission)}`);
                }
            }
            if (deny.length > 0) {
                lines.push('  deny:');
                for (const permission of deny) {
                    lines.push(`    - ${this.yamlScalar(permission)}`);
                }
            }
        }

        return lines.length > 0 ? `${lines.join('\n')}\n` : '';
    }

    async createAgent() {
        const form = this.querySelector('#agent-form');
        const data = Object.fromEntries(new FormData(form));
        const addDirsRaw = data.add_dirs || '';
        const agentYaml = data.agent_yaml || '';
        delete data.add_dirs;
        delete data.agent_yaml;

        // Handle model - only include if set
        let provider = data.provider || 'claude';
        delete data.provider;

        let model = data.model || null;
        delete data.model;

        // Form values (can be overridden by YAML)
        let name = data.name;
        let path = data.path;
        let permission_mode = data.permission_mode;
        let memory_file = null;
        delete data.name;
        delete data.path;
        delete data.permission_mode;

        let add_dirs = addDirsRaw
            .split(/\r?\n/)
            .map((l) => l.trim())
            .filter((l) => l.length > 0);

        // Parse YAML config if provided (overrides form fields)
        let settings_json = null;
        if (agentYaml.trim()) {
            try {
                const config = this.parseAgentYaml(agentYaml);
                // Override form fields with YAML values
                if (config.name) name = config.name;
                if (config.path) path = config.path;
                if (config.provider) provider = config.provider;
                if (config.permission_mode) permission_mode = config.permission_mode;
                if (config.model) model = config.model;
                if (config.memory_file) memory_file = config.memory_file;
                if (config.add_dirs && Array.isArray(config.add_dirs)) {
                    // Merge YAML add_dirs with form add_dirs
                    add_dirs = [...new Set([...add_dirs, ...config.add_dirs])];
                }
                // Build settings_json from permissions if any were specified
                const hasAllow = config.permissions?.allow?.length > 0;
                const hasDeny = config.permissions?.deny?.length > 0;
                if (hasAllow || hasDeny) {
                    settings_json = { permissions: {} };
                    if (hasAllow) settings_json.permissions.allow = config.permissions.allow;
                    if (hasDeny) settings_json.permissions.deny = config.permissions.deny;
                }
            } catch (e) {
                alert(`YAML parse error: ${e.message}`);
                return;
            }
        }

        // Validate required fields (from form or YAML)
        if (!name || !name.trim()) {
            alert('Name is required (in form or YAML)');
            return;
        }
        if (!path || !path.trim()) {
            alert('Working directory is required (in form or YAML)');
            return;
        }

        const submitBtn = form.querySelector('button[type="submit"]');
        submitBtn.disabled = true;
        submitBtn.textContent = 'Creating...';

        try {
            const inst = await api.createInstance({ name, path, provider, kind: 'agent', permission_mode, model, add_dirs, memory_file, settings_json });
            this.close();
            form.reset();

            this.dispatchEvent(new CustomEvent('instance-created', {
                bubbles: true,
                detail: { title: inst.title }
            }));
        } catch (err) {
            alert(`Create failed: ${err.message}`);
        } finally {
            submitBtn.disabled = false;
            submitBtn.textContent = 'Create Agent';
        }
    }

    async createTeam() {
        const yamlText = this.querySelector('#team-yaml').value.trim();
        const errorDiv = this.querySelector('#yaml-error');
        errorDiv.textContent = '';

        if (!yamlText) {
            errorDiv.textContent = 'Please enter YAML configuration';
            return;
        }

        // Parse YAML
        let config;
        try {
            config = this.parseYaml(yamlText);
        } catch (e) {
            errorDiv.textContent = `YAML parse error: ${e.message}`;
            return;
        }

        // Validate
        if (!config.title) {
            errorDiv.textContent = 'Missing required field: title';
            return;
        }
        if (!config.path) {
            errorDiv.textContent = 'Missing required field: path';
            return;
        }

        const submitBtn = this.querySelector('#team-form button[type="submit"]');
        submitBtn.disabled = true;
        submitBtn.textContent = 'Creating...';

        try {
            // Create the loop instance first
            const loopResp = await fetch('/api/instances', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    name: config.title,
                    path: config.path,
                    permission_mode: 'plan',
                    model: config.model || null,
                    memory_file: config.memory_file || null,
                })
            });
            if (!loopResp.ok) {
                const err = await loopResp.json();
                throw new Error(err.detail || 'Failed to create loop instance');
            }
            const loopInst = await loopResp.json();

            // Set instance_type to 'loop'
            await fetch(`/api/instances/${encodeURIComponent(loopInst.title)}/type`, {
                method: 'PATCH',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ instance_type: 'loop' })
            });

            // Set the task
            if (config.task) {
                await fetch(`/api/instances/${encodeURIComponent(loopInst.title)}/task`, {
                    method: 'PATCH',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ task: config.task })
                });
            }

            // Create child agents
            if (config.agents && Array.isArray(config.agents)) {
                for (const agent of config.agents) {
                    if (!agent.name || !agent.path) continue;

                    // Create agent instance
                    const agentResp = await fetch('/api/instances', {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({
                            name: agent.name,
                            path: agent.path,
                            permission_mode: 'acceptEdits',
                            model: agent.model || null,
                            memory_file: agent.memory_file || null,
                        })
                    });
                    if (!agentResp.ok) {
                        console.error(`Failed to create agent ${agent.name}`);
                        continue;
                    }
                    const agentInst = await agentResp.json();

                    // Reparent to the loop instance
                    await fetch(`/api/instances/${encodeURIComponent(agentInst.title)}/reparent`, {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({ parent: loopInst.title })
                    });

                    // Set agent_preset if specified
                    if (agent.preset) {
                        await fetch(`/api/instances/${encodeURIComponent(agentInst.title)}/type`, {
                            method: 'PATCH',
                            headers: { 'Content-Type': 'application/json' },
                            body: JSON.stringify({ agent_preset: agent.preset })
                        });
                    }
                }
            }

            // Dispatch event and close
            this.dispatchEvent(new CustomEvent('instance-created', {
                bubbles: true,
                detail: { title: loopInst.title }
            }));
            this.close();
            this.querySelector('#team-yaml').value = '';

        } catch (e) {
            errorDiv.textContent = e.message;
        } finally {
            submitBtn.disabled = false;
            submitBtn.textContent = 'Create Team';
        }
    }

    async scanBatchDirectory() {
        const form = this.querySelector('#batch-form');
        const directory = form.querySelector('input[name="directory"]').value.trim();
        const errorDiv = this.querySelector('#batch-error');
        const previewDiv = this.querySelector('#batch-preview');
        const fileList = this.querySelector('.batch-file-list');
        const countSpan = this.querySelector('.batch-count');
        const submitBtn = form.querySelector('button[type="submit"]');
        const scanBtn = this.querySelector('.batch-scan-btn');

        errorDiv.textContent = '';
        previewDiv.hidden = true;
        submitBtn.disabled = true;

        if (!directory) {
            errorDiv.textContent = 'Please enter a directory path';
            return;
        }

        scanBtn.disabled = true;
        scanBtn.textContent = 'Scanning...';

        try {
            const resp = await fetch(`/api/batch/scan?directory=${encodeURIComponent(directory)}`);
            if (!resp.ok) {
                const err = await resp.json();
                throw new Error(err.detail || 'Failed to scan directory');
            }
            const data = await resp.json();
            this._batchFiles = data.files || [];

            if (this._batchFiles.length === 0) {
                errorDiv.textContent = 'No .yaml or .yml files found in directory';
                return;
            }

            // Show preview
            countSpan.textContent = this._batchFiles.length;
            fileList.innerHTML = '';
            for (const f of this._batchFiles) {
                const li = document.createElement('li');
                li.innerHTML = `<code>${f.filename}</code> → <strong>${f.config?.name || '(no name)'}</strong>`;
                if (f.error) {
                    li.innerHTML += ` <span class="batch-file-error">⚠ ${f.error}</span>`;
                }
                fileList.appendChild(li);
            }
            previewDiv.hidden = false;

            // Enable submit if we have valid files
            const validFiles = this._batchFiles.filter(f => !f.error && f.config?.name);
            submitBtn.disabled = validFiles.length === 0;
            submitBtn.textContent = `Create ${validFiles.length} Agent${validFiles.length !== 1 ? 's' : ''}`;

        } catch (e) {
            errorDiv.textContent = e.message;
        } finally {
            scanBtn.disabled = false;
            scanBtn.textContent = 'Scan Directory';
        }
    }

    async createBatch() {
        if (!this._batchFiles || this._batchFiles.length === 0) return;

        const form = this.querySelector('#batch-form');
        const errorDiv = this.querySelector('#batch-error');
        const submitBtn = form.querySelector('button[type="submit"]');

        const validFiles = this._batchFiles.filter(f => !f.error && f.config?.name);
        if (validFiles.length === 0) {
            errorDiv.textContent = 'No valid YAML configs to create';
            return;
        }

        submitBtn.disabled = true;
        submitBtn.textContent = 'Creating...';
        errorDiv.textContent = '';

        const created = [];
        const errors = [];

        try {
            for (const f of validFiles) {
                try {
                    const config = f.config;
                    const inst = await api.createInstance({
                        name: config.name,
                        path: config.path || '~',
                        provider: config.provider || 'claude',
                        kind: 'agent',
                        permission_mode: config.permission_mode || 'acceptEdits',
                        model: config.model || null,
                        add_dirs: config.add_dirs || [],
                        memory_file: config.memory_file || null,
                        settings_json: config.permissions ? { permissions: config.permissions } : null,
                    });
                    created.push(inst.title);
                } catch (e) {
                    errors.push(`${f.filename}: ${e.message}`);
                }
            }

            if (created.length > 0) {
                this.dispatchEvent(new CustomEvent('instance-created', {
                    bubbles: true,
                    detail: { title: created[0] }  // Select first created
                }));
            }

            if (errors.length > 0) {
                errorDiv.textContent = `Created ${created.length}, failed ${errors.length}: ${errors[0]}`;
            } else {
                this.close();
                this._batchFiles = null;
            }
        } finally {
            submitBtn.disabled = false;
            submitBtn.textContent = `Create ${validFiles.length} Agent${validFiles.length !== 1 ? 's' : ''}`;
        }
    }

    /**
     * Simple YAML parser for our specific schema.
     */
    parseYaml(text) {
        const result = { agents: [] };
        const lines = text.split('\n');
        let currentAgent = null;
        let inAgents = false;

        for (let line of lines) {
            if (!line.trim() || line.trim().startsWith('#')) continue;

            const indent = line.search(/\S/);
            line = line.trim();

            // Top-level fields
            if (indent === 0 && line.includes(':')) {
                const [key, ...valueParts] = line.split(':');
                const value = valueParts.join(':').trim();

                if (key === 'agents') {
                    inAgents = true;
                    continue;
                }

                inAgents = false;
                result[key.trim()] = this.parseValue(value);
                continue;
            }

            // Agent list item start
            if (inAgents && line.startsWith('- ')) {
                if (currentAgent) {
                    result.agents.push(currentAgent);
                }
                currentAgent = {};

                const rest = line.substring(2).trim();
                if (rest.includes(':')) {
                    const [key, ...valueParts] = rest.split(':');
                    currentAgent[key.trim()] = this.parseValue(valueParts.join(':').trim());
                }
                continue;
            }

            // Agent properties
            if (inAgents && currentAgent && indent >= 2 && line.includes(':')) {
                const [key, ...valueParts] = line.split(':');
                currentAgent[key.trim()] = this.parseValue(valueParts.join(':').trim());
            }
        }

        if (currentAgent) {
            result.agents.push(currentAgent);
        }

        return result;
    }

    parseValue(value) {
        if (!value) return '';
        if ((value.startsWith('"') && value.endsWith('"')) ||
            (value.startsWith("'") && value.endsWith("'"))) {
            return value.slice(1, -1);
        }
        // Parse booleans
        if (value === 'true') return true;
        if (value === 'false') return false;
        // Parse numbers
        if (/^-?\d+$/.test(value)) return parseInt(value, 10);
        if (/^-?\d+\.\d+$/.test(value)) return parseFloat(value);
        return value;
    }

    /**
     * Parse YAML for agent config with nested permissions.
     * Supports: model, add_dirs (list), permissions (object with allow/deny arrays)
     */
    parseAgentYaml(text) {
        const result = { add_dirs: [], permissions: { allow: [], deny: [] } };
        const lines = text.split('\n');
        let currentSection = null;  // 'add_dirs' | 'permissions'
        let currentPermKey = null;  // 'allow' | 'deny'

        for (let line of lines) {
            if (!line.trim() || line.trim().startsWith('#')) continue;

            const indent = line.search(/\S/);
            line = line.trim();

            // Top-level fields
            if (indent === 0 && line.includes(':')) {
                const [key, ...valueParts] = line.split(':');
                const keyName = key.trim();
                const value = valueParts.join(':').trim();

                if (keyName === 'add_dirs') {
                    currentSection = 'add_dirs';
                    currentPermKey = null;
                    continue;
                }
                if (keyName === 'permissions') {
                    currentSection = 'permissions';
                    currentPermKey = null;
                    continue;
                }

                currentSection = null;
                currentPermKey = null;
                result[keyName] = this.parseValue(value);
                continue;
            }

            // add_dirs list items
            if (currentSection === 'add_dirs' && line.startsWith('- ')) {
                const path = line.substring(2).trim();
                if (path) {
                    result.add_dirs.push(this.parseValue(path));
                }
                continue;
            }

            // Permissions sub-keys (allow/deny)
            if (currentSection === 'permissions' && indent === 2 && line.includes(':')) {
                const [key, ...valueParts] = line.split(':');
                const keyName = key.trim();
                if (keyName === 'allow' || keyName === 'deny') {
                    currentPermKey = keyName;
                }
                continue;
            }

            // Permissions list items
            if (currentSection === 'permissions' && currentPermKey && line.startsWith('- ')) {
                const perm = line.substring(2).trim();
                if (perm) {
                    result.permissions[currentPermKey].push(this.parseValue(perm));
                }
            }
        }

        return result;
    }

    async open(mode = 'agent', options = {}) {
        this.setMode(mode);
        await this.loadProviders();
        const duplicateFrom = options.duplicateFrom || null;
        const providerName = duplicateFrom?.provider || this.querySelector('select[name="provider"]').value || 'claude';
        await this.applyProvider(providerName);
        if (duplicateFrom) {
            this.prefillDuplicateAgent(duplicateFrom);
        } else if (mode === 'agent') {
            this.syncAgentYamlFromFields();
        }
        this.querySelector('#new-dialog').showModal();
    }

    prefillDuplicateAgent(inst) {
        const providerSelect = this.querySelector('select[name="provider"]');
        const yamlDetails = this.querySelector('.yaml-config-section');
        const yamlInput = this.querySelector('textarea[name="agent_yaml"]');
        const nameInput = this.querySelector('input[name="name"]');
        const pathInput = this.querySelector('input[name="path"]');
        const modeSelect = this.querySelector('select[name="permission_mode"]');
        const modelSelect = this.querySelector('select[name="model"]');
        const addDirsInput = this.querySelector('textarea[name="add_dirs"]');

        this.setSelectValue(providerSelect, inst.provider || 'claude');
        if (nameInput) nameInput.value = `${this.displayName(inst)} copy`;
        if (pathInput) pathInput.value = inst.path || '~';
        if (inst.permission_mode) this.setSelectValue(modeSelect, inst.permission_mode);
        if (inst.model) this.setSelectValue(modelSelect, inst.model);
        if (addDirsInput) addDirsInput.value = Array.isArray(inst.add_dirs) ? inst.add_dirs.join('\n') : '';

        if (yamlInput) {
            yamlInput.value = this.buildDuplicateYaml(inst);
        }
        if (yamlDetails) {
            yamlDetails.open = true;
        }
    }

    buildDuplicateYaml(inst) {
        const config = {
            name: `${this.displayName(inst)} copy`,
            path: inst.path || '~',
            provider: inst.provider || 'claude',
            add_dirs: Array.isArray(inst.add_dirs) ? inst.add_dirs : [],
        };
        if (inst.permission_mode) {
            config.permission_mode = inst.permission_mode;
        }
        if (inst.model) {
            config.model = inst.model;
        }
        if (inst.memory_file) {
            config.memory_file = inst.memory_file;
        }
        return this.serializeAgentYaml(config);
    }

    displayName(inst) {
        return inst.display_title || inst.title || 'Agent';
    }

    setSelectValue(select, value) {
        if (!select || !value) return;
        if (![...select.options].some((option) => option.value === value)) {
            const option = document.createElement('option');
            option.value = value;
            option.textContent = value;
            select.appendChild(option);
        }
        select.value = value;
    }

    yamlScalar(value) {
        const text = String(value ?? '');
        if (/^[A-Za-z0-9_./~:@-]+$/.test(text) && text !== '') {
            return text;
        }
        return JSON.stringify(text);
    }

    async loadProviders() {
        try {
            this.providers = await api.fetchProviders();
            const select = this.querySelector('select[name="provider"]');
            select.innerHTML = '';
            for (const provider of this.providers) {
                const opt = document.createElement('option');
                opt.value = provider.provider;
                opt.textContent = provider.enabled ? provider.label : `${provider.label} (coming soon)`;
                opt.disabled = !provider.enabled;
                select.appendChild(opt);
            }
            if (!select.value) select.value = 'claude';
        } catch (e) {
            console.error('Failed to load providers:', e);
        }
    }

    async applyProvider(providerName) {
        const provider = this.providers.find((p) => p.provider === providerName)
            || await api.fetchProvider(providerName).catch(() => null);
        if (!provider) return;

        const modeSelect = this.querySelector('select[name="permission_mode"]');
        const modes = provider.runtime_options?.permission_modes || [];
        const defaultMode = provider.runtime_options?.default_permission_mode || '';
        modeSelect.innerHTML = '';
        for (const mode of modes) {
            const opt = document.createElement('option');
            opt.value = mode;
            opt.textContent = mode;
            opt.selected = mode === defaultMode;
            modeSelect.appendChild(opt);
        }
        modeSelect.disabled = modes.length === 0;

        await this.checkProviderAuth(providerName, provider);
        await this.loadModels(providerName);
    }

    async checkProviderAuth(providerName, provider) {
        const statusEl = this.querySelector('.provider-auth-status');
        const loginBtn = this.querySelector('.provider-login-btn');
        statusEl.textContent = '';
        loginBtn.hidden = false;
        loginBtn.disabled = true;
        loginBtn.textContent = 'Checking auth...';

        try {
            const status = await api.checkAuth(providerName);
            loginBtn.textContent = `Log in to ${provider.label || providerName}`;
            if (status.authed) {
                statusEl.textContent = `${provider.label || providerName} authenticated`;
                loginBtn.disabled = true;
                loginBtn.textContent = 'Authenticated';
                return;
            }
            statusEl.textContent = `${provider.label || providerName} not authenticated`;
            loginBtn.disabled = !provider.auth?.login_supported;
            if (loginBtn.disabled) loginBtn.textContent = 'Login unavailable';
        } catch {
            statusEl.textContent = `${provider.label || providerName} auth status unavailable`;
            loginBtn.disabled = true;
            loginBtn.textContent = 'Login unavailable';
        }
    }

    async loadModels(provider = 'claude') {
        try {
            const models = await api.fetchModels(provider);
            const select = this.querySelector('select[name="model"]');

            // Clear existing options except default
            select.innerHTML = '<option value="" selected>Default</option>';

            // Add fetched models
            for (const id of models) {
                const opt = document.createElement('option');
                opt.value = id;
                opt.textContent = id;
                select.appendChild(opt);
            }
        } catch (e) {
            console.error('Failed to load models:', e);
        }
    }

    close() {
        this.querySelector('#new-dialog').close();
        // Reset forms
        this.querySelector('#agent-form').reset();
        this.querySelector('#team-yaml').value = '';
        this.querySelector('#yaml-error').textContent = '';
        // Reset batch form
        this.querySelector('#batch-form').reset();
        this.querySelector('#batch-preview').hidden = true;
        this.querySelector('#batch-error').textContent = '';
        this.querySelector('#batch-form button[type="submit"]').disabled = true;
        this.querySelector('#batch-form button[type="submit"]').textContent = 'Create Agents';
        this._batchFiles = null;
    }
}

customElements.define('am-new-dialog', AmNewDialog);
