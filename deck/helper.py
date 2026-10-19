#!/usr/bin/env python3
"""flickr on a Steam Deck — the helper beside the QML client.

The QML runtime can draw documents and play a stream, and three things it
cannot do: carry a bearer on a picture fetch, stand at Keycloak's device door,
and read a gamepad. This process does those three and nothing else:

  * a PROXY on 127.0.0.1. Every address the client follows out of a document
    is followed through here, untouched, with the household's bearer and the
    profile's client_id cookie added. No address is composed on this side
    either: `/api/` is still the one the client knows by heart.
  * the SIGN-IN. A 401 from the root is a problem whose remedy names the door
    (docs/hypermedia.md, "The sign-in is a remedy like any other"). A browser
    walks through it; a box with a D-pad cannot type a password, so the door's
    redirect is read for the issuer and the OAuth device grant is used
    instead: a short code on the screen, approved on a phone.
  * the PAD. evdev gamepads become the same few words a remote sends —
    up/down/left/right/ok/back and a handful more — long-polled by the client.
  * the SCREEN, kept awake while something plays: nobody touches a pad
    during a film, so Steam's dimmer would otherwise come down on it.

Standard library only (evdev and dbus-python are on SteamOS already; without
evdev the pad is simply absent and the keyboard still works, without dbus the
screen dims as it always did).
"""
import argparse
import http.client
import json
import os
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HOP = {'connection', 'keep-alive', 'proxy-authenticate', 'proxy-authorization',
       'te', 'trailers', 'transfer-encoding', 'upgrade', 'host', 'content-length',
       'accept-encoding'}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


def http_json(url, data=None, headers=None, timeout=15):
    """POST a form (or GET) and answer (status, parsed body or None)."""
    body = urllib.parse.urlencode(data).encode() if data is not None else None
    req = urllib.request.Request(url, data=body, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b'null')
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read() or b'null')
        except ValueError:
            return e.code, None


class Auth:
    """The household's bearer: found, asked for at the device door, kept fresh."""

    def __init__(self, upstream, state_dir, client_id):
        self.upstream = upstream
        self.client_id = client_id
        self.path = os.path.join(state_dir, 'token.json')
        self.lock = threading.Lock()
        self.tok = self._load()
        self.gated = None       # None until the root has been asked
        self.pending = None     # {user_code, verification_uri, ...} while the code is up
        self.error = ''
        self.endpoints = None

    def _load(self):
        try:
            with open(self.path) as f:
                return json.load(f)
        except (OSError, ValueError):
            return {}

    def _save(self):
        fd = os.open(self.path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, 'w') as f:
            json.dump(self.tok, f)

    def _discover(self):
        """The issuer, read off the door the 401's remedy names."""
        if self.endpoints:
            return self.endpoints
        status, problem = http_json(self.upstream + '/api/')
        if status != 401:
            return None
        door = ((problem or {}).get('remedy') or {}).get('link', {}).get('href')
        if not door:
            raise RuntimeError('the server refused, and named no door to sign in at')
        opener = urllib.request.build_opener(NoRedirect)
        try:
            opener.open(urllib.parse.urljoin(self.upstream + '/', door), timeout=15)
            raise RuntimeError('the sign-in door did not lead to a provider')
        except urllib.error.HTTPError as e:
            loc = e.headers.get('Location', '')
        issuer = loc.split('/protocol/', 1)[0]
        if not issuer.startswith('http'):
            raise RuntimeError('the sign-in door did not lead to a provider')
        status, conf = http_json(issuer + '/.well-known/openid-configuration')
        if status != 200 or 'device_authorization_endpoint' not in (conf or {}):
            raise RuntimeError('the provider offers no device sign-in')
        self.endpoints = conf
        return conf

    def check(self):
        """Is there a gate, and are we through it?"""
        try:
            status, _ = http_json(self.upstream + '/api/', headers=self._header())
        except OSError as e:
            self.error = 'flickr cannot be reached: %s' % e
            return
        self.error = ''
        if status == 401:
            self.gated = True
            if self.tok and not self._refresh():
                self.tok = {}
        else:
            self.gated = bool(self.tok.get('access_token'))

    def _header(self):
        t = self.tok.get('access_token')
        return {'Authorization': 'Bearer ' + t} if t else {}

    def bearer(self):
        """The header to send, refreshed a minute before it would run out."""
        with self.lock:
            if self.tok.get('refresh_token') and time.time() > self.tok.get('expires_at', 0) - 60:
                self._refresh()
            return self._header()

    def _refresh(self):
        try:
            conf = self._discover()
            if not conf or not self.tok.get('refresh_token'):
                return False
            status, body = http_json(conf['token_endpoint'], {
                'grant_type': 'refresh_token', 'client_id': self.client_id,
                'refresh_token': self.tok['refresh_token']})
        except (OSError, RuntimeError):
            return bool(self.tok.get('access_token'))  # offline is not signed out
        if status != 200:
            self.tok = {}
            self._save()
            return False
        self._adopt(body)
        return True

    def _adopt(self, body):
        self.tok = {
            'access_token': body['access_token'],
            'refresh_token': body.get('refresh_token', self.tok.get('refresh_token')),
            'expires_at': time.time() + int(body.get('expires_in', 300)),
        }
        self._save()

    def signed_in(self):
        return self.gated is False or bool(self.tok.get('access_token'))

    def start(self):
        """Ask for a code and poll for its approval on a thread of its own."""
        with self.lock:
            if self.pending:
                return
            self.error = ''
            try:
                conf = self._discover()
                if conf is None:
                    self.gated = False
                    return
                status, body = None, None
                # offline_access keeps the Deck signed in past the realm's idle
                # timeout; a realm that does not grant it still signs in.
                for scope in ('openid profile offline_access', 'openid profile'):
                    status, body = http_json(conf['device_authorization_endpoint'],
                                             {'client_id': self.client_id, 'scope': scope})
                    if status == 200:
                        break
                if status != 200:
                    raise RuntimeError((body or {}).get('error_description') or
                                       'the provider refused a device sign-in (%s)' % status)
            except (OSError, RuntimeError) as e:
                self.error = str(e)
                return
            self.pending = body
        threading.Thread(target=self._poll, args=(conf, body), daemon=True).start()

    def _poll(self, conf, grant):
        wait = int(grant.get('interval', 5))
        until = time.time() + int(grant.get('expires_in', 600))
        while time.time() < until:
            time.sleep(wait)
            try:
                status, body = http_json(conf['token_endpoint'], {
                    'grant_type': 'urn:ietf:params:oauth:grant-type:device_code',
                    'client_id': self.client_id, 'device_code': grant['device_code']})
            except OSError:
                continue
            if status == 200:
                with self.lock:
                    self._adopt(body)
                    self.pending = None
                    self.gated = True
                return
            err = (body or {}).get('error')
            if err == 'slow_down':
                wait += 5
            elif err != 'authorization_pending':
                self.error = (body or {}).get('error_description') or err or 'sign-in failed'
                break
        else:
            self.error = 'the code ran out before it was approved'
        self.pending = None

    def sign_out(self):
        with self.lock:
            self.tok = {}
            self._save()
            self.pending = None

    def state(self):
        p = self.pending or {}
        return {
            'signed_in': self.signed_in(), 'gated': bool(self.gated), 'error': self.error,
            'pending': bool(self.pending), 'user_code': p.get('user_code', ''),
            'verification_uri': p.get('verification_uri', ''),
            'verification_uri_complete': p.get('verification_uri_complete', ''),
        }


class Awake:
    """The screen held awake while the player plays. In Game Mode Steam itself
    is org.freedesktop.ScreenSaver; an inhibit lasts until it is let go or
    this process leaves the bus, so a helper that dies cannot leave the
    screen on. A Steam that restarted under it is found again on the next
    call."""

    def __init__(self):
        self.lock = threading.Lock()
        self.cookie = None
        self.saver = None

    def _saver(self):
        if self.saver is None:
            import dbus
            bus = dbus.SessionBus()
            self.saver = dbus.Interface(
                bus.get_object('org.freedesktop.ScreenSaver', '/org/freedesktop/ScreenSaver'),
                'org.freedesktop.ScreenSaver')
        return self.saver

    def hold(self, on):
        with self.lock:
            for _ in range(2):
                try:
                    if on and self.cookie is None:
                        self.cookie = int(self._saver().Inhibit('flickr', 'Playing'))
                    elif not on and self.cookie is not None:
                        cookie, self.cookie = self.cookie, None
                        self._saver().UnInhibit(cookie)
                    break
                except Exception:
                    # No dbus, no saver, or a Steam that is not the one the
                    # cookie came from: that one has already let go of it.
                    self.saver, self.cookie = None, None
            return self.cookie is not None


class Pad:
    """Gamepads as the words a remote sends. Held directions repeat."""

    FIRST, AGAIN, DEAD = 0.40, 0.11, 0.6

    def __init__(self):
        self.events = []         # (seq, word)
        self.seq = 0
        self.cond = threading.Condition()
        self.held = {}           # word -> next repeat time
        self.open = set()

    def emit(self, word):
        with self.cond:
            self.seq += 1
            self.events.append((self.seq, word))
            del self.events[:-64]
            self.cond.notify_all()

    def since(self, seq, timeout=25):
        with self.cond:
            if seq > self.seq:   # the helper restarted under a running client
                seq = 0
            if not any(s > seq for s, _ in self.events):
                self.cond.wait(timeout)
            return self.seq, [w for s, w in self.events if s > seq]

    def run(self):
        try:
            import evdev
        except ImportError:
            return
        threading.Thread(target=self._repeat, daemon=True).start()
        while True:
            for path in evdev.list_devices():
                if path in self.open:
                    continue
                try:
                    dev = evdev.InputDevice(path)
                    keys = dev.capabilities().get(evdev.ecodes.EV_KEY, [])
                except OSError:
                    continue
                if evdev.ecodes.BTN_SOUTH in keys:
                    self.open.add(path)
                    threading.Thread(target=self._read, args=(evdev, dev), daemon=True).start()
                else:
                    dev.close()
            time.sleep(3)

    def _hold(self, word, down):
        if down:
            if word not in self.held:
                self.held[word] = time.time() + self.FIRST
                self.emit(word)
        else:
            self.held.pop(word, None)

    def _repeat(self):
        while True:
            time.sleep(0.02)
            now = time.time()
            for word, at in list(self.held.items()):
                if now >= at:
                    self.held[word] = now + self.AGAIN
                    self.emit(word)

    def _read(self, evdev, dev):
        e = evdev.ecodes
        buttons = {e.BTN_SOUTH: 'ok', e.BTN_EAST: 'back', e.BTN_NORTH: 'play',
                   e.BTN_WEST: 'search', e.BTN_TL: 'prev', e.BTN_TR: 'next',
                   e.BTN_START: 'menu', e.BTN_SELECT: 'subs'}
        dpad = {getattr(e, 'BTN_DPAD_UP', -1): 'up', getattr(e, 'BTN_DPAD_DOWN', -1): 'down',
                getattr(e, 'BTN_DPAD_LEFT', -1): 'left', getattr(e, 'BTN_DPAD_RIGHT', -1): 'right'}
        span = {}
        for code, info in dev.capabilities().get(e.EV_ABS, []):
            span[code] = (info.min, info.max)

        def axis(code, value, neg, pos, dead):
            lo, hi = span.get(code, (-1, 1))
            mid, half = (lo + hi) / 2, max((hi - lo) / 2, 1)
            v = (value - mid) / half
            self._hold(neg, v < -dead)
            self._hold(pos, v > dead)

        try:
            for ev in dev.read_loop():
                if ev.type == e.EV_KEY:
                    if ev.code in dpad:
                        self._hold(dpad[ev.code], ev.value != 0)
                    elif ev.code in buttons and ev.value == 1:
                        self.emit(buttons[ev.code])
                elif ev.type == e.EV_ABS:
                    if ev.code == e.ABS_HAT0X:
                        axis(ev.code, ev.value, 'left', 'right', 0.5)
                    elif ev.code == e.ABS_HAT0Y:
                        axis(ev.code, ev.value, 'up', 'down', 0.5)
                    elif ev.code == e.ABS_X:
                        axis(ev.code, ev.value, 'left', 'right', self.DEAD)
                    elif ev.code == e.ABS_Y:
                        axis(ev.code, ev.value, 'up', 'down', self.DEAD)
        except OSError:
            pass
        finally:
            self.open.discard(dev.path)
            for word in ('up', 'down', 'left', 'right'):
                self.held.pop(word, None)


class Handler(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    auth = None
    pad = None
    awake = None
    upstream = ''
    profile = ''
    profile_path = ''

    def log_message(self, *a):
        pass

    def _json(self, obj, status=200):
        body = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _body(self):
        n = int(self.headers.get('Content-Length') or 0)
        return self.rfile.read(n) if n else b''

    def _own(self, method):
        """The helper's own few addresses, under /_deck/."""
        url = urllib.parse.urlsplit(self.path)
        name = url.path[len('/_deck/'):]
        cls = type(self)
        if name == 'state':
            if cls.auth.gated is None:
                cls.auth.check()
            return self._json(self._state())
        if name == 'pad':
            q = urllib.parse.parse_qs(url.query)
            seq, words = cls.pad.since(int(q.get('since', ['0'])[0]))
            return self._json({'seq': seq, 'events': words})
        if method != 'POST':
            return self._json({'error': 'no such address'}, 404)
        body = self._body()
        if name == 'signin':
            cls.auth.start()
        elif name == 'signout':
            cls.auth.sign_out()
            cls.auth.check()
        elif name == 'awake':
            cls.awake.hold(bool(json.loads(body or b'{}').get('on')))
        elif name == 'profile':
            cls.profile = (json.loads(body or b'{}').get('name') or '').strip()
            with open(cls.profile_path, 'w') as f:
                f.write(cls.profile)
        else:
            return self._json({'error': 'no such address'}, 404)
        self._json(self._state())

    def _state(self):
        cls = type(self)
        s = cls.auth.state()
        s.update(profile=cls.profile, upstream=cls.upstream, pad_seq=cls.pad.seq,
                 awake=cls.awake.cookie is not None)
        return s

    def _proxy(self, method):
        cls = type(self)
        body = self._body()
        up = urllib.parse.urlsplit(cls.upstream)
        Conn = http.client.HTTPSConnection if up.scheme == 'https' else http.client.HTTPConnection
        headers = {k: v for k, v in self.headers.items() if k.lower() not in HOP and k.lower() != 'cookie'}
        headers.update(cls.auth.bearer())
        if cls.profile:
            headers['Cookie'] = 'client_id=' + urllib.parse.quote(cls.profile)
        try:
            conn = Conn(up.netloc, timeout=60)
            conn.request(method, self.path, body=body or None, headers=headers)
            r = conn.getresponse()
            data = r.read()
        except OSError as e:
            return self._json({'type': 'unreachable', 'title': 'flickr cannot be reached',
                               'status': 502, 'detail': str(e)}, 502)
        if r.status == 401:
            cls.auth.check()
        self.send_response(r.status)
        for k, v in r.getheaders():
            if k.lower() not in HOP and k.lower() != 'set-cookie':
                self.send_header(k, v)
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        if method != 'HEAD':
            self.wfile.write(data)
        conn.close()

    def _any(self, method):
        try:
            if self.path.startswith('/_deck/'):
                self._own(method)
            else:
                self._proxy(method)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def do_GET(self): self._any('GET')
    def do_POST(self): self._any('POST')
    def do_PUT(self): self._any('PUT')
    def do_DELETE(self): self._any('DELETE')
    def do_HEAD(self): self._any('HEAD')


def main():
    ap = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    ap.add_argument('--upstream', default=os.environ.get('FLICKR_URL', 'https://stream.kopsa.info'))
    ap.add_argument('--client-id', default=os.environ.get('FLICKR_DECK_CLIENT', 'flickr-deck'))
    ap.add_argument('--port', type=int, default=0)
    ap.add_argument('--state-dir', default=os.path.join(
        os.environ.get('XDG_CONFIG_HOME', os.path.expanduser('~/.config')), 'flickr-deck'))
    ap.add_argument('--no-pad', action='store_true')
    args = ap.parse_args()

    os.makedirs(args.state_dir, mode=0o700, exist_ok=True)
    Handler.upstream = args.upstream.rstrip('/')
    Handler.auth = Auth(Handler.upstream, args.state_dir, args.client_id)
    Handler.pad = Pad()
    Handler.awake = Awake()
    # Who is on the sofa outlives the app: the last profile picked is the one
    # the next launch opens as.
    Handler.profile_path = os.path.join(args.state_dir, 'profile')
    try:
        with open(Handler.profile_path) as f:
            Handler.profile = f.read().strip()
    except OSError:
        pass
    if not args.no_pad:
        threading.Thread(target=Handler.pad.run, daemon=True).start()

    srv = ThreadingHTTPServer(('127.0.0.1', args.port), Handler)
    srv.daemon_threads = True
    # The launcher reads this one line for the port, then starts the client.
    print(srv.server_address[1], flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == '__main__':
    sys.exit(main())
