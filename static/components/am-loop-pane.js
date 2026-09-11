/** Team conversation: controller activity and messages from team members. */
import { streamManager } from '../lib/streams.js';

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
        document.addEventListener('filter-changed', this._onFilterChanged);
        this.innerHTML = `
            <style>
                am-loop-pane { min-height: 0; flex-direction: column; font-size: var(--fs-md); line-height: 1.5; }
                am-loop-pane .loop-header { padding: 16px; border-bottom: 1px solid var(--border); }
                am-loop-pane .loop-header h2 { margin: 0 0 8px; font-size: 16px; }
                am-loop-pane .loop-status, am-loop-pane .loop-empty { color: var(--text-dim); }
                am-loop-pane .loop-events { flex: 1; overflow-y: auto; padding: 16px; }
                am-loop-pane .loop-event { --event-color: var(--text-dim); margin-bottom: 10px; border: 1px solid var(--border); border-left: 3px solid var(--event-color); border-radius: 6px; }
                am-loop-pane .loop-user_prompt { --event-color: var(--accent); }
                am-loop-pane .loop-assistant_text, am-loop-pane .loop-result { --event-color: var(--green); }
                am-loop-pane .loop-tool_use { --event-color: var(--purple); }
                am-loop-pane .loop-status { --event-color: var(--yellow); }
                am-loop-pane .loop-error { --event-color: var(--red); }
                am-loop-pane .loop-event summary { display: flex; align-items: baseline; flex-wrap: wrap; gap: 10px; font-size: var(--fs-sm); padding: 10px 12px; cursor: pointer; list-style: none; }
                am-loop-pane .loop-event summary::-webkit-details-marker { display: none; }
                am-loop-pane .loop-event summary::before { content: '▸'; color: var(--event-color); }
                am-loop-pane .loop-event[open] summary::before { content: '▾'; }
                am-loop-pane .loop-event summary:focus-visible { outline: 2px solid var(--accent); outline-offset: -2px; }
                am-loop-pane .loop-actor { font-weight: 600; overflow-wrap: anywhere; }
                am-loop-pane .loop-event-type { color: var(--event-color); font-size: var(--fs-sm); font-weight: 600; }
                am-loop-pane .loop-time { color: #8a92b3; font-size: var(--fs-sm); font-weight: normal; font-variant-numeric: tabular-nums; }
                am-loop-pane .loop-preview { flex-basis: 100%; margin-left: 18px; color: var(--text-dim); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; font-size: var(--fs-sm); }
                am-loop-pane .loop-event[open] .loop-preview { display: none; }
                am-loop-pane .loop-event pre { white-space: pre-wrap; overflow-wrap: anywhere; margin: 0; padding: 0 12px 12px 30px; font-family: inherit; font-size: var(--fs-md); line-height: 1.5; color: var(--text); }
            </style>
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
        const type = Object.hasOwn(labels, event.event_type) ? event.event_type : 'controller';
        const row = document.createElement('details');
        row.className = `loop-event loop-${type}`;
        row.dataset.eventType = type;
        row.hidden = this._filters[type] === false;
        row.open = type === 'assistant_text' || type === 'user_prompt' || type === 'error';
        const header = document.createElement('summary');
        const actor = document.createElement('span');
        actor.className = 'loop-actor';
        actor.textContent = `${event.actor || 'Controller'}${event.target ? ` → ${event.target}` : ''}`;
        const label = document.createElement('span');
        label.className = 'loop-event-type';
        label.textContent = labels[type];
        const time = document.createElement('time');
        time.className = 'loop-time';
        if (event.ts) {
            time.dateTime = event.ts;
            time.textContent = new Date(event.ts).toLocaleTimeString();
        }
        const preview = document.createElement('span');
        preview.className = 'loop-preview';
        preview.textContent = (event.text || '').replace(/\s+/g, ' ').slice(0, 180);
        header.append(actor, label, time, preview);
        const text = document.createElement('pre');
        text.textContent = event.text || '';
        row.append(header, text);
        output.appendChild(row);
        if (follow) output.scrollTop = output.scrollHeight;
    }
}
customElements.define('am-loop-pane', AmLoopPane);
