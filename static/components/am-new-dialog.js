/**
 * New instance dialog component - creates either an agent or a team.
 */

import * as api from '../lib/api.js';

class AmNewDialog extends HTMLElement {
    constructor() {
        super();
        this._mode = 'agent';  // 'agent' or 'team'
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
                            <span class="mode-desc">Single Claude instance</span>
                        </button>
                        <button type="button" class="mode-btn" data-mode="team">
                            <span class="mode-icon">👥</span>
                            <span class="mode-label">Team</span>
                            <span class="mode-desc">Orchestrated group</span>
                        </button>
                    </div>

                    <!-- Agent Form -->
                    <form id="agent-form" class="mode-form active">
                        <label>
                            Name
                            <input name="name" required autocomplete="off" placeholder="My cool project">
                            <small class="hint">Used as the display label. A snake_case ID is generated for storage.</small>
                        </label>
                        <label>
                            Working directory
                            <input name="path" value="~" required autocomplete="off">
                        </label>
                        <label>
                            Permission mode
                            <select name="permission_mode">
                                <option value="acceptEdits" selected>acceptEdits</option>
                                <option value="default">default</option>
                                <option value="plan">plan</option>
                                <option value="bypassPermissions">bypassPermissions</option>
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
model: claude-sonnet-4-20250514  # optional
task: Build feature X with tests
agents:
  - name: coder-1
    path: /path/to/repo
    preset: coder
    model: claude-opus-4-20250514  # optional
  - name: researcher
    path: /path/to/docs
    preset: researcher"></textarea>
                        <div id="yaml-error" class="yaml-error"></div>
                        <menu>
                            <button type="button" class="btn-cancel">Cancel</button>
                            <button type="submit" class="btn-primary">Create Team</button>
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

        // Team form submit
        this.querySelector('#team-form').addEventListener('submit', (e) => {
            e.preventDefault();
            this.createTeam();
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

    async createAgent() {
        const form = this.querySelector('#agent-form');
        const data = Object.fromEntries(new FormData(form));
        const addDirsRaw = data.add_dirs || '';
        delete data.add_dirs;

        // Handle model - only include if set
        const model = data.model || null;
        delete data.model;

        const add_dirs = addDirsRaw
            .split(/\r?\n/)
            .map((l) => l.trim())
            .filter((l) => l.length > 0);

        const submitBtn = form.querySelector('button[type="submit"]');
        submitBtn.disabled = true;
        submitBtn.textContent = 'Creating...';

        try {
            const inst = await api.createInstance({ ...data, model, add_dirs });
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
        return value;
    }

    async open(mode = 'agent') {
        this.setMode(mode);
        await this.loadModels();
        this.querySelector('#new-dialog').showModal();
    }

    async loadModels() {
        try {
            const models = await api.fetchModels();
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
    }
}

customElements.define('am-new-dialog', AmNewDialog);
