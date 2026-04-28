/**
 * Tab bar component.
 */

class AmTabs extends HTMLElement {
    constructor() {
        super();
        this._activeTab = 'terminal';
        this.tabs = ['terminal', 'diff', 'settings', 'plans', 'memory'];
        this.labels = {
            terminal: 'Conversation',
            diff: 'Diff',
            settings: 'Settings',
            plans: 'Plans',
            memory: 'Memory',
        };
    }

    connectedCallback() {
        this.id = 'tabs';
        this.render();
    }

    get activeTab() {
        return this._activeTab;
    }

    set activeTab(value) {
        this._activeTab = value;
        this.updateActiveState();
    }

    render() {
        this.innerHTML = this.tabs.map(tab => `
            <div class="tab ${tab === this._activeTab ? 'active' : ''}" data-tab="${tab}">
                ${this.labels[tab]}
            </div>
        `).join('');

        // Click handlers
        for (const tabEl of this.querySelectorAll('.tab')) {
            tabEl.addEventListener('click', () => {
                const tab = tabEl.dataset.tab;
                this.dispatchEvent(new CustomEvent('tab-changed', {
                    bubbles: true,
                    detail: { tab }
                }));
            });
        }
    }

    updateActiveState() {
        for (const tabEl of this.querySelectorAll('.tab')) {
            tabEl.classList.toggle('active', tabEl.dataset.tab === this._activeTab);
        }
    }
}

customElements.define('am-tabs', AmTabs);
