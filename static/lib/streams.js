/**
 * WebSocket stream manager for instance events.
 * Maintains one connection per instance, handles reconnection,
 * and dispatches events to subscribers.
 */

import { eventsWebSocketUrl } from './api.js';

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
            turns: 0,
        };
        this.listeners = new Set();
        this.eventHistory = [];  // Buffer events for replay to new subscribers
    }

    connect() {
        if (this.ws) return;
        const url = eventsWebSocketUrl(this.title);
        this.ws = new WebSocket(url);

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
            if (!this.evicting) {
                this.emit({ type: 'connection', status: 'closed' });
            }
        };

        this.ws.onerror = () => {
            if (!this.evicting) {
                this.emit({ type: 'connection', status: 'error' });
            }
        };
    }

    handleEvent(event) {
        // Track status changes
        if (event.type === 'status') {
            this.status = event.status;
        }

        // Track active model from system_init
        if (event.type === 'system_init') {
            const model = event.data && event.data.model;
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
        if (usage) {
            this.totals.input_tokens += usage.input_tokens || 0;
            this.totals.output_tokens += usage.output_tokens || 0;
            this.totals.cache_read += usage.cache_read_input_tokens || usage.cache_read || 0;
            this.totals.cache_creation += usage.cache_creation_input_tokens || usage.cache_creation || 0;
        }
        if (typeof event.total_cost_usd === 'number') {
            this.totals.cost += event.total_cost_usd;
        }
        this.totals.turns += 1;
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
