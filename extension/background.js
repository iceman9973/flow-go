/**
 * Flow Go Bridge — background service worker.
 *
 * The browser surface of the flow-go backend, and nothing more. Where the
 * generic CDP bridge this replaces exposes `cdp.call` and `cdp.evaluate` — that
 * is, arbitrary DevTools access to the whole browser — this extension exposes
 * the handful of things the backend actually needs from a page, each with its
 * own scope:
 *
 *   cookies.list      the ~15 cookies Flow depends on, and only those
 *   flow.fingerprint  the identity a generation request has to present
 *   flow.navigate     move the attached tab, inside the tab scope
 *   flow.projects     the project links on the current page
 *   flow.captcha      is the client loaded, and mint a token for an action
 *   flow.upscale      run the app's own SPrCad call and hand back the image
 *
 * Of those, flow.upscale is the one nothing calls. It is implemented and tested
 * here and on the Go side, and the backend has no path that reaches it — the
 * upscale chain there ends at Engine.SubmitVideo, which has no callers either.
 * It is kept rather than deleted because the Go implementation is the last
 * description of that wire shape, and it is flagged here so that finding it in
 * the advertised ops list is not mistaken for the backend using it.
 *
 * Everything page-level runs through chrome.scripting.executeScript rather than
 * chrome.debugger. That is the substantive difference from the generic bridge:
 * no `debugger` permission, so Chrome never shows the "started debugging this
 * browser" banner, and no generic escape hatch ships to a user.
 *
 * Wire format is the backend's: {id, op, params} in, {id, result} or
 * {id, error:{message}} out. Events are pushed as {event, params}.
 */

import { loadConfig, saveConfig, resetConfig, urlAllowed, domainAllowed } from './config.js';

const PROTOCOL_VERSION = 1;

let socket = null;
let reconnectTimer = null;
let attachedTabId = null;
let config = null;
let eventQueue = [];

const state = {
  daemonConnected: false,
  attachedTabId: null,
  tabTitle: null,
  tabUrl: null,
  lastOp: null,
  lastError: null,
  lastActivity: null,
  captchaMints: 0,
  upscales: 0,
};

/* ------------------------------------------------------------------ *
 * Cookies
 * ------------------------------------------------------------------ */

/**
 * The cookies Flow actually depends on.
 *
 * The backend asks for a whole domain scope, and a signed-in Chrome profile has
 * hundreds of cookies under `.google.com` — analytics for every Google property
 * and a separate session for Mail, Drive, Play, NotebookLM, Colab and the rest.
 * None of them have any bearing on Flow, they made a 19 kB Cookie header, and
 * they were being written to disk. The filter lives here rather than in the
 * backend because this is the extension that knows what Flow needs.
 */
const ESSENTIAL_COOKIES = new Set([
  // The Labs session, and the Google identity cookies it is rebuilt from.
  '__Secure-next-auth.session-token',
  '__Secure-next-auth.callback-url',
  '__Host-next-auth.csrf-token',
  '__Secure-1PSID',
  '__Secure-3PSID',
  '__Secure-1PSIDTS',
  '__Secure-3PSIDTS',
  'SID',
  'HSID',
  'SSID',
  'APISID',
  'SAPISID',
  // Set by the Flow app itself.
  'EMAIL',
  'OSID',
  '__Secure-OSID',
  // The account session, and the record of which accounts are signed in.
  //
  // `authuser=N` selects among the accounts a browser is signed into, but only
  // alongside the cookies that carry those sessions. Dropping these left the
  // parameter with nothing to select, so every index answered for the default
  // account and the account list read as one long.
  'LSID',
  'LSOLH',
  '__Host-1PLSID',
  '__Host-3PLSID',
  'ACCOUNT_CHOOSER',
]);

const isEssentialCookie = (name) => ESSENTIAL_COOKIES.has(String(name || ''));

/**
 * The cookies visible for one scope.
 *
 * `getAll({domain})` can come back empty for a host that plainly has cookies —
 * no error, no warning, and the host permission is granted. A per-name `get` on
 * the host's own URL is a different path through Chrome, so when the domain
 * query yields nothing at all it is worth asking by name before reporting the
 * host as having no cookies. Both are scoped and both are permission-checked;
 * this only changes which one is asked first.
 */
async function cookiesForScope(domain, url) {
  const found = await chrome.cookies.getAll(domain ? { domain } : { url });
  if (found.length > 0 || !domain) return found;

  const byName = [];
  for (const name of ESSENTIAL_COOKIES) {
    const one = await chrome.cookies.get({ url: `https://${domain}/`, name });
    if (one) byName.push(one);
  }
  return byName;
}

/* ------------------------------------------------------------------ *
 * Tabs
 * ------------------------------------------------------------------ */

async function listScopedTabs() {
  const cfg = await ensureConfig();
  const tabs = await chrome.tabs.query({});
  return tabs.filter((t) => urlAllowed(t.url, cfg));
}

async function resolveTab(tabId = null) {
  if (tabId) {
    const tab = await chrome.tabs.get(tabId).catch(() => null);
    if (tab && urlAllowed(tab.url, await ensureConfig())) return tab;
  }
  if (attachedTabId !== null) {
    const tab = await chrome.tabs.get(attachedTabId).catch(() => null);
    if (tab && urlAllowed(tab.url, await ensureConfig())) return tab;
  }
  const scoped = await listScopedTabs();
  if (scoped.length > 0) return scoped[0];

  const cfg = await ensureConfig();
  if (!cfg.autoOpenOnCommand) throw new Error('No tab inside the configured scope is open');

  const target = (cfg.targetUrlPrefixes || [])[0];
  if (!target) throw new Error('No tab inside the configured scope is open, and no target to open');

  const created = await chrome.tabs.create({ url: target, active: false });
  await waitForTab(created.id, (t) => urlAllowed(t.url, cfg), 45000, target);
  return chrome.tabs.get(created.id);
}

function attach(tabId) {
  attachedTabId = tabId;
  updateState({ attachedTabId: tabId });
  return chrome.tabs.get(tabId);
}

function detach() {
  attachedTabId = null;
  updateState({ attachedTabId: null });
  return { detached: true };
}

async function waitForTab(tabId, predicate, timeoutMs = 45000, what = 'The tab') {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const tab = await chrome.tabs.get(tabId).catch(() => null);
    if (tab && predicate(tab)) return tab;
    if (Date.now() > deadline) throw new Error(`${what} did not reach the expected state`);
    await sleep(250);
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/* ------------------------------------------------------------------ *
 * Page-level work
 * ------------------------------------------------------------------ */

/**
 * Run a function in the attached tab's MAIN world and return its value.
 *
 * MAIN rather than the default isolated world, because everything this extension
 * reads lives on the page: window.grecaptcha, navigator.userAgentData, the
 * rendered project list. The function must be self-contained — it is serialised
 * and re-created in the page, so it cannot close over anything here.
 */
async function inPage(fn, args = []) {
  const tab = await resolveTab();
  const results = await chrome.scripting.executeScript({
    target: { tabId: tab.id },
    world: 'MAIN',
    func: fn,
    args,
  });
  if (!results || results.length === 0) throw new Error('The page returned nothing');
  return results[0].result;
}

/**
 * The page's own request identity.
 *
 * The reCAPTCHA assessment is tied to the client that produced the token, so a
 * generation request that goes out under a different user-agent or sec-ch-ua
 * does not line up with its own token and is rejected as unusual activity.
 *
 * The shape is the backend's, down to `mobile` being a `?0`/`?1` client hint and
 * `platformFull` being a quoted string — those go straight into sec-ch-ua
 * headers, so the formatting is part of the value.
 */
function readFingerprint() {
  const d = navigator.userAgentData || {};
  const brands = (d.brands || []).map((b) => '"' + b.brand + '";v="' + b.version + '"').join(', ');
  return {
    userAgent: navigator.userAgent,
    language: navigator.language || 'en-US',
    brands,
    platform: d.platform || '',
    mobile: d.mobile ? '?1' : '?0',
    platformFull: '"' + (d.platform || 'macOS') + '"',
  };
}

/** The project links the Flow app has rendered on the current page. */
function readProjectLinks() {
  return [...document.querySelectorAll('a[href*="/project/"]')].map((a) => a.href).slice(0, 40);
}

/**
 * The two values the app puts on every batchexecute request.
 *
 * `at` goes in the form body and `f.sid` in the query string. The backend read the
 * first and never sent the second, so every call it made was missing a parameter
 * the app sends on all of them — visible only by reading the app's own traffic.
 */
function readPageTokens() {
  const w = window.WIZ_global_data || {};
  return { at: w.SNlM0e || '', fsid: w.FdrFJe || '' };
}

/**
 * Mint a reCAPTCHA Enterprise token with the page's own client.
 *
 * The `ready()` wait is load-bearing: skipping it still returns a token, but one
 * produced before the client finished initialising, and the assessment behind it
 * scores low enough that the upstream rejects the call. A token of the right
 * length is therefore not evidence that this worked.
 */
async function mintCaptcha(siteKey, action) {
  try {
    if (!window.grecaptcha || !window.grecaptcha.enterprise) {
      return { available: false, error: 'the page has no reCAPTCHA client loaded' };
    }
    await new Promise((resolve) => window.grecaptcha.enterprise.ready(resolve));
    const token = await window.grecaptcha.enterprise.execute(siteKey, { action });
    return { available: true, token };
  } catch (e) {
    return { available: true, error: String(e) };
  }
}

/**
 * Run the app's own image-upscale call in the page and return the image.
 *
 * The backend cannot make this call itself: the identical request from Go is
 * rejected with PUBLIC_ERROR_UNUSUAL_ACTIVITY while the same token and payload
 * succeed from the page, and the difference is the TLS fingerprint. So the page
 * makes it.
 *
 * The request and the extraction mirror the in-page expression the backend used
 * before this extension existed. That expression lived in an upscale.go the Go
 * side no longer has — the file is gone from the tree, along with the
 * TestUpscaleExpressionMatchesTheRequestTheAppExpects that used to pin it — so
 * the claim this comment used to make, that changing one side fails the other's
 * test, is no longer true and is not a property anyone should rely on.
 *
 * What does hold it is this extension's own harness: the request made from here
 * is pinned by the flow.upscale cases in test/dispatch.test.mjs. That covers the
 * extension's half and nothing else.
 *
 * Be aware of what this operation is before wiring it to anything: it is
 * implemented and tested at both ends and called by nothing in the engine. The
 * Go-side upsample cluster is unreachable too — the chain ends at
 * Engine.SubmitVideo, which has no callers. See the Upsampling section of
 * internal/flowapi/generate.go.
 */
async function runUpscale(cfg) {
  try {
    if (!window.grecaptcha || !window.grecaptcha.enterprise) {
      return { error: 'grecaptcha is not loaded on this page' };
    }
    await new Promise((resolve) => window.grecaptcha.enterprise.ready(resolve));
    const token = await window.grecaptcha.enterprise.execute(cfg.siteKey, { action: cfg.action });

    // Position [0] is the media id, not the content id. This is the opposite of
    // the generation calls, which take content ids in startImage/endImage, and
    // getting it backwards returns a null payload rather than an error — which
    // is exactly how it presented. Captured from the app's own SPrCad request.
    //
    // Position [1] is the resolution selector and [2] is the context block the
    // app always sends: tool id 22, the project, then the captcha pair.
    const arg = [
      cfg.mediaId,
      cfg.resolution,
      [null, 22, null, null, null, cfg.projectId, null, null, null, null, [token, 1]],
    ];

    const body = new URLSearchParams();
    body.set('f.req', JSON.stringify([[['SPrCad', JSON.stringify(arg), null, 'generic']]]));
    const at = (window.WIZ_global_data && window.WIZ_global_data.SNlM0e) || '';
    if (at) body.set('at', at);

    // The source path names the project, and only the project. An earlier
    // version appended `/edit/<mediaId>`; the app's own request does not, and
    // the media id travels in arg[0] instead.
    let url = '/_/AiSandboxAngularFrontend/data/batchexecute?rpcids=SPrCad'
      + '&source-path=' + encodeURIComponent('/project/' + cfg.projectId)
      + '&hl=' + encodeURIComponent(navigator.language || 'en')
      + '&rt=c';
    if (cfg.buildLabel) url += '&bl=' + encodeURIComponent(cfg.buildLabel);

    const resp = await fetch(url, {
      method: 'POST',
      credentials: 'include',
      headers: {
        'content-type': 'application/x-www-form-urlencoded;charset=UTF-8',
        'X-Same-Domain': '1',
      },
      body: body.toString(),
    });
    const text = await resp.text();

    if (text.indexOf('PUBLIC_ERROR') !== -1) {
      const m = text.match(/PUBLIC_ERROR_[A-Z_]+/);
      return { error: m ? m[0] : 'PUBLIC_ERROR' };
    }

    // The body is a wrb.fr frame stream, and the payload is a JSON *string*
    // inside it — so the image arrives with its quotes escaped as \". Scraping
    // the raw text for a quoted base64 run therefore finds nothing at all: the
    // character in front of the image is a backslash, not a quote, and every
    // upscale would report "no image data" on a response that plainly has one.
    // Parse the frame, then walk the decoded payload.
    const line = text.split('\n').find((l) => l.trim().indexOf('[["wrb.fr"') === 0);
    if (!line) {
      // Carry a snippet of what actually came back. "No SPrCad frame" alone
      // cannot be told apart from an upstream error page, a captcha rejection,
      // or a frame id that moved — and the response is the only place that
      // distinction lives.
      return { error: 'the response carried no SPrCad frame: ' + text.trim().slice(0, 240) };
    }

    const outer = JSON.parse(line);
    const inner = JSON.parse(outer[0][2]);

    // The image is the longest base64-looking string anywhere in the payload.
    // Walking beats a fixed path: the metadata around it moves between captures,
    // and the image is unambiguous by size and alphabet.
    let best = '';
    const walk = (x) => {
      if (typeof x === 'string') {
        if (x.length > best.length && /^[A-Za-z0-9+/=\-_]{500,}$/.test(x)) best = x;
      } else if (Array.isArray(x)) {
        for (const v of x) walk(v);
      } else if (x && typeof x === 'object') {
        for (const v of Object.values(x)) walk(v);
      }
    };
    walk(inner);

    if (!best) {
      // Carry what actually came back. "No image data" cannot be told apart from
      // an error frame, a payload whose image moved, or a response that hands
      // back a URL instead of bytes — and the response is where that distinction
      // lives.
      return { error: 'the response carried no image data: ' + JSON.stringify(inner).slice(0, 400) };
    }
    return { data: best };
  } catch (e) {
    return { error: String(e) };
  }
}

/* ------------------------------------------------------------------ *
 * Operations
 * ------------------------------------------------------------------ */

async function handle(op, params = {}) {
  const cfg = await ensureConfig();

  switch (op) {
    case 'ping':
      // `ops` is how the backend decides which surface it is talking to. The
      // generic bridge answers ping too, but without this list — so a backend
      // that prefers these operations can tell the two apart instead of guessing
      // from a failed call.
      return {
        ok: true,
        protocol: PROTOCOL_VERSION,
        version: chrome.runtime.getManifest().version,
        ops: [
          'ping', 'config.get', 'config.set', 'config.reset',
          'tabs.list', 'tabs.open', 'tab.attach', 'tab.detach', 'tab.current',
          'events.read', 'cookies.list', 'cookies.names',
          'flow.fingerprint', 'flow.navigate', 'flow.projects',
          'flow.captcha', 'flow.upscale', 'flow.at', 'status',
        ],
      };

    case 'config.get':
      return cfg;

    case 'config.set': {
      const previous = await ensureConfig();
      const next = await saveConfig(params.patch || {});
      config = next;

      // Reconnect when the endpoint or the credential changes. On first pairing
      // this is what moves the socket from tokenless to authenticated: the
      // backend hands the token over on the connection it has just accepted, and
      // without a reconnect the socket stays tokenless and the pairing marker is
      // never written — so the bridge would sit in the open state for good.
      //
      // The close is deferred by a tick so the reply goes out first. Closing
      // synchronously drops it: `send` checks `readyState`, and `close` moves it
      // to CLOSING before returning, so the backend would report a disconnected
      // socket for a `config.set` that actually succeeded.
      const endpointChanged =
        (params.patch?.bridgeUrl && params.patch.bridgeUrl !== previous.bridgeUrl) ||
        (params.patch?.bridgeToken && params.patch.bridgeToken !== previous.bridgeToken);
      if (endpointChanged) setTimeout(() => socket?.close(), 0);

      return next;
    }

    case 'config.reset':
      config = await resetConfig();
      return config;

    case 'tabs.list': {
      const tabs = await listScopedTabs();
      return tabs.map((t) => ({ tabId: t.id, url: t.url, title: t.title || null }));
    }

    case 'tabs.open': {
      const url = String(params.url || '');
      if (!urlAllowed(url, cfg)) throw new Error(`Refusing to open a URL outside the tab scope: ${url}`);
      const created = await chrome.tabs.create({ url, active: params.active !== false });
      const tab = await waitForTab(created.id, (t) => urlAllowed(t.url, cfg), 45000, url);
      return { tabId: tab.id, url: tab.url, title: tab.title || null };
    }

    case 'tab.attach': {
      const tab = await resolveTab(params.tabId || null);
      const attached = await attach(tab.id);
      return { tabId: attached.id, url: attached.url, title: attached.title || null };
    }

    case 'tab.detach':
      return detach();

    case 'tab.current': {
      if (attachedTabId === null) return null;
      const tab = await chrome.tabs.get(attachedTabId).catch(() => null);
      return tab ? { tabId: tab.id, url: tab.url, title: tab.title || null } : null;
    }

    case 'events.read': {
      const limit = Number(params.limit) > 0 ? Number(params.limit) : 500;
      const out = eventQueue.slice(-limit);
      eventQueue = [];
      return out;
    }

    case 'cookies.names': {
      // A diagnostic: which cookie names are visible for a scope, without any
      // values. Retrieval fails silently in three different ways — the scope
      // check refuses, the host permission was never granted, and the name
      // filter drops everything — and all three return the same empty list.
      // Chrome gives no error for a missing host permission, so the only way to
      // tell them apart is to look before the filter runs.
      const details = params.details || {};
      const domain = String(details.domain || '');
      const url = String(details.url || '');
      if (!domain && !url) {
        throw new Error('Refusing an unscoped cookie read: give a domain or a url');
      }

      const direct = await chrome.cookies.getAll(domain ? { domain } : { url });
      const found = await cookiesForScope(domain, url);
      const names = [...new Set(found.map((c) => String(c.name)))].sort();
      const hosts = [...new Set(found.map((c) => String(c.domain)))].sort();

      return {
        scope: domain || url,
        // `direct` is what the domain query alone returned, `seen` is what the
        // per-name fallback added. When direct is 0 and seen is not, the domain
        // query is the thing that cannot see this host.
        direct: direct.length,
        seen: found.length,
        names,
        hosts,
        kept: names.filter(isEssentialCookie),
      };
    }

    case 'cookies.list': {
      const details = params.details || {};
      const domain = String(details.domain || '');
      const url = String(details.url || '');

      // Exactly one scope is required, and it has to be given rather than
      // inferred. An unscoped call used to fall through to `getAll({})` — every
      // cookie in the profile — and because the name filter downstream kept
      // only the fifteen Flow cookies, the widening was invisible: it looked
      // like a scoped read that happened to return the right names.
      if (!domain && !url) {
        throw new Error('Refusing an unscoped cookie read: give a domain or a url');
      }
      if (domain && !domainAllowed(domain, cfg)) {
        throw new Error(`Refusing to read cookies outside the configured scope: ${domain}`);
      }
      if (url) {
        let host = '';
        try {
          host = new URL(url).hostname;
        } catch {
          throw new Error(`Not a URL: ${url}`);
        }
        if (!domainAllowed(host, cfg)) {
          throw new Error(`Refusing to read cookies outside the configured scope: ${url}`);
        }
      }

      const found = await cookiesForScope(domain, url);
      const kept = found.filter((c) => isEssentialCookie(c.name));
      return kept.map((c) => ({
        domain: c.domain,
        expirationDate: c.expirationDate,
        hostOnly: c.hostOnly,
        httpOnly: c.httpOnly,
        name: c.name,
        path: c.path,
        sameSite: c.sameSite,
        secure: c.secure,
        session: c.session,
        storeId: c.storeId,
        value: c.value,
      }));
    }

    case 'flow.fingerprint': {
      const fp = await inPage(readFingerprint);
      if (!fp || !fp.userAgent) throw new Error('The page did not report an identity');
      return fp;
    }

    case 'flow.navigate': {
      const url = String(params.url || '');
      if (!urlAllowed(url, cfg)) throw new Error(`Refusing to navigate outside the tab scope: ${url}`);
      const tab = await resolveTab();
      await chrome.tabs.update(tab.id, { url });
      return { tabId: tab.id, url };
    }

    case 'flow.projects':
      return await inPage(readProjectLinks);

    case 'flow.at': {
      const t = await inPage(readPageTokens);
      return { at: String((t && t.at) || ''), fsid: String((t && t.fsid) || '') };
    }

    case 'flow.captcha': {
      const action = String(params.action || '');
      const siteKey = cfg.recaptchaSiteKey || params.siteKey || '6LdsFiUsAAAAAIjVDZcuLhaHiDn5nnHVXVRQGeMV';
      if (!action) {
        // A probe: the caller wants to know whether the page can mint at all,
        // usually to decide whether to navigate somewhere that can.
        const fp = await inPage(() => !!(window.grecaptcha && window.grecaptcha.enterprise && window.grecaptcha.enterprise.execute));
        return { available: !!fp };
      }
      const out = await inPage(mintCaptcha, [siteKey, action]);
      if (out && out.token) state.captchaMints++;
      return out || { available: false, error: 'the page returned nothing' };
    }

    case 'flow.upscale': {
      // The source path the app expects names the media's editor route, so the
      // project id is required. The media id is passed through when the caller
      // has one — a caller holding only a content id is valid, and the app
      // accepts the project route without it.
      const projectId = String(params.projectId || '');
      if (!projectId) throw new Error('flow.upscale needs a projectId');

      const out = await inPage(runUpscale, [{
        siteKey: cfg.recaptchaSiteKey || '6LdsFiUsAAAAAIjVDZcuLhaHiDn5nnHVXVRQGeMV',
        action: 'IMAGE_GENERATION',
        projectId,
        mediaId: String(params.mediaId || ''),
        contentId: String(params.contentId || ''),
        resolution: Number(params.resolution) || 1,
        buildLabel: String(params.buildLabel || ''),
      }]);
      if (out && out.data) state.upscales++;
      return out || { error: 'the page returned nothing' };
    }

    case 'status':
      return { ...state, config: cfg, eventBuffer: eventQueue.length };

    default:
      throw new Error(`Unknown operation: ${op}`);
  }
}

/* ------------------------------------------------------------------ *
 * Connection
 * ------------------------------------------------------------------ */

async function ensureConfig() {
  if (!config) config = await loadConfig();
  return config;
}

function socketUrl(cfg) {
  const base = String(cfg.bridgeUrl || '').replace(/\/$/, '');
  const token = String(cfg.bridgeToken || '');
  return token ? `${base}?token=${encodeURIComponent(token)}` : base;
}

function send(payload) {
  if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify(payload));
}

function pushEvent(event, params) {
  eventQueue.push({ event, params, at: Date.now() });
  const max = config?.eventBufferSize || 500;
  if (eventQueue.length > max) eventQueue = eventQueue.slice(-max);
  send({ event, params });
}

function updateState(values) {
  Object.assign(state, values, { lastActivity: Date.now() });
  chrome.action.setBadgeText({ text: socket ? 'ON' : '' }).catch(() => {});
  chrome.action.setBadgeBackgroundColor({ color: '#16803c' }).catch(() => {});
}

async function connect() {
  if (socket && [WebSocket.OPEN, WebSocket.CONNECTING].includes(socket.readyState)) return;
  const cfg = await ensureConfig();

  try {
    socket = new WebSocket(socketUrl(cfg));
  } catch (error) {
    updateState({ daemonConnected: false, lastError: error?.message || String(error) });
    scheduleReconnect();
    return;
  }

  socket.onopen = () => {
    updateState({ daemonConnected: true, lastError: null });
    send({
      event: 'bridge.ready',
      params: {
        version: chrome.runtime.getManifest().version,
        protocol: PROTOCOL_VERSION,
        config: cfg,
      },
    });
  };

  socket.onmessage = async (event) => {
    let msg;
    try {
      msg = JSON.parse(event.data);
    } catch {
      return;
    }
    if (!msg || !msg.id) return;

    updateState({ lastOp: msg.op });
    try {
      const result = await handle(msg.op, msg.params || {});
      send({ id: msg.id, result });
    } catch (error) {
      updateState({ lastError: error?.message || String(error) });
      send({ id: msg.id, error: { message: error?.message || String(error) } });
    }
  };

  socket.onclose = () => {
    updateState({ daemonConnected: false });
    socket = null;
    scheduleReconnect();
  };

  socket.onerror = () => {
    updateState({ daemonConnected: false });
  };
}

function scheduleReconnect() {
  if (reconnectTimer) return;
  const delay = config?.reconnectDelayMs || 1500;
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    connect();
  }, delay);
}

chrome.alarms.create('keepAlive', { periodInMinutes: 0.5 });
chrome.alarms.onAlarm.addListener((alarm) => {
  if (alarm.name === 'keepAlive') connect();
});

chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);

// The popup asks the worker for its own state rather than keeping a copy, so what
// it shows is what the bridge is doing. It goes through the same dispatcher as
// the backend's calls, which keeps one definition of every operation.
chrome.runtime.onMessage.addListener((msg, _sender, reply) => {
  if (!msg || !msg.op) return false;
  handle(msg.op, msg.params || {})
    .then((result) => reply({ ok: true, ...result }))
    .catch((error) => reply({ ok: false, error: error?.message || String(error) }));
  return true;
});

// The event buffer stays, and events.read drains it, but nothing pushes into it
// by default: observing page navigations would need the webNavigation permission,
// and that buys a diagnostic nicety rather than a capability the backend uses.
// pushEvent is the hook for anything that later wants it.

connect();
