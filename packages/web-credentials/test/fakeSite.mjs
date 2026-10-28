// A local fake site the tests sign in to. No real account, no network beyond loopback. Two hosts:
// the primary (127.0.0.1) and an "other" host (127.0.0.2) a sign-in or check can wrongly land on.
// It has: a password form, a two-step (TOTP) step, a token API, a session that can be made to
// expire, a CAPTCHA page, a page that redirects off-host, and a password it keeps rejecting
// (for the lockout guard).
import { createServer } from 'node:http';
import { totp } from '../src/totp.mjs';

export const PASSWORD = 'correct horse';
export const USERNAME = 'admin@example.com';
export const TOTP_SECRET = 'JBSWY3DPEHPK3PXP';
export const API_TOKEN = 'tok_example_123456';

function page(body) {
  return `<!doctype html><html><head><title>Fake</title></head><body>${body}</body></html>`;
}

function cookies(req) {
  const out = {};
  for (const part of (req.headers.cookie || '').split(';')) {
    const [k, v] = part.split('=');
    if (k) out[k.trim()] = (v || '').trim();
  }
  return out;
}

// opts: { captcha, offsite, rejectPassword } flip the awkward behaviours on.
export function startFakeSite(opts = {}) {
  const bindHost = opts.host || '127.0.0.1';
  const sessions = new Set(); // live session tokens
  let counter = 0;

  const handler = (req, res) => {
    const url = new URL(req.url, `http://${req.headers.host}`);
    const send = (status, body, headers = {}) => {
      res.writeHead(status, { 'content-type': 'text/html; charset=utf-8', ...headers });
      res.end(body);
    };

    // The token API: a bearer header signed in, anything else 401.
    if (url.pathname === '/api/me') {
      const auth = req.headers.authorization || '';
      if (auth === `Bearer ${API_TOKEN}`) return send(200, page('<h1>Me</h1>ok signed in'));
      return send(401, page('<h1>Unauthorized</h1>'));
    }

    // The signed-in check page.
    if (url.pathname === '/dashboard') {
      const c = cookies(req);
      if (c.sess && sessions.has(c.sess)) return send(200, page('<h1>Dashboard</h1><p id="who">Signed in</p>'));
      return send(302, '', { location: '/login' });
    }

    if (url.pathname === '/captcha-login') {
      return send(200, page('<h1>Sign in</h1><div class="g-recaptcha">Please prove you are human (captcha)</div>'));
    }

    if (url.pathname === '/offsite-login') {
      return send(302, '', { location: (opts.offsiteOrigin || `http://127.0.0.2:${url.port}`) + '/elsewhere' });
    }

    if (url.pathname === '/elsewhere') {
      return send(200, page('<h1>Another host</h1>'));
    }

    // The password form and its POST.
    if (url.pathname === '/login') {
      if (req.method !== 'POST') {
        const captcha = opts.captcha ? '<div class="h-captcha">captcha</div>' : '';
        return send(200, page(`<h1>Sign in</h1>${captcha}<form method="post" action="/login">
          <input type="email" name="email" />
          <input type="password" name="password" />
          <button type="submit">Log in</button></form>`));
      }
      return readBody(req, (form) => {
        if (opts.rejectPassword || form.password !== PASSWORD || form.email !== USERNAME) {
          return send(200, page('<h1>Sign in</h1><p class="error">Wrong username or password</p><form method="post" action="/login"><input type="email" name="email"/><input type="password" name="password"/><button type="submit">Log in</button></form>'));
        }
        if (opts.totp) {
          const pending = 'pending' + ++counter;
          sessions.add('half:' + pending);
          return send(302, '', { location: '/totp', 'set-cookie': `half=${pending}; Path=/` });
        }
        const token = 'sess' + ++counter;
        sessions.add(token);
        return send(302, '', { location: '/dashboard', 'set-cookie': `sess=${token}; Path=/` });
      });
    }

    if (url.pathname === '/totp') {
      if (req.method !== 'POST') {
        return send(200, page('<h1>Two-step</h1><form method="post" action="/totp"><input autocomplete="one-time-code" name="code"/><button type="submit">Verify</button></form>'));
      }
      return readBody(req, (form) => {
        const expected = totp(TOTP_SECRET);
        if (form.code !== expected) {
          return send(200, page('<h1>Two-step</h1><p class="error">Wrong code</p><form method="post" action="/totp"><input autocomplete="one-time-code" name="code"/><button type="submit">Verify</button></form>'));
        }
        const token = 'sess' + ++counter;
        sessions.add(token);
        return send(302, '', { location: '/dashboard', 'set-cookie': `sess=${token}; Path=/` });
      });
    }

    if (url.pathname === '/') return send(200, page('<h1>Home</h1>'));
    return send(404, page('<h1>Not found</h1>'));
  };

  const server = createServer(handler);
  return new Promise((resolve) => {
    server.listen(0, bindHost, () => {
      const port = server.address().port;
      resolve({
        host: bindHost,
        port,
        origin: `http://${bindHost}:${port}`,
        // A valid storageState for the interactive method: a known live session cookie.
        liveSession(host = '127.0.0.1') {
          const token = 'sess-live-' + ++counter;
          sessions.add(token);
          return { cookies: [{ name: 'sess', value: token, domain: host, path: '/', expires: Math.floor(Date.now() / 1000) + 3600, httpOnly: false, secure: false, sameSite: 'Lax' }], origins: [] };
        },
        deadSession(host = '127.0.0.1') {
          return { cookies: [{ name: 'sess', value: 'gone', domain: host, path: '/', expires: -1, httpOnly: false, secure: false, sameSite: 'Lax' }], origins: [] };
        },
        close: () => new Promise((r) => server.close(r)),
      });
    });
  });
}

function readBody(req, cb) {
  let data = '';
  req.on('data', (c) => (data += c));
  req.on('end', () => {
    const form = {};
    for (const pair of data.split('&')) {
      const [k, v] = pair.split('=');
      if (k) form[decodeURIComponent(k)] = decodeURIComponent((v || '').replace(/\+/g, ' '));
    }
    cb(form);
  });
}
