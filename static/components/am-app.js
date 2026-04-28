/**
 * Root application component - orchestrates global state and child components.
 */

import * as api from '../lib/api.js';
import { streamManager } from '../lib/streams.js';

// Import all components
import './am-auth-banner.js';
import './am-sidebar.js';
import './am-toolbar.js';
import './am-tabs.js';
import './am-terminal-pane.js';
import './am-diff-pane.js';
import './am-file-editor.js';
import './am-new-dialog.js';
import './am-login-dialog.js';

class AmApp extends HTMLElement {
    constructor() {
        super();
        this.instances = [];
        this.currentTitle = null;
        this.currentInst = null;
        this.activeTab = 'terminal';
    }

    connectedCallback() {
        this.innerHTML = `
            <am-auth-banner></am-auth-banner>
            <div id="sidebar-backdrop"></div>
            <am-sidebar></am-sidebar>
            <div id="main">
                <div id="main-header">
                    <button id="sidebar-expand" type="button" title="Show sidebar" aria-label="Show sidebar">
                        <svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor">
                            <path d="M5.5 3L10.5 8l-5 5" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/>
                        </svg>
                    </button>
                    <h1 id="main-header-brand"><img src="/favicon.svg" alt=""> Agent Manager</h1>
                </div>
                <div id="empty-state">Select or create an instance to get started.</div>
                <div id="active-view">
                    <am-tabs></am-tabs>
                    <am-toolbar></am-toolbar>
                    <div id="tab-content">
                        <am-terminal-pane data-pane="terminal" class="tab-pane active"></am-terminal-pane>
                        <am-diff-pane data-pane="diff" class="tab-pane"></am-diff-pane>
                        <am-file-editor data-pane="settings" data-endpoint="rules" data-has-permissions="true" class="tab-pane"></am-file-editor>
                        <am-file-editor data-pane="plans" data-endpoint="plans" class="tab-pane"></am-file-editor>
                        <am-file-editor data-pane="memory" data-endpoint="memory" class="tab-pane"></am-file-editor>
                    </div>
                </div>
            </div>
            <am-new-dialog></am-new-dialog>
            <am-login-dialog></am-login-dialog>
        `;

        this.setupEventListeners();
        this.init();
    }

    setupEventListeners() {
        // Sidebar expand (only visible when sidebar is hidden)
        this.querySelector('#sidebar-expand').addEventListener('click', () => {
            this.openSidebar();
        });

        this.querySelector('#sidebar-backdrop').addEventListener('click', () => {
            this.closeSidebar();
        });

        this.addEventListener('close-sidebar', () => {
            this.closeSidebar();
        });

        // Instance selection
        this.addEventListener('instance-selected', (e) => {
            this.selectInstance(e.detail.instance);
            // Auto-close sidebar on mobile after selection
            if (window.innerWidth <= 768) {
                this.closeSidebar();
            }
        });

        // Instance created
        this.addEventListener('instance-created', (e) => {
            this.loadInstances().then(() => {
                const inst = this.instances.find(i => i.title === e.detail.title);
                if (inst) this.selectInstance(inst);
            });
        });

        // Instance deleted
        this.addEventListener('instance-deleted', (e) => {
            if (this.currentTitle === e.detail.title) {
                this.deselectInstance();
            }
            this.loadInstances();
        });

        // Instance renamed
        this.addEventListener('instance-renamed', () => {
            this.loadInstances();
        });

        // Instances reordered
        this.addEventListener('instances-reordered', () => {
            this.loadInstances();
        });

        // Tab changed
        this.addEventListener('tab-changed', (e) => {
            this.setActiveTab(e.detail.tab);
        });

        // Open new dialog
        this.addEventListener('open-new-dialog', () => {
            this.querySelector('am-new-dialog').open();
        });

        // Open login dialog
        this.addEventListener('open-login-dialog', () => {
            this.querySelector('am-login-dialog').open();
        });

        // Auth changed
        this.addEventListener('auth-changed', () => {
            this.checkAuth();
        });
    }

    async init() {
        this.initSidebarState();

        await Promise.all([
            this.checkAuth(),
            this.loadInstances(),
        ]);

        // Polling (reduced frequency)
        setInterval(() => this.loadInstances(), 30000);
        setInterval(() => this.checkAuth(), 30000);
    }

    async checkAuth() {
        try {
            const { authed } = await api.checkAuth();
            this.querySelector('am-auth-banner').authed = authed;
        } catch (e) {
            console.error('Auth check failed', e);
        }
    }

    async loadInstances() {
        try {
            this.instances = await api.fetchInstances();

            // Sync stream manager
            streamManager.sync(this.instances.map(i => i.title));

            // Ensure streams exist for all instances
            for (const inst of this.instances) {
                streamManager.get(inst.title);
            }

            // Update sidebar
            const sidebar = this.querySelector('am-sidebar');
            sidebar.instances = this.instances;

            // Update current instance if it changed
            if (this.currentTitle) {
                const updated = this.instances.find(i => i.title === this.currentTitle);
                if (updated) {
                    this.currentInst = updated;
                    this.querySelector('am-toolbar').instance = updated;
                }
            }
        } catch (e) {
            console.error('Failed to load instances', e);
        }
    }

    selectInstance(inst) {
        this.currentTitle = inst.title;
        this.currentInst = inst;

        // Show active view, hide empty state
        this.querySelector('#empty-state').style.display = 'none';
        this.querySelector('#active-view').classList.add('visible');

        // Update sidebar selection
        const sidebar = this.querySelector('am-sidebar');
        sidebar.selectedTitle = inst.title;

        // Update toolbar
        this.querySelector('am-toolbar').instance = inst;

        // Update terminal pane
        const terminal = this.querySelector('am-terminal-pane');
        terminal.instance = inst;

        // Enable prompt form
        terminal.enablePrompt();

        // Load active tab content
        this.onTabActivated(this.activeTab);
    }

    deselectInstance() {
        this.currentTitle = null;
        this.currentInst = null;

        // Show empty state, hide active view
        this.querySelector('#empty-state').style.display = '';
        this.querySelector('#active-view').classList.remove('visible');

        // Clear sidebar selection
        const sidebar = this.querySelector('am-sidebar');
        sidebar.selectedTitle = null;

        // Clear toolbar
        this.querySelector('am-toolbar').instance = null;

        // Clear terminal
        const terminal = this.querySelector('am-terminal-pane');
        terminal.instance = null;
        terminal.disablePrompt();
    }

    setActiveTab(name) {
        this.activeTab = name;

        // Update tab bar
        this.querySelector('am-tabs').activeTab = name;

        // Update pane visibility
        const panes = this.querySelectorAll('#tab-content > .tab-pane');
        for (const pane of panes) {
            const isActive = pane.dataset.pane === name;
            pane.classList.toggle('active', isActive);
        }

        this.onTabActivated(name);
    }

    onTabActivated(name) {
        if (!this.currentTitle) return;

        switch (name) {
            case 'terminal':
                // Terminal auto-updates via WebSocket
                break;
            case 'diff':
                this.querySelector('am-diff-pane').load(this.currentTitle);
                break;
            case 'settings':
            case 'plans':
            case 'memory':
                const editor = this.querySelector(`am-file-editor[data-pane="${name}"]`);
                editor.load(this.currentTitle);
                break;
        }
    }

    // Sidebar visibility
    toggleSidebar() {
        const sidebar = this.querySelector('am-sidebar');
        if (sidebar.classList.contains('collapsed')) {
            this.openSidebar();
        } else {
            this.closeSidebar();
        }
    }

    closeSidebar() {
        const sidebar = this.querySelector('am-sidebar');
        const backdrop = this.querySelector('#sidebar-backdrop');
        sidebar.classList.add('collapsed');
        backdrop.classList.remove('visible');
        this.classList.add('sidebar-collapsed');  // For CSS fallback
    }

    openSidebar() {
        const sidebar = this.querySelector('am-sidebar');
        const backdrop = this.querySelector('#sidebar-backdrop');
        sidebar.classList.remove('collapsed');
        backdrop.classList.add('visible');
        this.classList.remove('sidebar-collapsed');  // For CSS fallback
    }

    initSidebarState() {
        // Start collapsed on mobile
        if (window.innerWidth <= 768) {
            this.closeSidebar();
        } else {
            // Ensure class state matches on desktop
            this.classList.remove('sidebar-collapsed');
        }
    }

    // Public method to get current instance
    getInstance() {
        return this.currentInst;
    }

    getStream() {
        return this.currentTitle ? streamManager.get(this.currentTitle) : null;
    }
}

customElements.define('am-app', AmApp);
