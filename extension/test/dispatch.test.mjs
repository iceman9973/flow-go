/**
 * Dispatch harness for the Flow Bridge extension.
 *
 * The extension cannot be exercised without Chrome, and the parts that can break
 * silently are not the Chrome calls — they are the shapes. Every operation here
 * answers a JSON value that a Go decoder on the other end has a fixed idea of,
 * and a mismatch does not throw: it decodes into a zero value and reads as
 * "there was nothing there". That is exactly how the flow.captcha probe bug
 * survived — generation kept working, only the captcha got worse.
 *
 * So this harness stubs the Chrome surface, loads the real background.js, drives
 * every operation the backend calls, and asserts the answers against the shapes
 * in internal/cdp/client.go and internal/bridge/bridge.go.
 *
 * Run:  node test/dispatch.test.mjs
 */

import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const extDir = path.resolve(here, '..');

/* ------------------------------------------------------------------ *
 * Stage the extension so Node will import it as a module
 * ------------------------------------------------------------------ */

// The extension has no package.json, so Node would read background.js as
// CommonJS and choke on its `import`. Copying the two real files into a staged
// directory with a one-line package.json keeps the originals untouched.
const stage = fs.mkdtempSync(path.join(os.tmpdir(), 'fgext-'));
for (const file of ['background.js', 'config.js', 'popup.js']) {
  fs.copyFileSync(path.join(extDir, file), path.join(stage, file));
}
fs.writeFileSync(path.join(stage, 'package.json'), JSON.stringify({ type: 'module' }));

/* ------------------------------------------------------------------ *
 * Manifest coverage
 *
 * Chrome hands back a cookie only when the extension holds host permission for
 * the cookie's domain, and injection needs permission for the tab. So the
 * manifest has to cover every domain the extension is asked about — and a near
 * miss is invisible.
 *
 * `https://www.google.com/*` looks like it covers Google. It covers the `www`
 * host and nothing else, while the identity cookies the backend rebuilds its
 * session from — SID, HSID, SSID, APISID, SAPISID, __Secure-1PSID — are set on
 * `.google.com`, the parent domain. They are simply absent from the answer, with
 * no error anywhere. The backend then reports "no SAPISID cookie in the jar" and
 * the profile looks signed out when it is signed in.
 *
 * The scheme matters for the same reason and is easier to miss. Google still sets
 * `SID`, `HSID` and `APISID` without the Secure flag, and Chrome matches a cookie
 * against the URL it came from — so an https-only pattern omits exactly those
 * three and returns every Secure cookie beside them. The generic bridge's
 * `<all_urls>` covers http as well, which is why it saw them and a narrower
 * extension did not.
 *
 * The check is written against the domains in config.js rather than a hardcoded
 * list, so adding a scope to the config without widening the manifest fails here
 * instead of in the browser.
 * ------------------------------------------------------------------ */

const manifest = JSON.parse(fs.readFileSync(path.join(extDir, 'manifest.json'), 'utf8'));
const { DEFAULTS } = await import(path.join(stage, 'config.js'));

/** Does one manifest host pattern cover a bare host? */
function patternCoversHost(pattern, host) {
  if (pattern === '<all_urls>') return true;
  const match = /^[a-z*]+:\/\/([^/]+)\//.exec(pattern);
  if (!match) return false;

  const patternHost = match[1];
  if (patternHost === host) return true;

  // `*.example.com` covers example.com and everything under it.
  if (patternHost.startsWith('*.')) {
    const base = patternHost.slice(2);
    return host === base || host.endsWith(`.${base}`);
  }
  return false;
}

const uncovered = (hosts) =>
  hosts.filter((host) => !manifest.host_permissions.some((p) => patternCoversHost(p, host)));

/* ------------------------------------------------------------------ *
 * Chrome, as much of it as the extension touches
 * ------------------------------------------------------------------ */

const FLOW_URL = 'https://flow.google.com/project/abc123';
const MAIL_URL = 'https://mail.google.com/mail/u/0';

const tabs = [
  { id: 1, url: FLOW_URL, title: 'Flow' },
  { id: 2, url: MAIL_URL, title: 'Mail' },
];

const cookie = (domain, name, value) => ({
  domain,
  name,
  value,
  path: '/',
  secure: true,
  httpOnly: true,
  hostOnly: false,
  session: false,
  expirationDate: 9999999999,
  sameSite: 'no_restriction',
  storeId: '0',
});

const allCookies = [
  cookie('.google.com', '__Secure-1PSID', 'psid-1'),
  cookie('.google.com', '__Secure-1PSIDTS', 'psidts-1'),
  cookie('.google.com', 'SID', 'sid'),
  cookie('.google.com', 'HSID', 'hsid'),
  cookie('.google.com', 'SSID', 'ssid'),
  cookie('.google.com', 'APISID', 'apisid'),
  cookie('.google.com', 'SAPISID', 'sapisid'),
  cookie('.labs.google', '__Secure-next-auth.session-token', 'nextauth'),
  cookie('.labs.google', 'EMAIL', 'someone@example.com'),
  cookie('.labs.google', 'OSID', 'osid-labs'),
  cookie('.google.com', '__Secure-OSID', 'osid-secure'),
  // Everything below has to be dropped by the name filter.
  cookie('.google.com', '_ga', 'GA1.1.000000000.0000000000'),
  cookie('.google.com', '_ga_ABCDEFGHIJ', 'GS1.1.0000000000.1.0.0'),
  cookie('.google.com', '_gcl_au', '1.1.000000000.0000000000'),
  cookie('.google.com', '__utmz', '000000000.0000000000.1.1.utmcsr'),
  cookie('.google.com', 'AEC', 'AEC-value'),
  cookie('.google.com', 'NID', 'NID-value'),
  cookie('mail.google.com', 'OSID', 'osid-mail'),
  cookie('notebooklm.google.com', '__Secure-OSID', 'osid-notebooklm'),
  cookie('drive.google.com', 'COMPASS', 'COMPASS-value'),
];

// Hosts the stubbed domain query refuses to see, mirroring Chrome: no error, and
// the host permission is granted, but the answer is empty.
const getAllBlind = new Set();

const store = new Map();
let badgeText = '';

// The popup talks to the worker through runtime messaging, exactly as it does in
// Chrome. Capturing the listener is what lets the harness drive that path — the
// popup is the recovery route for an extension that has lost its token, so it is
// the one piece of UI that has to work when everything else already does not.
let workerOnMessage = null;

// The cookie-change listener the worker registers. Captured for the same reason
// the message listener is: the rotation announcement is the one thing the
// extension says unprompted, so there is no request to drive it with — the only
// way to exercise it is to fire the event Chrome would have fired.
let cookiesOnChanged = null;

const chromeStub = {
  runtime: {
    getManifest: () => ({ version: '1.0.0' }),
    onStartup: { addListener: () => {} },
    onInstalled: { addListener: () => {} },
    onMessage: {
      addListener: (fn) => { workerOnMessage = fn; },
    },
    // Routes a popup message into the worker's own dispatcher.
    sendMessage: (msg) =>
      new Promise((resolve) => {
        if (!workerOnMessage) {
          resolve({ ok: false, error: 'the worker is not listening' });
          return;
        }
        workerOnMessage(msg, {}, (reply) => resolve(reply));
      }),
  },
  alarms: { create: () => {}, onAlarm: { addListener: () => {} } },
  action: {
    setBadgeText: async ({ text }) => { badgeText = text; },
    setBadgeBackgroundColor: async () => {},
  },
  storage: {
    local: {
      get: async (key) => (store.has(key) ? { [key]: store.get(key) } : {}),
      set: async (obj) => { for (const [k, v] of Object.entries(obj)) store.set(k, v); },
      remove: async (key) => { store.delete(key); },
    },
  },
  tabs: {
    query: async () => tabs,
    get: async (id) => {
      const tab = tabs.find((t) => t.id === id);
      if (!tab) throw new Error(`No tab ${id}`);
      return tab;
    },
    create: async ({ url }) => {
      const tab = { id: 99, url, title: 'New' };
      tabs.push(tab);
      return tab;
    },
    update: async (id, { url }) => {
      const tab = tabs.find((t) => t.id === id);
      tab.url = url;
      return tab;
    },
  },
  cookies: {
    onChanged: {
      addListener: (fn) => { cookiesOnChanged = fn; },
    },
    getAll: async (query = {}) => {
      // Hosts Chrome refuses to see through the domain query, which it does
      // silently — no error, and the host permission is granted.
      if (query.domain && getAllBlind.has(query.domain)) return [];
      if (query.url && getAllBlind.has(new URL(query.url).hostname)) return [];

      if (query.url) {
        const host = new URL(query.url).hostname;
        return allCookies.filter((c) => c.domain.replace(/^\./, '') === host || host.endsWith(c.domain));
      }
      if (query.domain) {
        return allCookies.filter((c) => c.domain === query.domain || c.domain.endsWith(`.${query.domain}`));
      }
      return allCookies;
    },
    get: async ({ url, name }) => {
      if (!url || !name) return null;
      const host = new URL(url).hostname;
      return (
        allCookies.find(
          (c) => c.name === name && (c.domain.replace(/^\./, '') === host || host.endsWith(c.domain)),
        ) || null
      );
    },
  },
  // Runs the function the extension would have injected into the page.
  scripting: {
    executeScript: async ({ func, args }) => [{ result: await func(...(args || [])) }],
  },
};

/* ------------------------------------------------------------------ *
 * Page, socket, network
 * ------------------------------------------------------------------ */

const page = {
  grecaptcha: {
    enterprise: {
      ready: (resolve) => resolve(),
      execute: async (siteKey, { action }) => `token-for-${action}-${siteKey.length}`,
    },
  },
  // Both values the app puts on a batchexecute request: the anti-CSRF token in
  // the body and the session id in the query string.
  WIZ_global_data: { SNlM0e: 'at-value', FdrFJe: '-56329636' },
};

// An `upscaleFrame` fixture used to live here, with a guard that proved it
// round-tripped the image. Both went with `flow.upscale`: the op was implemented
// and tested at both ends and called by nothing, so it was deleted from the
// extension and from the backend's pinned bridge surface.

let sockets = [];

class FakeWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;

  constructor(url) {
    this.url = url;
    this.readyState = FakeWebSocket.OPEN;
    this.sent = [];
    sockets.push(this);
  }

  send(data) {
    const frame = JSON.parse(data);
    this.sent.push(frame);
    if (frame.id && this._pending?.has(frame.id)) {
      this._pending.get(frame.id)(frame);
      this._pending.delete(frame.id);
    }
  }

  close() {
    this.readyState = FakeWebSocket.CLOSED;
  }
}

/* ------------------------------------------------------------------ *
 * Install the stubs, then load the real extension
 * ------------------------------------------------------------------ */

const descriptors = {
  chrome: { value: chromeStub, configurable: true, writable: true },
  WebSocket: { value: FakeWebSocket, configurable: true, writable: true },
  window: { value: page, configurable: true, writable: true },
  document: {
    value: {
      querySelectorAll: (selector) =>
        selector === 'a[href*="/project/"]'
          ? [
              { href: 'https://flow.google.com/project/abc123' },
              { href: 'https://flow.google.com/project/def456' },
              { href: 'https://flow.google.com/project/abc123' },
            ]
          : [],
    },
    configurable: true,
    writable: true,
  },
  navigator: {
    value: {
      userAgent:
        'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36',
      language: 'en-US',
      userAgentData: {
        brands: [
          { brand: 'Chromium', version: '140' },
          { brand: 'Google Chrome', version: '140' },
        ],
        platform: 'macOS',
        mobile: false,
      },
    },
    configurable: true,
    writable: true,
  },
};

for (const [key, descriptor] of Object.entries(descriptors)) {
  Object.defineProperty(globalThis, key, descriptor);
}

await import(path.join(stage, 'background.js'));

// connect() is async — let it reach `new WebSocket(...)`.
for (let i = 0; i < 10 && sockets.length === 0; i++) {
  await new Promise((resolve) => setTimeout(resolve, 0));
}
assert.equal(sockets.length, 1, 'the extension should have opened one socket');

const ws = sockets[0];
ws._pending = new Map();
let seq = 0;

/** Drive one request through the extension exactly as the backend would. */
async function call(op, params) {
  const id = String(++seq);
  const reply = new Promise((resolve) => ws._pending.set(id, resolve));
  await ws.onmessage({ data: JSON.stringify({ id, op, params }) });
  const frame = await reply;
  if (frame.error) throw new Error(`${op}: ${frame.error.message}`);
  return frame.result;
}

/** The same call, but returning the error message rather than throwing. */
async function callExpectingError(op, params) {
  try {
    await call(op, params);
  } catch (error) {
    return error.message;
  }
  return null;
}

/* ------------------------------------------------------------------ *
 * Assertions — each one mirrors a decoder on the Go side
 * ------------------------------------------------------------------ */

const results = [];
const check = async (name, fn) => {
  try {
    await fn();
    results.push({ name, ok: true });
  } catch (error) {
    results.push({ name, ok: false, error: error.message });
  }
};

await check('ping advertises every op the backend calls', async () => {
  const pong = await call('ping');
  assert.equal(pong.ok, true);
  assert.equal(pong.protocol, 1);

  // internal/cdp/client.go calls exactly these by name.
  const needed = [
    'ping', 'config.get', 'config.set', 'tabs.list', 'tabs.open', 'tab.attach',
    'tab.detach', 'tab.current', 'events.read', 'cookies.list',
    'flow.fingerprint', 'flow.navigate', 'flow.projects', 'flow.captcha',
  ];
  const missing = needed.filter((op) => !pong.ops.includes(op));
  assert.deepEqual(missing, [], `not advertised: ${missing.join(', ')}`);
});

await check('no generic CDP escape hatch is reachable', async () => {
  for (const op of ['cdp.call', 'cdp.evaluate']) {
    const message = await callExpectingError(op, {});
    assert.match(String(message), /Unknown operation/);
  }
});

await check('the manifest can read every cookie domain the backend asks for', async () => {
  const missing = uncovered(DEFAULTS.cookieDomains);
  assert.deepEqual(missing, [], `no host permission covers: ${missing.join(', ')}`);
});

await check('the manifest can inject into every tab target', async () => {
  const hosts = DEFAULTS.targetUrlPrefixes.map((prefix) => new URL(prefix).hostname);
  const missing = uncovered(hosts);
  assert.deepEqual(missing, [], `no host permission covers: ${missing.join(', ')}`);
});

await check('the manifest holds no wildcard-subdomain permission', async () => {
  // The point of this extension over the generic bridge is a narrow surface. A
  // `*.` host pattern quietly covers every subdomain, and that is how the sync
  // came to drag in mail, drive, play and photos session cookies — the exact
  // thing this extension exists to avoid. If one is ever genuinely needed,
  // change this test on purpose rather than letting it reappear unnoticed.
  const wildcards = manifest.host_permissions.filter((pattern) => pattern.includes('//*.'));
  assert.deepEqual(wildcards, [], `wildcard host permissions: ${wildcards.join(', ')}`);
});

await check('flow.fingerprint matches bridge.Fingerprint', async () => {
  const fp = await call('flow.fingerprint');

  // Field names are the JSON tags of bridge.Fingerprint; the value formats are
  // the ones that go straight into sec-ch-ua headers.
  assert.deepEqual(Object.keys(fp).sort(), [
    'brands', 'language', 'mobile', 'platform', 'platformFull', 'userAgent',
  ]);
  assert.match(fp.userAgent, /^Mozilla\/5\.0/);
  assert.equal(fp.language, 'en-US');
  assert.equal(fp.platform, 'macOS');
  assert.equal(fp.brands, '"Chromium";v="140", "Google Chrome";v="140"');
  assert.equal(fp.mobile, '?0');
  assert.equal(fp.platformFull, '"macOS"');
});

await check('flow.projects returns a string array (bridge.DiscoverProjects)', async () => {
  const links = await call('flow.projects');
  assert.ok(Array.isArray(links), 'expected an array');
  assert.ok(links.every((l) => typeof l === 'string'), 'expected strings');
  assert.deepEqual(links, [
    'https://flow.google.com/project/abc123',
    'https://flow.google.com/project/def456',
    'https://flow.google.com/project/abc123',
  ]);
});

await check('flow.at returns the page tokens the app sends', async () => {
  const out = await call('flow.at');
  // The backend seeds these into its batchexecute client. The anti-CSRF token
  // goes in the form body and `f.sid` in the query string — the app sends both
  // on every request, and the engine sent only the first until the two were read
  // together from one live capture.
  assert.equal(out.at, 'at-value');
  assert.equal(out.fsid, '-56329636');
});

await check('flow.captcha probe matches FlowCaptchaResult', async () => {
  const probe = await call('flow.captcha', {});
  assert.equal(typeof probe.available, 'boolean');
  assert.equal(probe.available, true);
  assert.equal(probe.token, undefined, 'a probe must not carry a token');
});

await check('flow.captcha mint matches FlowCaptchaResult', async () => {
  const mint = await call('flow.captcha', { action: 'VIDEO_GENERATION' });
  assert.equal(mint.available, true);
  assert.equal(typeof mint.token, 'string');
  assert.ok(mint.token.length > 0);
});

// Four `flow.upscale` cases used to sit here, covering the frame parse, an
// upstream PUBLIC_ERROR, a response with no SPrCad frame, and the missing-project
// guard. The op was implemented and tested at both ends and called by nothing, so
// it was deleted — and a case asserting "Unknown operation: flow.upscale" would
// be testing the deletion rather than the behaviour. What is worth keeping is
// that the deletion is visible: `ping` no longer advertises it, and the case
// above checks the advertised list against the ops the backend actually calls.

await check('flow.navigate refuses a URL outside the tab scope', async () => {
  const message = await callExpectingError('flow.navigate', { url: 'https://example.com/' });
  assert.match(String(message), /outside the tab scope/);
});

await check('cookies.list keeps only the essential names', async () => {
  const cookies = await call('cookies.list', { details: { domain: 'google.com' } });
  const names = [...new Set(cookies.map((c) => c.name))].sort();

  const dropped = ['_ga', '_ga_ABCDEFGHIJ', '_gcl_au', '__utmz', 'AEC', 'NID'];
  for (const name of dropped) {
    assert.ok(!names.includes(name), `${name} should have been filtered out`);
  }

  // The shape is cookiejar.Cookie's JSON tags, not Chrome's camelCase.
  const one = cookies[0];
  assert.deepEqual(Object.keys(one).sort(), [
    'domain', 'expirationDate', 'hostOnly', 'httpOnly', 'name', 'path',
    'sameSite', 'secure', 'session', 'storeId', 'value',
  ]);
});

await check('cookies.list scopes a url read to that host', async () => {
  const cookies = await call('cookies.list', { details: { url: 'https://labs.google/fx/tools/flow' } });
  const names = cookies.map((c) => c.name).sort();
  assert.ok(names.includes('__Secure-next-auth.session-token'));
  assert.ok(names.includes('EMAIL'));
  // A mail-host cookie must not come back for a labs url.
  assert.ok(!cookies.some((c) => c.domain === 'mail.google.com'));
});

await check('cookies.list refuses an unscoped read', async () => {
  const message = await callExpectingError('cookies.list', { details: {} });
  assert.match(String(message), /unscoped cookie read/);
});

await check('cookies.list refuses a domain outside the scope', async () => {
  const message = await callExpectingError('cookies.list', { details: { domain: 'example.com' } });
  assert.match(String(message), /outside the configured scope/);
});

await check('cookies.names reports what is visible before the filter', async () => {
  const out = await call('cookies.names', { details: { domain: 'google.com' } });

  assert.equal(out.scope, 'google.com');
  assert.equal(typeof out.seen, 'number');
  assert.equal(out.direct, out.seen, 'with the domain query working, no fallback is needed');

  // The whole point of the diagnostic: it shows the names the filter drops, so
  // an empty result can be told apart from a filter that ate everything.
  assert.ok(out.names.includes('_ga'), 'the unfiltered view must include names the filter drops');
  assert.ok(!out.kept.includes('_ga'), 'the kept view must not include them');
  assert.ok(out.kept.includes('SAPISID'), 'the kept view must include the identity cookies');

  // No values anywhere in it.
  assert.ok(!JSON.stringify(out).includes('psid-1'), 'the diagnostic must not leak values');
});

await check('cookies falls back to per-name reads when the domain query is blind', async () => {
  // Chrome can answer a domain query with nothing for a host that has cookies,
  // with no error and the permission granted. The per-name read is a different
  // path through it, so it is worth asking before giving up on the host.
  getAllBlind.add('labs.google');
  try {
    const out = await call('cookies.names', { details: { domain: 'labs.google' } });
    assert.equal(out.direct, 0, 'the domain query should return nothing in this case');
    assert.ok(out.seen > 0, 'the per-name fallback should still find the cookies');
    assert.ok(out.kept.includes('__Secure-next-auth.session-token'), 'and the session cookie must survive');

    const listed = await call('cookies.list', { details: { domain: 'labs.google' } });
    assert.ok(
      listed.some((c) => c.name === '__Secure-next-auth.session-token'),
      'cookies.list should return the session cookie too, not report the host as empty',
    );
  } finally {
    getAllBlind.delete('labs.google');
  }
});

/* ------------------------------------------------------------------ *
 * Cookie rotation
 *
 * The one thing the extension says without being asked, so there is no request to
 * drive it with — the only way to exercise it is to fire the event Chrome would
 * have fired and then read what reached the socket.
 *
 * The debounce is shortened through config rather than slept through. The window
 * is a deployment property, which is why it is a config key at all, and a suite
 * that waits 1.5 seconds per case is a suite that gets skipped.
 * ------------------------------------------------------------------ */

// 300 ms rather than the 1500 ms default. The window is a deployment property,
// which is why it is a config key at all — and the burst case below has to time a
// change *inside* the window, which a 1.5 s default would make a two-second test.
await call('config.set', { patch: { cookieRotationDebounceMs: 300 } });

/** The rotation frames the worker has put on the wire so far. */
const rotationFrames = () =>
  ws.sent.filter((f) => f.event === 'bridge.cookies_rotated').map((f) => f.params);

const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

/** Wait for the debounce to expire and the re-read behind it to finish. */
async function waitForRotation(before) {
  for (let i = 0; i < 60; i++) {
    if (rotationFrames().length > before) return;
    await pause(25);
  }
}

await check('the worker subscribes to cookie changes', async () => {
  assert.equal(
    typeof cookiesOnChanged, 'function',
    'chrome.cookies.onChanged should have a listener, or a rotation is never seen',
  );
});

await check('a rotation burst is announced once, after the window restarts', async () => {
  const before = rotationFrames().length;

  // The event is deliberately stale: it carries the value Chrome saw when it
  // fired, and the debounce window exists precisely because the cookie can rotate
  // again inside it. Announcing the event's own value would hand the backend the
  // value that has just gone out of date, which is the failure this path exists
  // to prevent.
  const stored = allCookies.find((c) => c.name === '__Secure-1PSIDTS');
  stored.value = 'psidts-2';

  const second = cookie('.google.com', '__Secure-3PSIDTS', 'psidts-3');
  allCookies.push(second);

  cookiesOnChanged({ removed: false, cause: 'explicit', cookie: { ...stored, value: 'psidts-1' } });

  // A change inside the window has to push the window out rather than leave the
  // first timer running. Without that the frame goes out mid-burst carrying half
  // the changes, and a caller has no way to tell a partial announcement from a
  // complete one — so this is asserted at the moment the first timer would have
  // fired had it not been reset.
  await pause(150);
  cookiesOnChanged({ removed: false, cause: 'explicit', cookie: { ...stored, value: 'psidts-2' } });
  await pause(200);
  assert.equal(
    rotationFrames().length, before,
    'the window must restart on each change, not expire on the first',
  );

  cookiesOnChanged({ removed: false, cause: 'explicit', cookie: second });
  await waitForRotation(before);

  assert.equal(rotationFrames().length, before + 1, 'a burst must produce exactly one frame');

  const params = rotationFrames()[before];
  assert.deepEqual(
    params.cookies.map((c) => c.name).sort(),
    ['__Secure-1PSIDTS', '__Secure-3PSIDTS'],
    'each changed name is announced once, not once per change',
  );
  assert.equal(
    params.cookies.find((c) => c.name === '__Secure-1PSIDTS').value,
    'psidts-2',
    'the announced value must be the one still on disk, not the one in the event',
  );
  assert.deepEqual(params.removed, []);

  // The shape has to be cookiejar.Cookie's, or the backend's merge is guessing at
  // which key holds the value.
  assert.deepEqual(Object.keys(params.cookies[0]).sort(), [
    'domain', 'expirationDate', 'hostOnly', 'httpOnly', 'name', 'path',
    'sameSite', 'secure', 'session', 'storeId', 'value',
  ]);
});

await check('only rotating cookies on the Flow domains are announced', async () => {
  const before = rotationFrames().length;

  // SID is essential to the backend but is not rotated — it is reissued when
  // someone signs in, and the next cookies.list carries it. `_ga` is analytics
  // and not on this surface at all. Neither belongs on this path.
  for (const name of ['SID', '_ga']) {
    cookiesOnChanged({
      removed: false, cause: 'explicit', cookie: allCookies.find((c) => c.name === name),
    });
  }

  // Right name, wrong domain: a rotating cookie set somewhere the backend never
  // reads from.
  cookiesOnChanged({
    removed: false, cause: 'explicit', cookie: cookie('example.com', '__Secure-1PSIDTS', 'elsewhere'),
  });

  // Longer than the debounce, so a frame would have had time to appear.
  await pause(600);
  assert.equal(rotationFrames().length, before, 'nothing outside the whitelist should be announced');
});

await check('a removed rotating cookie is announced by name', async () => {
  const before = rotationFrames().length;

  // Chrome reports a removal with the cookie as it last stood.
  const gone = allCookies.find((c) => c.name === '__Secure-1PSIDTS');
  cookiesOnChanged({ removed: true, cause: 'evicted', cookie: gone });

  await waitForRotation(before);
  const params = rotationFrames()[before];
  assert.deepEqual(params.cookies, [], 'a removed cookie has no value left to send');
  assert.deepEqual(
    params.removed, ['__Secure-1PSIDTS'],
    'a merge cannot tell "unchanged" from "removed" from values alone',
  );
});

await check('config.set round-trips and stays in the backend shape', async () => {
  const saved = await call('config.set', { patch: { eventBufferSize: 123 } });
  assert.equal(saved.eventBufferSize, 123);

  const read = await call('config.get');
  assert.equal(read.eventBufferSize, 123);
  // Keys the backend writes have to survive alongside the defaults.
  assert.deepEqual(read.targetUrlPrefixes, ['https://flow.google.com', 'https://labs.google']);

  // An unrelated key must not disturb the socket.
  assert.equal(ws.readyState, FakeWebSocket.OPEN, 'a non-credential change should not close the socket');
});

// Runs after this point nothing needs the socket: it is closed on purpose here,
// because a credential change has to make the extension reconnect.
await check('config.set with a new token replies, then reconnects', async () => {
  assert.equal(ws.readyState, FakeWebSocket.OPEN, 'the socket should be open before the change');

  const saved = await call('config.set', { patch: { bridgeToken: 'secret-token' } });

  // The reply has to arrive. Closing before sending would drop it — `send`
  // checks readyState and `close` moves it to CLOSING first — so the backend
  // would report a disconnected socket for a config that was applied.
  assert.equal(saved.bridgeToken, 'secret-token');

  // Then the socket closes so it can reconnect carrying the token. That is what
  // completes pairing: until the extension reconnects authenticated, the
  // backend never writes its pairing marker and the tokenless window stays open.
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(
    ws.readyState, FakeWebSocket.CLOSED,
    'a credential change must close the socket so it reconnects with the token',
  );
});

/* ------------------------------------------------------------------ *
 * The popup
 *
 * It carries two actions — an account-bundle export and opening Flow — and
 * deliberately no bridge-token field. Pairing is not a human step: the extension
 * dials tokenless on first run, the backend hands it a token over that
 * connection via `config.set`, and the extension persists it and reconnects with
 * it. The form for pasting one by hand was the fallback for that handover
 * failing, and it is gone.
 *
 * The first check is about that absence, and it is the only thing that would
 * notice the form creeping back. The rest drive the export, because the bundle
 * is a cross-language contract: the popup writes JSON that
 * `cookiejar.LoadBundleFile` decodes, and a field spelled the page's way rather
 * than the file's is invisible — it decodes to an empty string, not an error.
 *
 * These run after the socket tests because they read config through the popup's
 * own channel, which needs no socket.
 * ------------------------------------------------------------------ */

/**
 * A DOM just large enough to run the popup's own logic.
 *
 * It records which ids were asked for, because one of the things asserted below
 * is what the script does *not* reach for — and it can fire a click, so the
 * export itself is exercised rather than merely inspected.
 */
function makeDom() {
  const nodes = new Map();
  const listeners = new Map();

  const node = (id) => {
    if (!nodes.has(id)) {
      nodes.set(id, {
        id,
        textContent: '',
        className: '',
        title: '',
        value: '',
        disabled: false,
        addEventListener: (type, fn) => listeners.set(`${id}:${type}`, fn),
      });
    }
    return nodes.get(id);
  };

  return {
    node,
    ids: () => new Set(nodes.keys()),
    document: { getElementById: node },
    submit(id) {
      const handler = listeners.get(`${id}:submit`);
      if (!handler) throw new Error(`no submit handler was registered on ${id}`);
      return handler({ preventDefault() {} });
    },
    async click(id) {
      const handler = listeners.get(`${id}:click`);
      if (!handler) throw new Error(`no click handler was registered on ${id}`);
      await handler({ preventDefault() {} });
    },
  };
}

const dom = makeDom();
Object.defineProperty(globalThis, 'document', {
  value: dom.document, configurable: true, writable: true,
});

// The popup writes the export through the clipboard API. Chrome provides it, so
// the harness has to as well, or the handler throws before there is anything to
// inspect.
let clipboardText = null;
globalThis.navigator.clipboard = {
  writeText: async (text) => { clipboardText = text; },
};

await import(path.join(stage, 'popup.js'));
await new Promise((resolve) => setTimeout(resolve, 20));

/**
 * Ask the worker through the popup's own channel.
 *
 * Deliberately not `call`, which goes over the socket: the socket is closed by
 * the `config.set` case above, and the worker's `send` refuses to write on a
 * socket that is not open — so a socket call made here never gets a reply and
 * the whole run hangs rather than failing. The popup channel needs no socket,
 * which is why the popup still works when the backend is down.
 */
const askWorker = (op, params) =>
  chromeStub.runtime.sendMessage(params ? { op, params } : { op });

await check('the popup exposes no bridge-token surface', async () => {
  // Every id the script touched while rendering. A form that came back would
  // show up here, and so would a read of the stored token.
  const asked = dom.ids();
  for (const id of ['tokenForm', 'token', 'save', 'tokenState', 'settings']) {
    assert.ok(!asked.has(id), `the popup must not reach for #${id} any more`);
  }

  // And nothing can submit one, because no form registered a handler.
  assert.throws(
    () => dom.submit('tokenForm'),
    /no submit handler was registered on tokenForm/,
  );
});

await check('status reports the tab the bridge would act on', async () => {
  // Read by the popup's own first paint and by the export below. A value that is
  // only ever set as a side effect of some other op would leave both of them
  // describing a browser that is not there, and the export would name no project.
  const status = await askWorker('status');
  assert.equal(status.tabUrl, FLOW_URL);
  assert.equal(status.tabTitle, 'Flow');
});

await check('the popup copies a bundle cookiejar.LoadBundleFile can read', async () => {
  await dom.click('copy');
  await new Promise((resolve) => setTimeout(resolve, 20));

  assert.ok(clipboardText, 'nothing reached the clipboard');
  const bundle = JSON.parse(clipboardText);

  // The top-level keys are cookiejar.Bundle's JSON tags and nothing else: a field
  // the file does not have decodes to nothing, with no error anywhere.
  assert.deepEqual(
    Object.keys(bundle).sort(),
    ['at', 'cookies', 'fingerprint', 'fsid', 'project_id'],
  );

  // The project comes from the tab the browser is on, which is what the tab
  // reports — not the backend's answer, which is what it resolved at boot.
  assert.equal(bundle.project_id, 'abc123');
  assert.equal(bundle.at, 'at-value');
  assert.equal(bundle.fsid, '-56329636');

  // The identity is renamed on the way through: the page reports `brands` and
  // `platformFull`, the file spells those `sec_ch_ua` and `platform`. Writing the
  // page's names through unchanged is the failure this pins — every field would
  // come back unset after a decode, with nothing to point at it.
  assert.deepEqual(
    Object.keys(bundle.fingerprint).sort(),
    ['language', 'mobile', 'platform', 'sec_ch_ua', 'user_agent'],
  );
  assert.match(bundle.fingerprint.user_agent, /^Mozilla\/5\.0/);
  assert.equal(bundle.fingerprint.sec_ch_ua, '"Chromium";v="140", "Google Chrome";v="140"');
  assert.equal(bundle.fingerprint.platform, '"macOS"');
  assert.equal(bundle.fingerprint.language, 'en-US');
  assert.equal(bundle.fingerprint.mobile, '?0');

  // And the cookies are cookiejar.Cookie's tags, values included — this is the
  // credential, so a bundle that carried names without values would still parse.
  assert.ok(bundle.cookies.length > 0, 'the bundle must carry the credential');
  assert.deepEqual(Object.keys(bundle.cookies[0]).sort(), [
    'domain', 'expirationDate', 'hostOnly', 'httpOnly', 'name', 'path',
    'sameSite', 'secure', 'session', 'storeId', 'value',
  ]);
  assert.ok(bundle.cookies.some((c) => c.name === 'SAPISID'));
});

/* ------------------------------------------------------------------ *
 * Report
 * ------------------------------------------------------------------ */

const failed = results.filter((r) => !r.ok);
for (const r of results) {
  console.log(`${r.ok ? 'PASS' : 'FAIL'}  ${r.name}`);
  if (!r.ok) console.log(`      ${r.error}`);
}
console.log(`\n${results.length - failed.length}/${results.length} passed`);

fs.rmSync(stage, { recursive: true, force: true });
process.exit(failed.length === 0 ? 0 : 1);
