/**
 * WebSocket stream manager for instance events.
 * Maintains one connection per instance, handles reconnection,
 * and dispatches events to subscribers.
 */

import { eventsWebSocketUrl } from './api.js';

const PRICE_PER_M_TOKENS = {
    'gpt-5.2-codex': [1.75, 0.175, 14.00],
    'gpt-5.1-codex-max': [1.25, 0.125, 10.00],
    'gpt-5.1-codex': [1.25, 0.125, 10.00],
    'gpt-5-codex': [1.25, 0.125, 10.00],
    'gpt-5.5': [5.00, 0.50, 30.00],
    'gpt-5.4-mini': [0.75, 0.075, 4.50],
    'gpt-5.4': [2.50, 0.25, 15.00],
    'gpt-5.2': [1.75, 0.175, 14.00],
    'gpt-5.1': [1.25, 0.125, 10.00],
    'gpt-5-mini': [0.25, 0.025, 2.00],
    'gpt-5-nano': [0.05, 0.005, 0.40],
    'gpt-5': [1.25, 0.125, 10.00],
};

class Stream {
    constructor(title) {
        this.title = title;
        this.ws = null;
        this.status = null;
        this.activeModel = null;
        this.evicting = false;
        this.totals = {
            cost: 0,
            input_tokens: 0,
            output_tokens: 0,
            cache_read: 0,
            cache_creation: 0,
            cost_estimated: false,
            turns: 0,
        };
        this._lastCodexUsage = null;
        this.listeners = new Set();
        this.eventHistory = [];  // Buffer events for replay to new subscribers
        this.historyComplete = false;  // True once the server's history_end sentinel arrives

        // Reconnection state
        this._everConnected = false;   // True after first successful WS open
        this._reconnecting = false;    // True while server is sending the delta on reconnect
        this._lastSeq = -1;            // seq of the last event received; sent as ?since_seq= on reconnect
        this._reconnectDelay = 1000;   // Current backoff delay (ms)
        this._reconnectTimer = null;
        this._connecting = false;      // Guard against concurrent connect() calls
    }

    connect() {
        // Guard against concurrent connection attempts (race condition on spotty networks)
        if (this.ws || this._connecting) return;
        this._connecting = true;
        const url = eventsWebSocketUrl(this.title, this._lastSeq);
        this.ws = new WebSocket(url);

        this.ws.onopen = () => {
            this._connecting = false;
            this._reconnectDelay = 1000;  // Reset backoff on successful connect

            if (this._everConnected) {
                // This is a reconnect.  The server only sends events with
                // seq > _lastSeq, so eventHistory is kept intact and we just
                // wait for the (possibly empty) delta + history_end.
                this._reconnecting = true;
                this.historyComplete = false;
            } else {
                this._everConnected = true;
            }
        };

        this.ws.onmessage = (ev) => {
            try {
                const event = JSON.parse(ev.data);
                this.handleEvent(event);
            } catch (err) {
                console.error('Bad event', err, ev.data);
            }
        };

        this.ws.onclose = () => {
            this.ws = null;
            this._connecting = false;
            if (!this.evicting) {
                this.emit({ type: 'connection', status: 'closed' });
                this._scheduleReconnect();
                // Notify the app so it can show a disconnected banner
                document.dispatchEvent(new CustomEvent('am-stream-closed'));
            }
        };

        this.ws.onerror = () => {
            if (!this.evicting) {
                this.emit({ type: 'connection', status: 'error' });
            }
        };
    }

    _scheduleReconnect() {
        if (this.evicting) return;
        clearTimeout(this._reconnectTimer);
        this._reconnectTimer = setTimeout(() => {
            this._reconnectTimer = null;
            if (!this.evicting && !this.ws) {
                this.connect();  // _lastSeq is already up-to-date
            }
        }, this._reconnectDelay);
        // Exponential backoff: 1 → 2 → 4 → 8 → 16 → 30s max
        this._reconnectDelay = Math.min(this._reconnectDelay * 2, 30_000);
    }

    // Immediately cancel any pending reconnect and open a fresh connection.
    reconnectNow() {
        if (this.evicting) return;
        // Already open and healthy — nothing to do
        if (this.ws && this.ws.readyState === WebSocket.OPEN) return;

        clearTimeout(this._reconnectTimer);
        this._reconnectTimer = null;
        this._reconnectDelay = 1000;

        if (this.ws) {
            try { this.ws.close(); } catch { /* ignore */ }
            this.ws = null;
        }
        this.connect();  // _lastSeq is already up-to-date
    }

    handleEvent(event) {
        // History sentinel — mark history complete.
        if (event.type === 'history_end') {
            this.historyComplete = true;
            if (this._reconnecting) {
                // History replay after a reconnect is complete.
                // Notify all live subscribers to re-render from the fresh eventHistory.
                this._reconnecting = false;
                const reconnectEvent = { type: 'connection', status: 'reconnected' };
                for (const cb of this.listeners) {
                    try { cb(reconnectEvent); } catch (err) { console.error('Stream listener error', err); }
                }
                document.dispatchEvent(new CustomEvent('am-stream-reconnected'));
            } else {
                // Normal initial load — forward to listeners as before.
                for (const cb of this.listeners) {
                    try { cb(event); } catch (err) { console.error('Stream listener error', err); }
                }
            }
            return;
        }

        // Track the highest seq seen so reconnects can resume from this point.
        // Use Math.max so _lastSeq only ever moves forward — guards against
        // out-of-order delivery or any future seq regression on the server.
        if (typeof event.seq === 'number') {
            this._lastSeq = Math.max(this._lastSeq, event.seq);
        }

        // Track status changes
        if (event.type === 'status') {
            this.status = event.status;
        }
        // Track active model from system_init. Providers may report an exact
        // model id or only a display-safe label for their configured default.
        if (event.type === 'system_init') {
            const model = event.data && (event.data.model || event.data.active_model_label);
            if (model) {
                this.activeModel = model;
            }
        }

        // Accumulate totals from result events
        if (event.type === 'result') {
            this.accumulateTotals(event);
        }

        this.emit(event);
    }

    accumulateTotals(event) {
        if (event.is_error) return;
        const usage = event.usage || event.data?.usage;
        let usageForTotals = null;
        if (usage) {
            usageForTotals = this.usageForTotals(usage);
            const cachedInput = usageForTotals.cached_input_tokens || usageForTotals.cache_read_input_tokens || usageForTotals.cache_read || 0;
            const reasoningOutput = usageForTotals.reasoning_output_tokens || 0;
            this.totals.input_tokens += usageForTotals.input_tokens || 0;
            this.totals.output_tokens += (usageForTotals.output_tokens || 0) + reasoningOutput;
            this.totals.cache_read += cachedInput;
            this.totals.cache_creation += usageForTotals.cache_creation_input_tokens || usageForTotals.cache_creation || 0;
        }
        if (typeof event.total_cost_usd === 'number') {
            this.totals.cost += event.total_cost_usd;
        } else if (usage) {
            const estimate = this.estimateCostForUsage(event.estimated_cost_model || this.activeModel, usageForTotals);
            if (typeof estimate === 'number') {
                this.totals.cost += estimate;
                this.totals.cost_estimated = true;
            }
        } else if (typeof event.estimated_cost_usd === 'number') {
            this.totals.cost += event.estimated_cost_usd;
            this.totals.cost_estimated = true;
        }
        this.totals.turns += 1;
    }

    usageForTotals(usage, { peek = false } = {}) {
        if (!this.isCodexCumulativeUsage(usage)) return usage;
        const previous = this._lastCodexUsage;
        const current = this.codexUsageSnapshot(usage);
        if (!peek) this._lastCodexUsage = current;
        if (!previous) return current;

        const delta = {};
        for (const key of Object.keys(current)) {
            delta[key] = Math.max(current[key] - (previous[key] || 0), 0);
        }
        return delta;
    }

    isCodexCumulativeUsage(usage) {
        return usage && (
            Object.hasOwn(usage, 'cached_input_tokens')
            || Object.hasOwn(usage, 'reasoning_output_tokens')
        );
    }

    codexUsageSnapshot(usage) {
        return {
            input_tokens: usage.input_tokens || 0,
            cached_input_tokens: usage.cached_input_tokens || 0,
            output_tokens: usage.output_tokens || 0,
            reasoning_output_tokens: usage.reasoning_output_tokens || 0,
        };
    }

    estimateCostForUsage(model, usage) {
        const rates = this.ratesForModel(model);
        if (!rates || !usage) return null;
        const [inputRate, cachedInputRate, outputRate] = rates;
        const cachedInput = usage.cached_input_tokens || usage.cache_read_input_tokens || usage.cache_read || 0;
        const input = Math.max((usage.input_tokens || 0) - cachedInput, 0);
        const output = (usage.output_tokens || 0) + (usage.reasoning_output_tokens || 0);
        return ((input * inputRate) + (cachedInput * cachedInputRate) + (output * outputRate)) / 1_000_000;
    }

    ratesForModel(model) {
        if (!model) return null;
        const normalized = model.trim().toLowerCase();
        const match = Object.entries(PRICE_PER_M_TOKENS)
            .sort((a, b) => b[0].length - a[0].length)
            .find(([key]) => normalized === key || normalized.startsWith(`${key}-`));
        return match?.[1] || null;
    }

    subscribe(callback, { replay = true } = {}) {
        this.listeners.add(callback);

        // Replay buffered events to new subscriber
        if (replay && this.eventHistory.length > 0) {
            for (const event of this.eventHistory) {
                try {
                    callback(event);
                } catch (err) {
                    console.error('Stream replay error', err);
                }
            }
        }

        return () => this.listeners.delete(callback);
    }

    emit(event) {
        // Buffer event for replay (skip connection events)
        if (event.type !== 'connection') {
            this.eventHistory.push(event);
        }

        // During a reconnect history replay, don't forward to live subscribers —
        // they'll get a single 'reconnected' event when the replay is complete.
        if (this._reconnecting) return;

        for (const cb of this.listeners) {
            try {
                cb(event);
            } catch (err) {
                console.error('Stream listener error', err);
            }
        }
    }

    close() {
        this.evicting = true;
        clearTimeout(this._reconnectTimer);
        this._reconnectTimer = null;
        if (this.ws) {
            try { this.ws.close(); } catch {}
            this.ws = null;
        }
        this.listeners.clear();
        this.eventHistory = [];
    }
}

class StreamManager {
    constructor() {
        this.streams = new Map();
    }

    get(title) {
        let stream = this.streams.get(title);
        if (!stream) {
            stream = new Stream(title);
            this.streams.set(title, stream);
            stream.connect();
        }
        return stream;
    }

    has(title) {
        return this.streams.has(title);
    }

    delete(title) {
        const stream = this.streams.get(title);
        if (stream) {
            stream.close();
            this.streams.delete(title);
        }
    }

    // Reconnect all active streams immediately (e.g. when app regains focus).
    reconnectAll() {
        for (const stream of this.streams.values()) {
            stream.reconnectNow();
        }
    }

    // Sync streams with instance list - close streams for deleted instances
    sync(titles) {
        const titleSet = new Set(titles);
        for (const [title, stream] of this.streams) {
            if (!titleSet.has(title)) {
                stream.close();
                this.streams.delete(title);
            }
        }
    }
}

// Singleton instance
export const streamManager = new StreamManager();
