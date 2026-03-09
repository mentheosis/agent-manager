const API = '/api/instances';

export async function api(path, opts = {}) {
  let res;
  try {
    res = await fetch(API + path, {
      headers: { 'Content-Type': 'application/json' },
      ...opts,
    });
  } catch (e) {
    throw new Error('Failed to fetch: server unreachable');
  }
  if (!res.ok) {
    const text = await res.text();
    throw new Error(text);
  }
  if (res.status === 200 || res.status === 201) {
    const ct = res.headers.get('content-type');
    if (ct && ct.includes('json')) return res.json();
  }
  return null;
}

export async function apiFetch(url, opts = {}) {
  let res;
  try {
    res = await fetch(url, {
      headers: { 'Content-Type': 'application/json' },
      ...opts,
    });
  } catch (e) {
    throw new Error('Failed to fetch: server unreachable');
  }
  if (!res.ok) {
    const text = await res.text();
    throw new Error(text);
  }
  if (res.status === 200 || res.status === 201) {
    const ct = res.headers.get('content-type');
    if (ct && ct.includes('json')) return res.json();
  }
  return null;
}
