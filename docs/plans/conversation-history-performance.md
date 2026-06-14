# Conversation History Performance Plan

## Goal

Make large Agent Manager conversations fast to switch into and safe to keep open, without losing full history on disk.

The main issue is not just event count. A small number of very large event bodies, such as base64 image payloads, long command logs, large diffs, or verbose tool results, can make browser replay slow and memory-heavy.

## Current Risks

- [ ] Initial WebSocket replay can send too many events on conversation switch.
- [ ] `stream.eventHistory` can retain large payloads in browser memory.
- [ ] Large `tool_result.output` values can dominate network transfer and DOM render time.
- [ ] Base64 image payloads from multimodal/tool events can be rendered or retained as text.
- [ ] Old turns are fully rendered into the DOM even when the user only needs recent context.
- [ ] Existing history files remain append-only and can grow indefinitely.

## Phase 1: Low-Risk Payload Reduction

- [ ] Cap rendered `tool_result.output` size before sending it to the browser.
- [ ] Preserve metadata for truncated results:
  - [ ] `truncated: true`
  - [ ] `original_bytes`
  - [ ] `shown_bytes`
- [ ] Show a compact placeholder for omitted content.
- [ ] Replace inline base64 image/data URLs with `[image payload omitted]`.
- [ ] Add tests for truncating large command output, base64 image payloads, and large JSON/tool responses.

Notes:

This should be the first implementation target. It reduces network cost, browser memory, and DOM render work without changing the core event storage model.

## Phase 2: Smaller Initial History Replay

- [ ] Change initial conversation replay to send only the latest window by default.
- [ ] Choose an initial default:
  - [ ] Last `300-500` events.
  - [ ] Or last `20-50` turns.
- [ ] Keep full JSONL history on disk.
- [ ] Add `has_more` metadata to the history replay.
- [ ] Add a UI affordance to load older events.
- [ ] Keep reconnect behavior based on `seq > since_seq` unchanged for live streams.

Notes:

This addresses slow conversation switching directly. The user sees recent work quickly, while older history remains available.

## Phase 3: Browser Memory Bound

- [ ] Bound `stream.eventHistory` in the browser.
- [ ] Keep only the latest N rendered events or turns in memory.
- [ ] Preserve enough sequence metadata for reconnects.
- [ ] Avoid retaining full text for heavy events after they have been rendered in truncated form.
- [ ] Verify switching between conversations does not accumulate old DOM or event arrays.

Notes:

This prevents long-lived browser sessions from getting progressively slower.

## Phase 4: On-Demand Full Payload Fetch

- [ ] Persist full event bodies on disk as today.
- [ ] Send only summarized/truncated bodies in normal history replay.
- [ ] Add an endpoint to fetch a full event body by instance title and event sequence.
- [ ] Add a `show full output` control for truncated events.
- [ ] Enforce size limits and secret filtering before returning full payloads.

Notes:

This preserves debuggability without making every conversation switch pay the cost of huge outputs.

## Phase 5: Turn-Level Virtualization

- [ ] Render only visible turns plus a small buffer.
- [ ] Unmount offscreen turns while preserving scroll position.
- [ ] Keep artifact/video sizing stable so virtualization does not cause jumpy scroll behavior.
- [ ] Test large histories with thousands of turns and heavy artifacts.

Notes:

This is the highest-complexity UI change. It should wait until payload trimming and smaller replay are proven insufficient.

## Open Decisions

- [ ] Should initial replay be event-count based or turn-count based?
- [ ] What should the default output cap be: `16 KB`, `32 KB`, `64 KB`, or provider-specific?
- [ ] Should truncation happen when writing history, when loading history, or only when sending to browser?
- [ ] Should full raw event fetch require an explicit button to avoid accidental huge downloads?
- [ ] Should old persisted histories be compacted/migrated, or only handled at read time?

## Recommended First Cut

- [ ] Implement read-time payload elision for browser delivery.
- [ ] Cap large text outputs at `32 KB` with truncation metadata.
- [ ] Omit base64 data URLs from rendered event bodies.
- [ ] Reduce initial conversation replay to the latest `500` events.
- [ ] Add a simple `load older` control later if the reduced replay works well.

