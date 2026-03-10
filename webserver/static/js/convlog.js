import { safeAnsiToHtml, esc } from './ansi.js';
import { api } from './api.js';

// ConvLogView manages the terminal output display for a single instance.
// It owns the WS connection and renders stable history + volatile pane.
export class ConvLogView {
  constructor(container) {
    this.container = container;
    this.historyDiv = null;
    this.paneDiv = null;
    this.ws = null;
    this.generation = 0;
    this.stableCount = 0; // number of stable lines rendered
    this.onStatus = null; // callback(title, status) for sidebar updates
    this.onContent = null; // callback(content) for mode detection etc.
  }

  // Connect to an instance: load initial history + start WS stream
  async connect(title) {
    this.disconnect();
    this.generation++;
    const gen = this.generation;

    // Set up DOM structure
    this.container.innerHTML = '';
    this.container.className = '';
    this.historyDiv = document.createElement('div');
    this.historyDiv.id = 'output-history';
    this.paneDiv = document.createElement('div');
    this.paneDiv.id = 'output-live';
    this.container.appendChild(this.historyDiv);
    this.container.appendChild(this.paneDiv);
    this.stableCount = 0;

    // Fetch initial state from server
    let initialState = null;
    try {
      initialState = await api(`/${encodeURIComponent(title)}/history`);
      if (gen !== this.generation) return;
    } catch (e) {
      if (gen !== this.generation) return;
      if (e.message && e.message.includes('Failed to fetch')) {
        this._showError('Cannot connect to Claude Squad server. Is it running?', title);
        return;
      }
    }

    if (gen !== this.generation) return;

    // Render initial stable lines
    if (initialState && initialState.stable_lines && initialState.stable_lines.length > 0) {
      this.historyDiv.innerHTML = safeAnsiToHtml(initialState.stable_lines.join('\n'));
      this.stableCount = initialState.stable_lines.length;
    }

    // Render initial pane
    if (initialState && initialState.pane && initialState.pane.length > 0) {
      this.paneDiv.innerHTML = safeAnsiToHtml(initialState.pane.join('\n'));
    }

    // Scroll to bottom
    this.container.scrollTop = this.container.scrollHeight;

    // Open WebSocket
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    let newWs;
    try {
      newWs = new WebSocket(`${proto}//${location.host}/api/instances/${encodeURIComponent(title)}/ws`);
    } catch (e) {
      if (gen !== this.generation) return;
      this._showError('Cannot connect to Claude Squad server. Is it running?', title);
      return;
    }

    if (gen !== this.generation) {
      newWs.close();
      return;
    }
    this.ws = newWs;

    newWs.onmessage = (e) => {
      if (gen !== this.generation) { newWs.close(); return; }
      const msg = JSON.parse(e.data);
      this._handleMessage(msg, gen, title);
    };

    newWs.onerror = () => {
      if (gen !== this.generation) return;
      this._showError('Connection to server lost. The server may have stopped.', title);
    };

    newWs.onclose = () => {
      if (gen === this.generation) this.ws = null;
    };
  }

  disconnect() {
    if (this.ws) { this.ws.close(); this.ws = null; }
    this.historyDiv = null;
    this.paneDiv = null;
    this.stableCount = 0;
  }

  _handleMessage(msg, gen, title) {
    if (gen !== this.generation) return;

    const wasAtBottom = this.container.scrollHeight - this.container.scrollTop - this.container.clientHeight < 30;

    if (msg.type === 'history_append') {
      // Append new stable lines to historyDiv using DOM insertion (not innerHTML +=)
      if (msg.lines && msg.lines.length > 0 && this.historyDiv) {
        const newHtml = safeAnsiToHtml(msg.lines.join('\n'));
        const fragment = document.createElement('span');
        fragment.innerHTML = newHtml;

        if (this.stableCount > 0) {
          // Add a newline text node before appending new content
          this.historyDiv.appendChild(document.createTextNode('\n'));
        }
        // Move all child nodes from fragment into historyDiv
        while (fragment.firstChild) {
          this.historyDiv.appendChild(fragment.firstChild);
        }
        this.stableCount += msg.lines.length;
      }
    } else if (msg.type === 'pane') {
      // Replace volatile pane content
      if (this.paneDiv) {
        const newHtml = safeAnsiToHtml(msg.content);
        this.paneDiv.innerHTML = (this.stableCount > 0 ? '\n' : '') + newHtml;
      }

      // Notify about status changes
      if (msg.status && this.onStatus) {
        this.onStatus(title, msg.status);
      }

      // Notify about content for mode detection
      if (msg.content && this.onContent) {
        this.onContent(msg.content);
      }
    }

    if (wasAtBottom) this.container.scrollTop = this.container.scrollHeight;
  }

  _showError(message, title) {
    this.container.innerHTML = `<div class="connection-error">
      <div class="error-icon">&#x26A0;</div>
      <div class="error-msg">${esc(message)}</div>
      <button class="retry-btn" onclick="window._convLogRetry && window._convLogRetry()">Retry</button>
    </div>`;
    window._convLogRetry = () => this.connect(title);
  }
}
