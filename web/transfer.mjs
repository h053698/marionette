// Lease-scoped HTTP helpers for file transfer and package install. The lease
// token and CSRF token travel in headers only, never in URLs.
export function createDeviceApi({ apiUrl, credentials, target }) {
  function resolve(operation, query) {
    const current = target();
    if (!current) throw new Error('먼저 기기를 점유해 주세요.');
    const params = query ? `?${new URLSearchParams(query)}` : '';
    const headers = { 'X-Lease-Token': current.token };
    if (current.csrf) headers['X-CSRF-Token'] = current.csrf;
    return { url: apiUrl(`${current.base}/${operation}${params}`), headers, key: current.key };
  }
  async function fail(response) {
    const result = await response.json().catch(() => ({}));
    const error = new Error(result.error || `요청에 실패했어요 (${response.status}).`);
    error.status = response.status;
    return error;
  }
  async function request(operation, { method = 'GET', query, body, signal, timeout = 30000 } = {}) {
    const { url, headers } = resolve(operation, query);
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    const response = await fetch(url, {
      method, headers, credentials, body: body === undefined ? undefined : JSON.stringify(body),
      signal: signal ? AbortSignal.any([signal, AbortSignal.timeout(timeout)]) : AbortSignal.timeout(timeout),
    });
    if (!response.ok) throw await fail(response);
    return response.json().catch(() => ({}));
  }
  async function download(operation, query, { signal, timeout = 15 * 60 * 1000 } = {}) {
    const { url, headers } = resolve(operation, query);
    const response = await fetch(url, { headers, credentials, signal: signal ? AbortSignal.any([signal, AbortSignal.timeout(timeout)]) : AbortSignal.timeout(timeout) });
    if (!response.ok) throw await fail(response);
    return response.blob();
  }
  // XMLHttpRequest is used for the upload progress events fetch does not provide.
  function upload(operation, query, file, { method = 'PUT', onProgress, signal } = {}) {
    const { url, headers } = resolve(operation, query);
    return new Promise((resolvePromise, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open(method, url);
      xhr.withCredentials = credentials === 'include';
      for (const [name, value] of Object.entries(headers)) xhr.setRequestHeader(name, value);
      xhr.upload.onprogress = (event) => { if (event.lengthComputable) onProgress?.(event.loaded, event.total); };
      xhr.onload = () => {
        let result = {};
        try { result = JSON.parse(xhr.responseText || '{}'); } catch { /* non-JSON error body */ }
        if (xhr.status >= 200 && xhr.status < 300) { resolvePromise(result); return; }
        const error = new Error(result.error || `요청에 실패했어요 (${xhr.status}).`);
        error.status = xhr.status; reject(error);
      };
      xhr.onerror = () => reject(new Error('네트워크 오류로 전송하지 못했어요.'));
      xhr.ontimeout = () => reject(new Error('전송 시간이 초과됐어요.'));
      xhr.timeout = 35 * 60 * 1000;
      const abort = () => { xhr.abort(); reject(new DOMException('aborted', 'AbortError')); };
      if (signal) { if (signal.aborted) { abort(); return; } signal.addEventListener('abort', abort, { once: true }); }
      xhr.send(file);
    });
  }
  return { request, download, upload, key: () => target()?.key || '' };
}

export function formatSize(bytes) {
  if (!Number.isFinite(bytes) || bytes < 0) return '';
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KB', 'MB', 'GB'];
  let value = bytes / 1024, unit = 0;
  while (value >= 1024 && unit < units.length - 1) { value /= 1024; unit++; }
  return `${value >= 100 ? value.toFixed(0) : value.toFixed(1)} ${units[unit]}`;
}

export function saveBlob(blob, name) {
  const url = URL.createObjectURL(blob), link = document.createElement('a');
  link.href = url; link.download = name; document.body.append(link); link.click(); link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}
