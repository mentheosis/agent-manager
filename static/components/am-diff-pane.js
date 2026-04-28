/**
 * Diff pane component - git status and diff display.
 */

import * as api from '../lib/api.js';

class AmDiffPane extends HTMLElement {
    constructor() {
        super();
        this._title = null;
    }

    connectedCallback() {
        this.innerHTML = `
            <div class="pane-toolbar">
                <span class="pane-label">git status + diff</span>
                <button type="button" class="pane-refresh-btn">Refresh</button>
            </div>
            <pre class="status-content"></pre>
            <pre class="diff-content" data-empty="No diff."></pre>
        `;

        this.querySelector('.pane-refresh-btn').addEventListener('click', () => {
            if (this._title) this.load(this._title, true);
        });
    }

    async load(title, force = false) {
        if (!force && title === this._title) return;
        this._title = title;

        const statusEl = this.querySelector('.status-content');
        const diffEl = this.querySelector('.diff-content');

        statusEl.textContent = '';
        diffEl.textContent = 'loading…';
        diffEl.className = 'diff-content';

        try {
            const [diffData, statusData] = await Promise.all([
                api.fetchDiff(title),
                api.fetchGitStatus(title),
            ]);

            // Render status
            this.renderStatus(statusEl, statusData);

            // Render diff
            const { content, error, returncode } = diffData;
            if (returncode !== 0 && !content) {
                diffEl.textContent = error || `git diff exited ${returncode}`;
                return;
            }
            this.renderDiff(diffEl, content);
        } catch (e) {
            diffEl.textContent = `error: ${e.message}`;
        }
    }

    renderStatus(el, { is_git, branch, status }) {
        el.textContent = '';
        if (!is_git) return;

        const frag = document.createDocumentFragment();

        // Branch line
        const branchSpan = document.createElement('span');
        branchSpan.className = 'branch';
        branchSpan.textContent = `On branch ${branch}\n`;
        frag.appendChild(branchSpan);

        // Status lines
        if (!status || !status.trim()) {
            const clean = document.createElement('span');
            clean.className = 'st-clean';
            clean.textContent = 'nothing to commit, working tree clean';
            frag.appendChild(clean);
        } else {
            for (const line of status.split('\n')) {
                if (!line) continue;
                const span = document.createElement('span');
                const code = line[0] !== ' ' ? line[0] : line[1];
                if (code === 'M') span.className = 'st-M';
                else if (code === 'A') span.className = 'st-A';
                else if (code === 'D') span.className = 'st-D';
                else if (code === 'R') span.className = 'st-R';
                else if (code === '?') span.className = 'st-Q';
                span.textContent = line + '\n';
                frag.appendChild(span);
            }
        }

        el.appendChild(frag);
    }

    renderDiff(el, content) {
        el.textContent = '';
        if (!content) return;

        const frag = document.createDocumentFragment();
        for (const line of content.split('\n')) {
            const span = document.createElement('span');
            if (line.startsWith('@@')) span.className = 'hunk';
            else if (line.startsWith('+++') || line.startsWith('---') || line.startsWith('diff ') || line.startsWith('index ')) span.className = 'meta';
            else if (line.startsWith('+')) span.className = 'add';
            else if (line.startsWith('-')) span.className = 'del';
            span.textContent = line + '\n';
            frag.appendChild(span);
        }
        el.appendChild(frag);
    }
}

customElements.define('am-diff-pane', AmDiffPane);
