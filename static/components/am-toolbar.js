/**
 * Toolbar component - mode selector, title, and action buttons.
 */

import * as api from '../lib/api.js';

class AmToolbar extends HTMLElement {
    constructor() {
        super();
        this._instance = null;
    }

    connectedCallback() {
        this.id = 'toolbar';
        this.innerHTML = `
            <div class="mode-selector">
                <span>Mode:</span>
                <button class="mode-btn" id="mode-agent" type="button">Agent</button>
                <button class="mode-btn" id="mode-plan" type="button">Plan</button>
            </div>
            <div class="toolbar-divider"></div>
            <input id="toolbar-title" type="text" placeholder="Untitled" spellcheck="false">
            <div style="flex:1"></div>
            <button class="toolbar-btn" id="btn-pause" type="button">Pause</button>
            <button class="toolbar-btn" id="btn-resume" type="button">Resume</button>
            <button class="toolbar-btn danger" id="btn-kill" type="button">Kill</button>
        `;

        this.setupEventListeners();
    }

    setupEventListeners() {
        // Mode buttons (placeholder - not fully implemented)
        this.querySelector('#mode-agent').addEventListener('click', () => {
            alert('Mode switching mid-session isn\'t implemented yet — set the mode when creating the instance.');
        });
        this.querySelector('#mode-plan').addEventListener('click', () => {
            alert('Mode switching mid-session isn\'t implemented yet — set the mode when creating the instance.');
        });

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

        // Action buttons (placeholders)
        this.querySelector('#btn-pause').addEventListener('click', () => {
            alert('Pause not implemented yet.');
        });
        this.querySelector('#btn-resume').addEventListener('click', () => {
            alert('Resume not implemented yet.');
        });
        this.querySelector('#btn-kill').addEventListener('click', () => {
            alert('Kill not implemented yet.');
        });
    }

    get instance() {
        return this._instance;
    }

    set instance(inst) {
        this._instance = inst;
        this.update();
    }

    update() {
        const titleInput = this.querySelector('#toolbar-title');
        if (this._instance) {
            titleInput.value = this._instance.display_title || this._instance.title;
            titleInput.disabled = false;
        } else {
            titleInput.value = '';
            titleInput.disabled = true;
        }
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
}

customElements.define('am-toolbar', AmToolbar);
