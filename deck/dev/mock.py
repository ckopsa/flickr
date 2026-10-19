#!/usr/bin/env python3
"""A flickr that is only its goldens — for drawing the Deck client with no
server, no Go and no sign-in.

Every top-level document in cmd/server/testdata/hyper/ is served at its own
`self`. A work or item the goldens do not hold answers the nearest one of its
kind, so every tile opens something. A play answers the session golden with
its `url` pointed at a clip ffmpeg makes on first run; pictures are one
generated card. Writes are accepted and forgotten.

    python3 deck/dev/mock.py --port 8099
"""
import argparse
import glob
import json
import os
import subprocess
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
GOLDENS = os.path.join(HERE, '..', '..', 'cmd', 'server', 'testdata', 'hyper')
CACHE = os.path.join(os.environ.get('XDG_CACHE_HOME', os.path.expanduser('~/.cache')), 'flickr-deck-mock')

docs = {}
for path in glob.glob(os.path.join(GOLDENS, '*.json')):
    with open(path) as f:
        d = json.load(f)
    if isinstance(d, dict) and d.get('self'):
        docs[d['self'].split('?')[0]] = d
        docs['golden:' + os.path.basename(path)[:-5]] = d


def asset(name, args):
    os.makedirs(CACHE, exist_ok=True)
    out = os.path.join(CACHE, name)
    if not os.path.exists(out):
        subprocess.run(['ffmpeg', '-v', 'error', '-y'] + args + [out], check=True)
    return out


def clip():
    return asset('clip.mp4', ['-f', 'lavfi', '-i', 'testsrc2=size=1280x720:rate=30:duration=120',
                              '-f', 'lavfi', '-i', 'sine=frequency=330:duration=120',
                              '-c:v', 'libx264', '-preset', 'ultrafast', '-pix_fmt', 'yuv420p',
                              '-c:a', 'aac', '-shortest'])


def card():
    return asset('card.png', ['-f', 'lavfi', '-i', 'gradients=size=400x600:c0=0x29406b:c1=0x8db4f0',
                              '-frames:v', '1'])


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def send(self, status, body, ctype='application/json'):
        if not isinstance(body, bytes):
            body = json.dumps(body).encode()
        self.send_response(status)
        self.send_header('Content-Type', ctype)
        self.send_header('Content-Length', str(len(body)))
        self.send_header('Accept-Ranges', 'bytes')
        self.end_headers()
        self.wfile.write(body)

    def file(self, path, ctype):
        size = os.path.getsize(path)
        start, end = 0, size - 1
        rng = self.headers.get('Range')
        if rng and rng.startswith('bytes='):
            a, _, b = rng[6:].partition('-')
            start = int(a or 0)
            end = int(b) if b else end
        with open(path, 'rb') as f:
            f.seek(start)
            data = f.read(end - start + 1)
        self.send_response(206 if rng else 200)
        self.send_header('Content-Type', ctype)
        self.send_header('Content-Length', str(len(data)))
        self.send_header('Accept-Ranges', 'bytes')
        if rng:
            self.send_header('Content-Range', 'bytes %d-%d/%d' % (start, end, size))
        self.end_headers()
        try:
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def do_GET(self):
        path = urllib.parse.urlsplit(self.path).path
        if path == '/media/clip.mp4':
            return self.file(clip(), 'video/mp4')
        if path.rsplit('/', 1)[-1] in ('poster', 'cover', 'artwork', 'backdrop', 'still'):
            return self.file(card(), 'image/png')
        if path == '/api/users':
            return self.send(200, [{'name': 'chris', 'avatar': '🦊'},
                                   {'name': 'robin', 'avatar': '🐸', 'kid': True}])
        if path in docs:
            return self.send(200, docs[path])
        if path.startswith('/api/works/'):
            return self.send(200, docs['golden:work-show'])
        if path.startswith('/api/artists/'):
            return self.send(200, docs['golden:artist'])
        if path.startswith('/api/items/'):
            return self.send(200, docs['golden:item-film'])
        self.send(404, {'type': 'not-found', 'title': 'Not found', 'status': 404,
                        'detail': 'the mock holds no document at ' + path})

    def do_POST(self):
        n = int(self.headers.get('Content-Length') or 0)
        body = self.rfile.read(n) if n else b''
        path = urllib.parse.urlsplit(self.path).path
        if path.endswith('/play') or path.endswith('/next'):
            s = dict(docs['golden:session-play'])
            s['url'] = '/media/clip.mp4'
            s.pop('passage', None)
            s['skips'] = [{'kind': 'intro', 'start': 5, 'end': 20, 'label': 'Skip intro'}]
            try:
                s['seek_seconds'] = json.loads(body or b'{}').get('seek_seconds') or 0
            except ValueError:
                s['seek_seconds'] = 0
            return self.send(200, s)
        self.send(200, {})

    def do_DELETE(self):
        self.send(200, {})


if __name__ == '__main__':
    ap = argparse.ArgumentParser()
    ap.add_argument('--port', type=int, default=8099)
    args = ap.parse_args()
    ThreadingHTTPServer(('127.0.0.1', args.port), Handler).serve_forever()
