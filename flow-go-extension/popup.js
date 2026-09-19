/**
 * Status popup. Reads the service worker's own state rather than keeping a copy,
 * so what it shows is what the bridge is actually doing.
 *
 * It also carries the one control a user ever needs: the bridge token. An
 * extension that has lost it cannot connect, and the backend cannot tell it
 * anything, because the connection is refused before a message can be sent — so
 * the recovery has to start from this side.
 */

const el = (id) => document.getElementById(id);

/** Describe the stored token without revealing it. */
function tokenState(token) {
  if (!token) return { label: 'not set', className: 'off' };
  return { label: `set (${token.length} chars)`, className: 'ok' };
}

async function refresh() {
  let status = null;
  try {
    status = await chrome.runtime.sendMessage({ op: 'status' });
  } catch {
    status = null;
  }

  const daemon = el('daemon');
  if (status?.daemonConnected) {
    daemon.textContent = 'connected';
    daemon.className = 'ok';
  } else {
    daemon.textContent = status?.lastError ? `not connected — ${status.lastError}` : 'not connected';
    daemon.className = 'off';
  }

  const tab = el('tab');
  if (status?.tabUrl) {
    tab.textContent = status.tabTitle || status.tabUrl;
    tab.title = status.tabUrl;
  } else {
    tab.textContent = 'none attached';
  }

  el('mints').textContent = String(status?.captchaMints ?? 0);
  el('upscales').textContent = String(status?.upscales ?? 0);
  el('lastOp').textContent = status?.lastOp || '—';

  const state = tokenState(status?.config?.bridgeToken);
  el('tokenState').textContent = `(${state.label})`;
  el('tokenState').className = state.className;
}

el('tokenForm').addEventListener('submit', async (event) => {
  event.preventDefault();

  const value = el('token').value.trim();
  const result = el('result');
  if (!value) {
    result.textContent = 'Paste the token first.';
    result.className = 'off';
    return;
  }

  // Saving the credential makes the worker close its socket and reconnect with
  // it, which is what completes pairing. Nothing else to do here.
  el('save').disabled = true;
  result.textContent = 'Saving…';
  result.className = '';

  try {
    const reply = await chrome.runtime.sendMessage({
      op: 'config.set',
      params: { patch: { bridgeToken: value } },
    });
    if (reply?.ok === false) throw new Error(reply.error || 'the worker refused it');

    el('token').value = '';
    result.textContent = 'Saved. Reconnecting with it now.';
    result.className = 'ok';
  } catch (error) {
    result.textContent = `Could not save: ${error?.message || String(error)}`;
    result.className = 'off';
  } finally {
    el('save').disabled = false;
    refresh();
  }
});

refresh();
setInterval(refresh, 1000);
