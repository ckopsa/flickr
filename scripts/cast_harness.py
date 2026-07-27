#!/usr/bin/env python3
"""Integration harness: drive a real Chromecast against the flickr server.

Bypasses the browser sender entirely — discovers the device via mDNS,
asks the server for a play decision using the device's capability
manifest, sends the resulting URL with pychromecast, then watches the
receiver's media status channel and reports what actually happened.

Usage:
  .venv/bin/python scripts/cast_harness.py                 # discover, pick first device
  .venv/bin/python scripts/cast_harness.py --device Ultra  # match by name/model
  .venv/bin/python scripts/cast_harness.py --item 42 --seek 300 --watch 30
"""
import argparse
import json
import sys
import time
import urllib.request

import os
SERVER = os.environ.get("FLICKR_SERVER", "http://localhost:8099")

# Conservative generic cast-device manifest: TS segments only (some
# Android-TV receivers reject fMP4 — the server re-encodes hevc as needed).
DEVICE_CAPS = {
    "schema_version": 1,
    "containers": ["mp4", "webm"],
    "video_codecs": ["h264", "hevc", "vp9"],
    "audio_codecs": ["aac", "mp3", "opus", "flac", "vorbis", "ac3", "eac3"],
    "max_width": 3840, "max_height": 2160,
    "supports_hdr": ["hdr10", "dolbyvision"],
    "max_audio_channels": 6,
    "hls_segment_formats": ["ts"],
    "hls_audio_codecs": ["aac"],
}


def api(path, body=None, method=None):
    req = urllib.request.Request(SERVER + path,
        data=json.dumps(body).encode() if body else None,
        headers={"Content-Type": "application/json"}, method=method)
    return json.load(urllib.request.urlopen(req))


def pick_item(item_id):
    items = api("/api/items")
    if item_id:
        return next(i for i in items if i["id"] == item_id)
    return next(i for i in items if i["media_info"] and i["media_info"]["duration_seconds"] > 900)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--device", help="substring of friendly name or model")
    ap.add_argument("--item", type=int, help="library item id")
    ap.add_argument("--url", help="cast this URL directly (control test), skipping the server")
    ap.add_argument("--segfmt", default="fmp4", choices=["fmp4", "ts", "none"],
                    help="HLS segment format hint sent to the receiver")
    ap.add_argument("--seek", type=float, default=0)
    ap.add_argument("--watch", type=float, default=25, help="seconds to observe playback")
    ap.add_argument("--keep", action="store_true", help="leave it playing")
    args = ap.parse_args()

    import pychromecast

    # mDNS is flaky; retry discovery until the target shows up.
    cast = browser = None
    for attempt in range(3):
        print(f"== discovering cast devices (mDNS, attempt {attempt + 1})…")
        chromecasts, browser = pychromecast.get_chromecasts(timeout=10)
        for cc in chromecasts:
            ci = cc.cast_info
            print(f"   found: {ci.friendly_name!r} model={ci.model_name!r} at {ci.host}:{ci.port}")
        if args.device:
            cast = next((cc for cc in chromecasts
                         if args.device.lower() in cc.cast_info.friendly_name.lower()
                         or args.device.lower() in cc.cast_info.model_name.lower()), None)
        else:
            cast = chromecasts[0] if chromecasts else None
        if cast:
            break
        pychromecast.discovery.stop_discovery(browser)
    if cast is None:
        print(f"!! no device matching {args.device!r}" if args.device else "!! no cast devices found")
        sys.exit(2)

    ci = cast.cast_info
    print(f"== using {ci.friendly_name!r} ({ci.model_name}) at {ci.host}")
    cast.wait(timeout=15)
    print(f"   connected. status: app={cast.status.display_name!r} volume={cast.status.volume_level:.2f}")
    # A stale receiver session from an earlier failed cast can wedge loads;
    # start from a clean slate.
    if cast.app_id and not cast.is_idle:
        print("   quitting stale receiver app first…")
        cast.quit_app()
        time.sleep(3)

    session_id = None
    if args.url:
        url = args.url
        is_hls = ".m3u8" in url
        title = "control test"
    else:
        base_url = api("/api/system")["base_url"]
        item = pick_item(args.item)
        mi = item["media_info"]
        print(f"== item {item['id']}: {item['object_key']}")
        print(f"   {mi['video_codec']}/{mi['audio_codec']} {mi['width']}x{mi['height']} {mi['duration_seconds']:.0f}s")

        resp = api(f"/api/items/{item['id']}/play",
                   {"capabilities": DEVICE_CAPS, "client_id": "cast-harness", "seek_seconds": args.seek})
        decision = resp["decision"]
        print(f"== decision: {decision['method']} — {decision['trace'][-1]['detail']}")
        if resp.get("video_encoder"):
            print(f"   video encoder: {resp['video_encoder']}")
        session_id = resp.get("session_id")
        url = resp["url"]
        if url.startswith("/"):
            url = base_url + url
        is_hls = decision["method"] == "transcode"
        args.segfmt = (decision.get("target") or {}).get("segment_format", "ts")
        title = item["object_key"].rsplit("/", 1)[-1]

    content_type = "application/x-mpegurl" if is_hls else "video/mp4"
    print(f"== casting {content_type}: {url[:100]}…" if len(url) > 100 else f"== casting {content_type}: {url}")

    mc = cast.media_controller
    # BUFFERED (not the pychromecast default LIVE), and for HLS declare the
    # fmp4 segment format — the receiver otherwise assumes MPEG-TS.
    hints = None
    if is_hls and args.segfmt != "none":
        hints = {"hlsSegmentFormat": args.segfmt, "hlsVideoSegmentFormat": args.segfmt}
    # current_time=0 pins playback to the session start — otherwise the
    # receiver treats a growing (no-ENDLIST) playlist as live and jumps to
    # the live edge. Server-side seeks already position the session itself.
    mc.play_media(url, content_type, title=title, stream_type="BUFFERED",
                  media_info=hints, current_time=0 if is_hls else None)
    try:
        mc.block_until_active(timeout=15)
    except Exception as e:
        print(f"!! media channel never became active: {e}")

    print(f"== watching receiver status for {args.watch:.0f}s…")
    last = None
    ok = False
    deadline = time.time() + args.watch
    while time.time() < deadline:
        st = mc.status
        state = (st.player_state, round(st.current_time or 0, 1))
        if state != last:
            extra = ""
            if st.player_state == "IDLE":
                extra = f" idle_reason={st.idle_reason}"
            print(f"   [{time.strftime('%H:%M:%S')}] {st.player_state} t={st.current_time or 0:.1f}"
                  f" content={str(st.content_id)[:60]!r}{extra}")
            last = state
        if st.player_state == "PLAYING" and (st.current_time or 0) > 1:
            ok = True
        if st.player_state == "IDLE" and st.idle_reason == "ERROR":
            print("!! receiver reported ERROR — playback failed on-device")
            break
        time.sleep(1)
        try:
            mc.update_status()  # currentTime is only fresh on request
        except Exception:
            pass

    print(f"== RESULT: {'PLAYING confirmed' if ok else 'did NOT reach stable playback'}")

    if not args.keep:
        try:
            mc.stop()
            cast.quit_app()
        except Exception:
            pass
        if session_id:
            api(f"/api/sessions/{session_id}", method="DELETE")
        print("== cleaned up (receiver stopped, session deleted)")

    pychromecast.discovery.stop_discovery(browser)
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
