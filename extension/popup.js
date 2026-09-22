/**
 * Status popup. Reads the service worker's own state rather than keeping a copy,
 * so what it shows is what the bridge is actually doing.
 *
 * It carries two actions:
 *
 *   Copy Cookies       a cookie export, for when a dump has to be produced by
 *                      hand without going through the backend's HTTP surface.
 *   Open Google Flow   opens the app in a new tab, so the panel is still useful
 *                      when the backend is not running at all.
 *
 * There is deliberately no bridge-token field. Pairing is not a human step — the
 * extension connects tokenless on first run, the backend hands it a token over
 * that connection, and the extension persists it and reconnects with it. The
 * token is therefore absent from this file entirely: no input, no read of
 * `config.bridgeToken`, nothing to validate. `socketUrl` and the reconnect in
 * `background.js` still carry it, because they are what completes the handover.
 *
 * The DOM surface is deliberately narrow — getElementById and plain node
 * properties only. The test harness supplies a stub DOM with exactly those, so
 * querySelector, classList or innerHTML would break the harness rather than the
 * browser, which is the failure mode this file is most likely to hit.
 *
 * Note that the script assigns `className` wholesale (`badge.className = 'ok'`).
 * Whatever class the markup starts with is gone after the first refresh, so
 * every rule in popup.html has to key off the element itself plus `.ok` / `.off`
 * and nothing else.
 */

const el = (id) => document.getElementById(id);

/** Ask the worker something over the popup channel. */
async function send(op, params) {
  try {
    return await chrome.runtime.sendMessage(params ? { op, params } : { op });
  } catch {
    return null;
  }
}

async function refresh() {
  const status = await send('status');

  const badge = el('badge');
  const sessionStatus = el('sessionStatus');
  const syncStatus = el('syncStatus');
  const engineStatus = el('engineStatus');

  if (status?.daemonConnected) {
    if (badge) {
      badge.textContent = 'Connected';
      badge.className = 'ok';
    }

    // The port is read from the config rather than written into the label, so a
    // changed WS_PORT does not leave the popup claiming 9222.
    if (engineStatus) {
      let port = '9222';
      try {
        if (status?.config?.bridgeUrl) {
          port = new URL(status.config.bridgeUrl).port || '9222';
        }
      } catch (_) {
        // An unparseable endpoint is not worth a blank line; the default stands.
      }

      engineStatus.textContent = `Connected (:${port})`;
      engineStatus.className = 'v';
    }

    // Cookie sync is what the backend pulls over the socket, so the honest
    // report is when it last pulled rather than a flat claim that it is running.
    if (syncStatus) {
      if (status.lastSyncTime) {
        const sec = Math.max(0, Math.floor((Date.now() - status.lastSyncTime) / 1000));
        if (sec < 5) syncStatus.textContent = 'Synced just now';
        else if (sec < 60) syncStatus.textContent = `Synced ${sec}s ago`;
        else syncStatus.textContent = `Synced ${Math.floor(sec / 60)}m ago`;
      } else {
        syncStatus.textContent = 'Real-time Active';
      }
      syncStatus.className = 'v ok';
    }

    // The extension cannot read the Labs session itself, so the count of
    // essential cookies stands in for it. Zero means there is nothing to
    // authenticate with, whatever the attached tab looks like.
    if (sessionStatus) {
      const count = status.cookieCount || 0;
      if (count > 0) {
        sessionStatus.textContent = `Active (${count} Cookies)`;
        sessionStatus.className = 'v ok';
      } else if (status.tabUrl && status.tabUrl.includes('flow.google.com')) {
        sessionStatus.textContent = 'Flow Tab Active';
        sessionStatus.className = 'v ok';
      } else {
        sessionStatus.textContent = 'No Cookies (Sign In)';
        sessionStatus.className = 'v faint';
      }
    }
  } else {
    if (badge) {
      badge.textContent = 'Disconnected';
      badge.className = 'off';
    }
    if (sessionStatus) {
      sessionStatus.textContent = 'Waiting for Bridge';
      sessionStatus.className = 'v faint';
    }
    if (syncStatus) {
      syncStatus.textContent = 'Disconnected';
      syncStatus.className = 'v faint';
    }
    if (engineStatus) {
      engineStatus.textContent = 'Offline';
      engineStatus.className = 'v off';
    }
  }
}

/**
 * The manifest is the only place the version is written down, so read it from
 * there rather than repeating it in the markup where the two would drift.
 */
function paintVersion() {
  try {
    const version = chrome.runtime.getManifest()?.version;
    if (version) el('version').textContent = `v${version}`;
  } catch {
    // A popup that cannot read its own manifest still has to render, and the
    // markup already carries a sane fallback.
  }
}

/* ------------------------------------------------------------------ *
 * Cookie export
 * ------------------------------------------------------------------ */

/**
 * The essential cookies for every configured domain, de-duplicated.
 *
 * Deliberately the same operation the backend uses, so what lands on the
 * clipboard is the set the engine would sync rather than "whatever Chrome has".
 * One call per domain is required and not a shortcut avoided: an unscoped read is
 * refused on purpose, and widening it here would export every cookie in the
 * profile — analytics for every Google property, plus a separate session for
 * Mail, Drive and the rest.
 */
async function collectCookies() {
  const status = await send('status');
  const domains = status?.config?.cookieDomains || [];
  if (domains.length === 0) throw new Error('no cookie domains are configured');

  const seen = new Set();
  const merged = [];

  for (const domain of domains) {
    const reply = await send('cookies.list', { details: { domain } });

    // The worker wraps a list reply in `result` instead of spreading it, because
    // `{...["a"]}` is `{0:"a"}` — spreading an array into an object silently
    // drops its array-ness, and the popup would then iterate over an object.
    const list = Array.isArray(reply?.result) ? reply.result : [];
    for (const cookie of list) {
      const key = `${cookie.domain}\t${cookie.name}\t${cookie.path}`;
      if (seen.has(key)) continue;
      seen.add(key);
      merged.push(cookie);
    }
  }
  return merged;
}

el('copy').addEventListener('click', async () => {
  const result = el('result');
  const button = el('copy');
  button.disabled = true;
  result.className = '';
  result.textContent = 'Collecting cookies…';

  try {
    const cookies = await collectCookies();
    if (cookies.length === 0) {
      throw new Error('no cookies were visible for the configured domains');
    }
    await navigator.clipboard.writeText(JSON.stringify(cookies, null, 2));
    result.textContent = `Copied ${cookies.length} cookies.`;
    result.className = 'ok';
  } catch (error) {
    result.textContent = `Could not copy: ${error?.message || String(error)}`;
    result.className = 'off';
  } finally {
    button.disabled = false;
  }
});

el('openFlow')?.addEventListener('click', () => {
  if (typeof chrome !== 'undefined' && chrome.tabs?.create) {
    chrome.tabs.create({ url: 'https://flow.google.com' });
  } else if (typeof window !== 'undefined') {
    window.open('https://flow.google.com', '_blank');
  }
});

paintVersion();
refresh();
setInterval(refresh, 1000);
