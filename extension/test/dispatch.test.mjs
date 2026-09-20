/**
 * Dispatch harness for the Flow Go Bridge extension.
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

// A realistic wrb.fr frame: the payload is a JSON *string* inside the frame, so
// the image arrives with its quotes escaped. That escaping is the whole reason
// the extraction has to parse rather than scrape.
//
//   frame line   [["wrb.fr","SPrCad","<payload>"]]
//   <payload>    [[null,[null,null,"<image>"]]]      (quotes escaped in the line)
const upscaleFrame = (payload) =>
  `)]}'\n\n[["wrb.fr","SPrCad","[[null,[null,null,\\"${payload}\\"]]]"]]\n`;

const PUBLIC_ERROR_FRAME = `)]}'\n\n[["wrb.fr","SPrCad","[[null,[null,null,null,null,[[null,null,[\\"PUBLIC_ERROR_UNUSUAL_ACTIVITY\\"]]]]]]"]]\n`;

let fetchReply = () => upscaleFrame('A'.repeat(600));
let lastFetch = null;

// Guard the fixture itself. A malformed frame here reads as an extension bug —
// which is exactly how this harness first failed, and it cost more time than the
// real defect did.
{
  const line = upscaleFrame('X'.repeat(600))
    .split('\n')
    .find((l) => l.trim().indexOf('[["wrb.fr"') === 0);
  assert.ok(line, 'the fixture should contain a wrb.fr frame');
  const outer = JSON.parse(line);
  const inner = JSON.parse(outer[0][2]);
  assert.equal(inner[0][1][2], 'X'.repeat(600), 'the fixture should round-trip the image');
}

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
  fetch: {
    value: async (url, init) => {
      lastFetch = { url, init };
      return { text: async () => fetchReply() };
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
    'flow.fingerprint', 'flow.navigate', 'flow.projects', 'flow.captcha', 'flow.upscale',
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

await check('flow.upscale parses the frame and matches FlowUpscaleResult', async () => {
  fetchReply = () => upscaleFrame('A'.repeat(600));
  const up = await call('flow.upscale', {
    projectId: 'abc123', mediaId: 'media-1', contentId: 'content-1',
    resolution: 2, buildLabel: 'bl-1',
  });
  assert.equal(typeof up.data, 'string');
  assert.equal(up.data.length, 600);

  // The request shape, which the app is strict about.
  assert.ok(lastFetch, 'the page should have made the request');

  // Same-origin, and it has to be. The whole reason the page makes this call is
  // that the identical request from Go is refused on its TLS fingerprint, so
  // pointing it anywhere but the page's own origin would defeat the point.
  assert.ok(
    lastFetch.url.startsWith('/_/AiSandboxAngularFrontend/data/batchexecute'),
    `the upscale must go to the page's own origin, got ${lastFetch.url}`,
  );

  const url = decodeURIComponent(lastFetch.url);
  // The project, and only the project. An earlier version appended the editor
  // route with the media id in it; the app's own request does not.
  assert.match(url, /source-path=\/project\/abc123(&|$)/);
  assert.doesNotMatch(url, /\/edit\//, 'the source path must not name the editor route');
  assert.match(lastFetch.url, /rpcids=SPrCad/);
  assert.match(lastFetch.url, /[?&]hl=en-US/);
  assert.match(lastFetch.url, /[?&]rt=c/);
  assert.match(lastFetch.url, /[?&]bl=bl-1/);

  assert.equal(lastFetch.init.method, 'POST');
  assert.equal(lastFetch.init.credentials, 'include');
  assert.equal(lastFetch.init.headers['X-Same-Domain'], '1');
  assert.equal(lastFetch.init.headers['content-type'], 'application/x-www-form-urlencoded;charset=UTF-8');

  // The body is the half of the request a URL cannot show, and a wrong argument
  // shape comes back as an opaque upstream error rather than a clear one. The
  // layout is the app's own: [media id, resolution selector, context block],
  // and the context block carries tool id 22, the project, then the captcha
  // pair — none of which is guessable from the outside.
  const body = new URLSearchParams(lastFetch.init.body);
  assert.equal(body.get('at'), 'at-value', 'the page anti-CSRF token must be carried');

  const frame = JSON.parse(body.get('f.req'));
  assert.equal(frame.length, 1);
  assert.equal(frame[0].length, 1);
  const [rpcid, argJSON, absent, kind] = frame[0][0];
  assert.equal(rpcid, 'SPrCad');
  assert.equal(absent, null);
  assert.equal(kind, 'generic');

  const arg = JSON.parse(argJSON);
  // The media id, not the content id. This is the opposite of the generation
  // calls, which take content ids — and getting it backwards returns a null
  // payload rather than an error, which is how it presented for a long time.
  assert.equal(arg[0], 'media-1', 'argument 0 is the media id');
  assert.notEqual(arg[0], 'content-1', 'the content id must not be sent here');
  assert.equal(arg[1], 2, 'argument 1 is the resolution selector');
  assert.equal(arg[2].length, 11, 'argument 2 is the context block');
  assert.equal(arg[2][1], 22, 'the context block carries the tool id the app always sends');
  assert.equal(arg[2][5], 'abc123', 'the context block carries the project id');
  assert.match(arg[2][10][0], /^token-for-IMAGE_GENERATION-\d+$/, 'the context block carries a fresh captcha token');
  assert.equal(arg[2][10][1], 1);
});

await check('flow.upscale reports an upstream PUBLIC_ERROR', async () => {
  fetchReply = () => PUBLIC_ERROR_FRAME;
  const up = await call('flow.upscale', { projectId: 'abc123', mediaId: 'm1', contentId: 'c1' });
  assert.equal(up.data, undefined);
  assert.equal(up.error, 'PUBLIC_ERROR_UNUSUAL_ACTIVITY');
  fetchReply = () => upscaleFrame('A'.repeat(600));
});

await check('flow.upscale refuses a response with no SPrCad frame', async () => {
  fetchReply = () => '<html>nope</html>';
  const up = await call('flow.upscale', { projectId: 'abc123', mediaId: 'm1', contentId: 'c1' });
  assert.equal(up.data, undefined);
  assert.match(String(up.error), /no SPrCad frame/);
  fetchReply = () => upscaleFrame('A'.repeat(600));
});

await check('flow.upscale needs a project id', async () => {
  const message = await callExpectingError('flow.upscale', { contentId: 'c1' });
  assert.match(String(message), /needs a projectId/);
});

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
 * It is the recovery route. An extension that has lost its token cannot connect,
 * so the backend can never tell it anything — the connection is refused before a
 * message could be sent — and the only way back is a human pasting the token in
 * here. That is exactly the moment it has to work, so it is driven rather than
 * eyeballed.
 *
 * These run after the socket tests because they read config through the popup's
 * own channel, which needs no socket.
 * ------------------------------------------------------------------ */

/** A DOM just large enough to run the popup's own logic. */
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
    document: { getElementById: node },
    submit(id) {
      const handler = listeners.get(`${id}:submit`);
      if (!handler) throw new Error(`no submit handler was registered on ${id}`);
      return handler({ preventDefault() {} });
    },
  };
}

const dom = makeDom();
Object.defineProperty(globalThis, 'document', {
  value: dom.document, configurable: true, writable: true,
});

/** Read the worker's config the way the popup does. */
const workerConfig = () => chromeStub.runtime.sendMessage({ op: 'config.get' });

await import(path.join(stage, 'popup.js'));
await new Promise((resolve) => setTimeout(resolve, 20));

await check('the popup reports the stored token without revealing it', async () => {
  const shown = dom.node('tokenState').textContent;
  assert.match(shown, /^\(set \(\d+ chars\)\)$/, `unexpected token state: ${shown}`);
  // The secret itself must never reach the UI.
  assert.ok(!shown.includes('secret-token'), 'the popup must not print the token');
});

await check('the popup refuses an empty token', async () => {
  dom.node('token').value = '   ';
  await dom.submit('tokenForm');

  assert.match(dom.node('result').textContent, /Paste the token first/);
  const read = await workerConfig();
  assert.equal(read.bridgeToken, 'secret-token', 'an empty submit must not change the token');
});

await check('the popup hands a pasted token to the worker', async () => {
  dom.node('token').value = 'a-freshly-pasted-token';
  await dom.submit('tokenForm');

  const read = await workerConfig();
  assert.equal(read.bridgeToken, 'a-freshly-pasted-token');
  // Cleared, so the secret is not left sitting on screen.
  assert.equal(dom.node('token').value, '');
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
