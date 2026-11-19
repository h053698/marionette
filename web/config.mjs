// Backend location. Empty (default) means same origin as this page.
// The frontend server injects its backend origin in config.js. Static hosts can
// edit config.js, or keep using the index.html meta setting.
const configured = globalThis.MARIONETTE_API_URL || globalThis.document?.querySelector('meta[name="marionette-api"]')?.content || '';

export const apiBase = configured.trim().replace(/\/+$/, '');

export const apiUrl = (path) => (path.startsWith('/') ? apiBase + path : path);

export const wsUrl = (path, page = globalThis.location) => {
  const { protocol, host } = apiBase ? new URL(apiBase) : page;
  return `${protocol === 'https:' ? 'wss:' : 'ws:'}//${host}${path}`;
};

export const fetchCredentials = apiBase ? 'include' : 'same-origin';
