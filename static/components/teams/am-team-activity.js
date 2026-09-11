/** Team conversation: controller activity and messages from team members. */
import { streamManager } from '../../lib/streams.js';
import { createActivityRow } from '../am-controller-activity.js';

class AmLoopPane extends HTMLElement {
    constructor() {
        super();
        this._instance = null;
        this._unsubscribe = null;
        this._poll = null;
        this._seen = new Set();
        this._filters = {};
        this._onFilterChanged = event => {
            if (event.detail.scope !== 'team') return;
            this._filters = {...event.detail.filters};
            for (const row of this.querySelectorAll('.loop-event')) {
                row.hidden = this._filters[row.dataset.eventType] === false;
            }
        };
    }

    connectedCallback() {
        this.classList.add('controller-activity');
        document.addEventListener('filter-changed', this._onFilterChanged);
        this.innerHTML = `

            <div class="loop-header"><h2>Team activity</h2><span class="loop-status">Controller stopped</span></div>
            <div class="loop-events" role="log" aria-live="polite"><p class="loop-empty">Controller events and agent messages will appear here when the team runs.</p></div>
        `;
    }

    set instance(inst) {
        if (this._instance?.title === inst?.title) return;
        this.cleanup();
        this._instance = inst;
        this._seen.clear();
        const output = this.querySelector('.loop-events');
        output.replaceChildren();
        const empty = document.createElement('p');
        empty.className = 'loop-empty';
        empty.textContent = 'Controller events and agent messages will appear here when the team runs.';
        output.appendChild(empty);
        if (!inst) return;
        this.querySelector('.loop-status').textContent = 'Checking controller…';
        this._unsubscribe = streamManager.get(inst.title).subscribe(event => this.renderEvent(event));
        this.loadStatus();
        this._poll = setInterval(() => this.loadStatus(), 3000);
    }

    cleanup() {
        this._unsubscribe?.();
        this._unsubscribe = null;
        clearInterval(this._poll);
        this._poll = null;
    }

    disconnectedCallback() {
        this.cleanup();
        document.removeEventListener('filter-changed', this._onFilterChanged);
    }

    async loadStatus() {
        const title = this._instance?.title;
        if (!title) return;
        try {
            const response = await fetch(`/api/instances/${encodeURIComponent(title)}/orchestrator/status`);
            if (!response.ok) throw new Error('Status unavailable');
            const status = await response.json();
            if (this._instance?.title !== title) return;
            this.querySelector('.loop-status').textContent = `Controller ${status.state || (status.running ? 'running' : 'stopped')}`;
        } catch {
            if (this._instance?.title === title) this.querySelector('.loop-status').textContent = 'Controller status unavailable';
        }
    }

    renderEvent(event) {
        if (event.type === 'connection' && event.status === 'reconnected' && this._instance) {
            for (const item of streamManager.get(this._instance.title).eventHistory) this.renderEvent(item);
            return;
        }
        // Ignore provider history left behind by old versions of team parents.
        if (event.type !== 'team_event') return;
        if (event.seq != null) {
            if (this._seen.has(event.seq)) return;
            this._seen.add(event.seq);
        }
        const output = this.querySelector('.loop-events');
        const follow = output.scrollHeight - output.scrollTop - output.clientHeight < 100;
        output.querySelector('.loop-empty')?.remove();
        const labels = {user_prompt: 'Task received', assistant_text: 'Message', result: 'Turn result', tool_use: 'Coordination', status: 'Status', controller: 'Controller event', error: 'Error'};
        const row = createActivityRow(event, labels, this._filters);
        output.appendChild(row);
        if (follow) output.scrollTop = output.scrollHeight;
    }
}
customElements.define('am-loop-pane', AmLoopPane);
