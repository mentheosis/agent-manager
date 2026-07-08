/**
 * Root application component - orchestrates global state and child components.
 */

import * as api from '../lib/api.js';
import { streamManager } from '../lib/streams.js';
import { idleNotificationsEnabled, showIdleNotification } from '../lib/notifications.js';

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
import './am-team-panel.js';

// Map internal tab names to URL-friendly names
const TAB_TO_URL = {
    terminal: 'conversation',
    diff: 'diff',
    settings: 'settings',
    plans: 'plans',
    memory: 'memory',
};
const URL_TO_TAB = Object.fromEntries(
    Object.entries(TAB_TO_URL).map(([k, v]) => [v, k])
);

class AmApp extends HTMLElement {
    constructor() {
        super();
        this.instances = [];
        this.currentTitle = null;
        this.currentInst = null;
        this.activeTab = 'terminal';
        this._skipUrlUpdate = false;  // Flag to prevent URL update during restore

        // Connection / disconnection tracking
        this._disconnected = false;
        this._disconnectTimer = null;  // Debounce timer before showing disconnected banner
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
            <am-team-panel></am-team-panel>
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

        this.addEventListener('open-sidebar', () => {
            this.openSidebar();
        });

        // Instance selection
        this.addEventListener('instance-selected', (e) => {
            this.selectInstance(e.detail.instance);
            // Auto-close sidebar on mobile after selection
            if (window.innerWidth <= 1100) {
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

        // Instance deleting (show deleting status immediately)
        this.addEventListener('instance-deleting', (e) => {
            const sidebar = this.querySelector('am-sidebar');
            sidebar.setDeleting(e.detail.title, e.detail.children || []);
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
        this.addEventListener('open-new-dialog', (e) => {
            this.querySelector('am-new-dialog').open('agent', e.detail || {});
        });

        this.addEventListener('duplicate-instance', (e) => {
            this.querySelector('am-new-dialog').open('agent', {
                duplicateFrom: e.detail.instance,
            });
        });

        // Open login dialog
        this.addEventListener('open-login-dialog', (e) => {
            this.querySelector('am-login-dialog').open(e.detail?.provider || 'claude');
        });

        // Auth changed (e.g. a login/re-auth completed)
        this.addEventListener('auth-changed', () => {
            this.checkAuth();
            const panel = this.querySelector('am-permissions-panel');
            if (panel && typeof panel.refreshAuthStatus === 'function') {
                panel.refreshAuthStatus();
            }
        });

        // Scroll terminal to bottom
        this.addEventListener('scroll-to-bottom', () => {
            this.querySelector('am-terminal-pane').scrollToBottom();
        });

        // Manual reconnect requested from the disconnected banner
        this.addEventListener('reconnect-requested', () => {
            this.reconnect();
        });

        // A stream's WebSocket closed — start a debounce timer.
        // We wait briefly before showing the disconnected banner so that
        // transient drops (phone switching apps for a moment) don't cause a flash.
        document.addEventListener('am-stream-closed', () => {
            if (this._disconnectTimer) return;  // already waiting
            this._disconnectTimer = setTimeout(() => {
                this._disconnectTimer = null;
                // Confirm the network is actually unreachable before showing banner
                this.checkAuth();
            }, 4000);
        });

        // A stream successfully reconnected — clear the banner and refresh data.
        document.addEventListener('am-stream-reconnected', () => {
            if (this._disconnectTimer) {
                clearTimeout(this._disconnectTimer);
                this._disconnectTimer = null;
            }
            if (this._disconnected) {
                this._setDisconnected(false);
                this.checkAuth();
                this.loadInstances();
            }
        });

        document.addEventListener('am-agent-idle', (e) => {
            this.notifyAgentIdle(e.detail?.title);
        });

        // A turn failed with an auth/gateway error — flip the banner to the
        // expired state immediately rather than waiting for the 30s poll, then
        // re-check to pick up the authoritative server-side flag.
        document.addEventListener('am-auth-error', (e) => {
            const banner = this.querySelector('am-auth-banner');
            banner.reauthReason = e.detail?.reason || 'expired';
            banner.needsReauth = true;
            this.checkAuth();
        });
    }

    async init() {
        this.initSidebarState();

        // Listen for browser back/forward
        window.addEventListener('popstate', () => this.restoreFromURL());

        // Reconnect immediately when the page becomes visible again
        // (handles the common mobile case of switching away and back)
        document.addEventListener('visibilitychange', () => {
            if (document.visibilityState === 'visible') {
                this._onBecameVisible();
            }
        });

        // Reconnect when the device comes back online after being offline
        window.addEventListener('online', () => {
            this._onBecameVisible();
        });

        await Promise.all([
            this.checkAuth(),
            this.loadInstances(),
        ]);

        // Restore state from URL after instances are loaded
        this.restoreFromURL();

        // Polling (reduced frequency)
        setInterval(() => this.loadInstances(), 30000);
        setInterval(() => this.checkAuth(), 30000);
    }

    _onBecameVisible() {
        // Cancel any pending disconnect debounce
        if (this._disconnectTimer) {
            clearTimeout(this._disconnectTimer);
            this._disconnectTimer = null;
        }
        this.reconnect();
    }

    // Reconnect all WebSocket streams and refresh state from the server.
    async reconnect() {
        streamManager.reconnectAll();
        await Promise.all([this.checkAuth(), this.loadInstances()]);
    }

    _setDisconnected(value) {
        this._disconnected = value;
        this.querySelector('am-auth-banner').disconnected = value;
    }

    notifyAgentIdle(title) {
        if (!title || !idleNotificationsEnabled(title)) return;
        const inst = this.instances.find(i => i.title === title);
        showIdleNotification(title, inst?.display_title || title);
    }

    async checkAuth() {
        try {
            // Collect every provider referenced by current instances plus the
            // built-ins, so we can show "authed" if ANY of them succeeded.
            const providers = new Set(['claude', 'codex']);
            for (const inst of this.instances) {
                providers.add(inst.provider || inst.instance_type);
            }
            const statuses = await api.checkAuthStatuses([...providers]);
            const authed = Object.values(statuses).some((status) => status?.authed);
            // For the reauth banner (single-provider concept), prefer Claude
            // if it's in the mix, otherwise take the first entry.
            const primary = statuses.claude || Object.values(statuses)[0] || {};
            // Got a real HTTP response — network is reachable
            if (this._disconnected) {
                this._setDisconnected(false);
            }
            const banner = this.querySelector('am-auth-banner');
            banner.authed = authed;
            banner.reauthReason = primary.reauth_reason;
            banner.needsReauth = Boolean(primary.needs_reauth);
        } catch (e) {
            if (e instanceof TypeError) {
                // fetch() throws TypeError on network failure (no response at all).
                // Show the disconnected banner rather than the misleading "not authed" one.
                this._setDisconnected(true);
            } else {
                console.error('Auth check failed', e);
            }
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

        // Update team panel (show for loop instances on conversation tab)
        this.updateTeamPanel();

        // Load active tab content
        this.onTabActivated(this.activeTab);

        // Update URL
        this.updateURL();
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

        // Hide team panel
        this.querySelector('am-team-panel').instance = null;

        // Update URL
        this.updateURL();
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

        // Update team panel (only visible on conversation tab for loop instances)
        this.updateTeamPanel();

        this.onTabActivated(name);

        // Update URL
        this.updateURL();
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

    // Update team panel visibility based on instance type and active tab
    updateTeamPanel() {
        const teamPanel = this.querySelector('am-team-panel');
        // Show team panel only for loop instances on conversation (terminal) tab
        if (this.currentInst?.instance_type === 'loop' && this.activeTab === 'terminal') {
            teamPanel.instance = this.currentInst;
        } else {
            teamPanel.instance = null;
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
        // Start collapsed on narrow screens (phones and tablets up to 1100px)
        if (window.innerWidth <= 1100) {
            this.closeSidebar();
        } else {
            // Ensure class state matches on wide screens
            this.classList.remove('sidebar-collapsed');
        }
    }

    // URL routing
    updateURL() {
        if (this._skipUrlUpdate) return;

        let path = '/';
        if (this.currentTitle) {
            const tabUrl = TAB_TO_URL[this.activeTab] || 'conversation';
            path = `/${encodeURIComponent(this.currentTitle)}/${tabUrl}`;
        }

        // Only push if path changed
        if (window.location.pathname !== path) {
            history.pushState({ title: this.currentTitle, tab: this.activeTab }, '', path);
        }
    }

    restoreFromURL() {
        const path = window.location.pathname;
        const parts = path.split('/').filter(Boolean);

        if (parts.length === 0) {
            // Root path - deselect if something is selected
            if (this.currentTitle) {
                this._skipUrlUpdate = true;
                this.deselectInstance();
                this._skipUrlUpdate = false;
            }
            return;
        }

        const title = decodeURIComponent(parts[0]);
        const tabUrl = parts[1] || 'conversation';
        const tab = URL_TO_TAB[tabUrl] || 'terminal';

        // Find the instance
        const inst = this.instances.find(i => i.title === title);
        if (!inst) {
            // Instance not found - go to root
            history.replaceState(null, '', '/');
            return;
        }

        // Restore state without updating URL
        this._skipUrlUpdate = true;

        if (this.currentTitle !== title) {
            this.selectInstance(inst);
        }

        if (this.activeTab !== tab) {
            this.setActiveTab(tab);
        }

        this._skipUrlUpdate = false;
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
