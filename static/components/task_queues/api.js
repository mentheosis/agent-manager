export async function queueRequest(title, path, body) {
  const response = await fetch(
    `/api/task-queues/${encodeURIComponent(title)}/${path}`,
    body === undefined
      ? {}
      : {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        },
  );
  const data = await response.json().catch(() => ({}));
  if (!response.ok)
    throw new Error(data.detail || `Queue request failed (${response.status})`);
  return data;
}
export function textElement(tag, text, cls) {
  const el = document.createElement(tag);
  el.textContent = text;
  if (cls) el.className = cls;
  return el;
}
