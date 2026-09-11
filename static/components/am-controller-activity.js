/** Shared safe, accessible controller-event presentation. */
export function createActivityRow(event, labels, filters = {}) {
  const type = Object.hasOwn(labels, event.event_type)
    ? event.event_type
    : "controller";
  const row = document.createElement("details");
  row.className = `loop-event loop-${type}`;
  row.dataset.eventType = type;
  row.hidden = filters[type] === false;
  row.open =
    type === "assistant_text" || type === "user_prompt" || type === "error";
  const header = document.createElement("summary");
  const actor = document.createElement("span");
  actor.className = "loop-actor";
  actor.textContent = `${event.actor || "Controller"}${event.target ? ` → ${event.target}` : ""}`;
  const label = document.createElement("span");
  label.className = "loop-event-type";
  label.textContent = labels[type];
  const time = document.createElement("time");
  time.className = "loop-time";
  if (event.ts) {
    time.dateTime = event.ts;
    time.textContent = new Date(event.ts).toLocaleTimeString();
  }
  const preview = document.createElement("span");
  preview.className = "loop-preview";
  preview.textContent = (event.text || "").replace(/\s+/g, " ").slice(0, 180);
  header.append(actor, label, time, preview);
  const text = document.createElement("pre");
  text.textContent = event.text || "";
  row.append(header, text);

  return row;
}
