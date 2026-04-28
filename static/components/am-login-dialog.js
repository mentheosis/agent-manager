/**
 * Login dialog component - authenticates Claude Code in the container.
 */

import * as api from '../lib/api.js';

class AmLoginDialog extends HTMLElement {
    constructor() {
        super();
        this.sessionId = null;
        this.ws = null;
    }

    connectedCallback() {
        this.innerHTML = `
            <dialog id="login-dialog">
                <h2>Authenticate Claude Code</h2>
                <p class="hint">
                    Running <code>claude login</code> inside the container. On first run you'll see
                    the setup wizard (theme picker, folder trust) - use Enter / arrow keys below to
                    drive it. Once the authorization URL appears, open it in your browser, copy the
                    code Anthropic shows, and paste it below.
                </p>
                <pre id="login-output" aria-live="polite"></pre>
                <div id="login-controls">
                    <div class="key-row">
                        <button type="button" class="key-btn" data-key="[A">&#x2191;</button>
                        <button type="button" class="key-btn" data-key="[B">&#x2193;</button>
                        <button type="button" class="key-btn" data-key="[D">&#x2190;</button>
                        <button type="button" class="key-btn" data-key="[C">&#x2192;</button>
                        <button type="button" class="key-btn key-enter" data-key="\\r">Enter</button>
                    </div>
                    <form id="login-form" method="dialog">
                        <input
                            name="text"
                            id="login-text"
                            placeholder="Type text or paste code, then Send (Enter key = submit)"
                            autocomplete="off"
                            spellcheck="false">
                        <button type="submit" id="login-submit">Send</button>
                    </form>
                </div>
                <menu id="login-actions">
                    <button type="button" id="login-cancel">Cancel</button>
                </menu>
            </dialog>
        `;

        this.setupEventListeners();
    }

    setupEventListeners() {
        const dialog = this.querySelector('#login-dialog');
        const form = this.querySelector('#login-form');
        const cancelBtn = this.querySelector('#login-cancel');
        const keyBtns = this.querySelectorAll('.key-btn');

        form.addEventListener('submit', (e) => {
            e.preventDefault();
            this.submitText();
        });

        cancelBtn.addEventListener('click', () => {
            this.cancel();
        });

        for (const btn of keyBtns) {
            btn.addEventListener('click', () => {
                this.sendInput(this.decodeKeyAttr(btn.dataset.key));
            });
        }

        // Close on dialog close
        dialog.addEventListener('close', () => {
            this.cleanup();
        });
    }

    decodeKeyAttr(attr) {
        if (attr === '\\r' || attr === '\r') return '\r';
        if (attr.startsWith('[') || attr.startsWith('O')) return '\x1b' + attr;
        return attr;
    }

    async open() {
        const output = this.querySelector('#login-output');
        const input = this.querySelector('#login-text');

        output.textContent = '';
        input.value = '';

        this.querySelector('#login-dialog').showModal();
        input.focus();

        try {
            const { id } = await api.startLogin();
            this.sessionId = id;

            this.ws = new WebSocket(api.loginWebSocketUrl(id));

            this.ws.onmessage = (ev) => {
                try {
                    const msg = JSON.parse(ev.data);
                    if (msg.type === 'output') {
                        this.appendOutput(msg.text);
                    } else if (msg.type === 'done') {
                        this.appendOutput(`\n[login process exited, code ${msg.returncode}]\n`);
                        this.ws = null;
                        this.sessionId = null;

                        setTimeout(() => {
                            this.dispatchEvent(new CustomEvent('auth-changed', { bubbles: true }));
                            this.close();
                        }, 200);
                    }
                } catch (err) {
                    console.error('Bad login event', err, ev.data);
                }
            };

            this.ws.onclose = () => {
                this.ws = null;
            };
        } catch (err) {
            this.appendOutput(`\n[error] failed to start login: ${err.message}\n`);
        }
    }

    close() {
        this.querySelector('#login-dialog').close();
    }

    async cancel() {
        if (this.sessionId) {
            try {
                await api.cancelLogin(this.sessionId);
            } catch {
                // Ignore
            }
            this.sessionId = null;
        }
        this.cleanup();
        this.close();
    }

    cleanup() {
        if (this.ws) {
            try {
                this.ws.close();
            } catch {
                // Ignore
            }
            this.ws = null;
        }
    }

    appendOutput(text) {
        // Strip ANSI codes
        const clean = text.replace(/\x1b\[[0-9;?]*[A-Za-z]/g, '');
        const output = this.querySelector('#login-output');

        const frag = document.createDocumentFragment();
        let idx = 0;
        const re = /(https?:\/\/[^\s]+)/g;
        let m;

        while ((m = re.exec(clean)) !== null) {
            if (m.index > idx) {
                frag.appendChild(document.createTextNode(clean.slice(idx, m.index)));
            }
            const a = document.createElement('a');
            a.href = m[1];
            a.target = '_blank';
            a.rel = 'noopener';
            a.textContent = m[1];
            frag.appendChild(a);
            idx = m.index + m[1].length;
        }

        if (idx < clean.length) {
            frag.appendChild(document.createTextNode(clean.slice(idx)));
        }

        output.appendChild(frag);
        output.scrollTop = output.scrollHeight;
    }

    async sendInput(data) {
        if (!this.sessionId) {
            this.appendOutput('\n[no active login session]\n');
            return;
        }
        if (!data) return;

        try {
            await api.sendLoginInput(this.sessionId, data);
        } catch (err) {
            this.appendOutput(`\n[error] send input failed: ${err.message}\n`);
        }
    }

    async submitText() {
        const input = this.querySelector('#login-text');
        const text = input.value;
        input.value = '';
        await this.sendInput(text + '\r');
    }
}

customElements.define('am-login-dialog', AmLoginDialog);
