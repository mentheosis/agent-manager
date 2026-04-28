/**
 * New instance dialog component.
 */

import * as api from '../lib/api.js';

class AmNewDialog extends HTMLElement {
    connectedCallback() {
        this.innerHTML = `
            <dialog id="new-dialog">
                <form method="dialog" id="new-form">
                    <h2>New instance</h2>
                    <label>
                        Name
                        <input name="name" required autocomplete="off" placeholder="My cool project">
                        <small class="hint">Used as the display label. A snake_case ID is generated for storage; duplicates auto-suffix with _2, _3, ...</small>
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
                        Additional allowed directories (one per line, optional)
                        <textarea name="add_dirs" rows="3" autocomplete="off" spellcheck="false" placeholder="/Users/you/wrk/other-project"></textarea>
                        <small class="hint">Paths the agent can read/write outside of its working directory. Each must also be mounted in docker-compose.local.yml.</small>
                    </label>
                    <menu>
                        <button type="submit" value="cancel" formnovalidate>Cancel</button>
                        <button type="submit" value="create" id="create-btn">Create</button>
                    </menu>
                </form>
            </dialog>
        `;

        this.setupEventListeners();
    }

    setupEventListeners() {
        const form = this.querySelector('#new-form');

        form.addEventListener('submit', async (e) => {
            const btn = e.submitter;
            if (!btn || btn.value !== 'create') return;

            e.preventDefault();

            const data = Object.fromEntries(new FormData(form));
            const addDirsRaw = data.add_dirs || '';
            delete data.add_dirs;

            const add_dirs = addDirsRaw
                .split(/\r?\n/)
                .map((l) => l.trim())
                .filter((l) => l.length > 0);

            try {
                const inst = await api.createInstance({ ...data, add_dirs });
                this.close();
                form.reset();

                this.dispatchEvent(new CustomEvent('instance-created', {
                    bubbles: true,
                    detail: { title: inst.title }
                }));
            } catch (err) {
                alert(`Create failed: ${err.message}`);
            }
        });
    }

    open() {
        this.querySelector('#new-dialog').showModal();
    }

    close() {
        this.querySelector('#new-dialog').close();
    }
}

customElements.define('am-new-dialog', AmNewDialog);
