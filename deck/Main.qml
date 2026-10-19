// flickr on a Steam Deck — the kernel of the native client.
//
// The same small knowledge the browser's kernel holds (docs/hypermedia.md
// §The client), and no more: ONE address by heart, `/api/`, and everything
// else a link or an action followed out of a document. What differs is the
// device — a screen nobody touches, a D-pad, and Qt's media player where the
// browser has a <video>.
//
// Every address is followed through the helper (helper.py), which adds the
// household's bearer and the profile's cookie; a stream's own `url` is the
// one thing fetched directly, because the session document already hands a
// device an address it can open with nothing in its hands.
//
// Input is WORDS, whoever sent them: the keyboard below, or a gamepad read by
// the helper. up/down/left/right/ok/back/play/search/prev/next/subs/menu —
// the screen on top answers `nav(word)`, and what it leaves alone falls
// through to here.
import QtQuick
import QtQuick.Window
import QtQuick.Controls

Window {
    id: win
    visible: true
    visibility: Window.FullScreen
    width: 1280; height: 800
    color: bg
    title: "flickr"

    // the colour tokens, as web/index.html names them
    readonly property color bg: "#14151a"
    readonly property color panel: "#1b1d24"
    readonly property color edge: "#2a2c33"
    readonly property color fg: "#e8e8ea"
    readonly property color muted: "#9a9daa"
    readonly property color accent: "#8db4f0"
    readonly property color warn: "#f0b45c"
    // one unit is a pixel on the Deck's own 800-line screen
    readonly property real u: Math.max(0.5, height / 800)

    // THE one address a client may hold.
    readonly property string rootHref: "/api/"

    property string base: ""        // the helper
    property string upstream: ""    // where a stream's own url is resolved
    property string profile: ""
    property var rootDoc: null
    property int padSeq: 0
    property bool booted: false
    property string noteText: ""
    property string upscaler: "fsr" // the player's: FLICKR_UPSCALE=fsrcnnx or 0 starts it so

    // What this device plays: Qt's FFmpeg backend takes nearly anything, so
    // nearly everything is a direct play. No HDR is claimed — the server
    // tone-maps rather than this screen washing it out.
    readonly property var capabilities: ({
        schema_version: 1,
        containers: ["mp4", "mkv", "webm", "m4v", "mov", "avi", "ts", "m4a", "m4b", "mp3", "flac", "ogg", "opus", "wav"],
        video_codecs: ["h264", "hevc", "av1", "vp9", "vp8", "mpeg2video", "mpeg4"],
        audio_codecs: ["aac", "mp3", "opus", "flac", "vorbis", "ac3", "eac3", "dts", "truehd", "alac"],
        max_width: 3840, max_height: 2160, max_audio_channels: 8,
        hls_segment_formats: ["ts", "fmp4"], hls_audio_codecs: ["aac", "mp3", "ac3", "eac3"]
    })

    // --- following an address -------------------------------------------------

    // api follows one href. It never builds one. A refusal is an answer: the
    // problem comes back as `err`, with its sentence and its remedy.
    function api(href, opts, done) {
        var o = opts || {};
        var x = new XMLHttpRequest();
        x.open(o.method || "GET", base + href);
        if (o.body !== undefined) x.setRequestHeader("Content-Type", "application/json");
        x.onreadystatechange = function () {
            if (x.readyState !== XMLHttpRequest.DONE) return;
            var doc = null;
            try { doc = x.responseText ? JSON.parse(x.responseText) : null; } catch (e) { doc = null; }
            if (x.status >= 200 && x.status < 300) { if (done) done(null, doc); return; }
            var p = doc || {};
            var err = {
                status: x.status,
                detail: p.detail || p.title || (x.status ? "That could not be done (" + x.status + ")." : "The helper is not answering."),
                remedy: p.remedy ? (p.remedy.text || "") : ""
            };
            if (x.status === 401 && !o.own) { door(); return; }
            if (done) done(err, null);
        };
        x.send(o.body !== undefined ? JSON.stringify(o.body) : undefined);
    }
    function act(action, body, done) {
        api(action.href, { method: action.method || "POST", body: body }, done);
    }
    function href(link) { return link && link.href ? link.href : ""; }
    function picture(h) { return h ? base + h : ""; }
    function awake(on) { api("/_deck/awake", { method: "POST", body: { on: on }, own: true }, null); }
    function note(text) { noteText = text || ""; if (noteText) noteTimer.restart(); }
    function refusal(err) { note([err.detail, err.remedy].filter(Boolean).join(" — ")); }

    // --- tiles ----------------------------------------------------------------

    // One tile out of one envelope — a work, an artist's shelf, or an item
    // with a place kept in it. The picture is whichever artwork link the
    // document carries; the shape follows the medium.
    function tile(d) {
        var l = d.links || {};
        var item = d.kind === "item";
        return {
            title: (item && d.work_title) ? d.work_title : (d.title || ""),
            subtitle: item ? (d.work_title ? (d.label || d.title || "") : (d.tech || "")) : (d.subtitle || ""),
            art: href(l.artwork) || href(l.poster) || href(l.cover) || href(l.still),
            shape: (d.medium === "audio" || d.kind === "artist") ? "square" : "poster",
            percent: d.percent || 0,
            doc: d
        };
    }
    function tiles(list) { return (list || []).map(tile); }

    // A tile is opened by what its document IS and what it affords.
    function open(d) {
        var a = d.actions || {}, l = d.links || {};
        if (d.kind === "item" && (a.resume || a.play)) {
            var at = d.position_seconds || (d.resume ? d.resume.position_seconds : 0) || 0;
            play(a.resume || a.play, at, d);
        } else if (d.kind === "artist") {
            api(href(l.self) || d.self, null, function (err, doc) {
                if (err) return refusal(err);
                stack.push(rowsScreen, { heading: doc.title, rows: [{ title: "Albums", tiles: tiles(doc.albums) }] });
            });
        } else if (href(l.self) || d.self) {
            stack.push(workScreen, { address: href(l.self) || d.self });
        } else if (href(l.work)) {
            stack.push(workScreen, { address: href(l.work) });
        }
    }

    // A play is the document's own action, sent what this device can take.
    // The answer is a session document, and the player is handed it.
    function play(action, seconds, itemDoc) {
        act(action, { capabilities: capabilities, client_id: profile, seek_seconds: seconds || 0 }, function (err, session) {
            if (err) return refusal(err);
            if (session.decision && session.decision.method === "deny") return note("This cannot be played on this device.");
            if (stack.currentItem && stack.currentItem.isPlayer) {
                stack.currentItem.adopt(session, action, itemDoc || null, seconds || 0);
            } else {
                stack.push(playerScreen, { session: session, playAction: action, itemDoc: itemDoc || null, startAt: seconds || 0 });
            }
        });
    }

    // --- boot: the door, who is watching, home ---------------------------------

    function boot() {
        api("/_deck/state", { own: true }, function (err, s) {
            if (err) { note(err.detail); bootRetry.start(); return; }
            upstream = s.upstream;
            profile = s.profile;
            if (!booted) { padSeq = s.pad_seq; booted = true; pad(); }
            if (s.error && !s.signed_in && !s.gated) { note(s.error); bootRetry.start(); return; }
            if (!s.signed_in) return door();
            enter();
        });
    }
    function door() {
        if (stack.currentItem && stack.currentItem.isDoor) return;
        stack.clear();
        stack.push(doorScreen);
    }
    function enter() {
        stack.clear();
        if (!profile) return stack.push(profilesScreen);
        api(rootHref, null, function (err, doc) {
            if (err) { refusal(err); bootRetry.start(); return; }
            rootDoc = doc;
            stack.push(homeScreen);
        });
    }
    function choose(name) {
        api("/_deck/profile", { method: "POST", body: { name: name }, own: true }, function (err, s) {
            if (err) return refusal(err);
            profile = s.profile;
            enter();
        });
    }

    // --- the words --------------------------------------------------------------

    function send(word) {
        var top = stack.currentItem;
        if (top && top.nav && top.nav(word)) return;
        if (word === "back" && stack.depth > 1) stack.pop();
        else if (word === "search" && rootDoc && href(rootDoc.links.search) && !(top && top.searching))
            stack.push(rowsScreen, { heading: "Search", searchHref: href(rootDoc.links.search) });
        else if (word === "menu" && rootDoc) { stack.clear(); stack.push(profilesScreen); }
    }

    // The pad, long-polled: the helper holds the request until a button moves.
    function pad() {
        var x = new XMLHttpRequest();
        x.open("GET", base + "/_deck/pad?since=" + padSeq);
        x.onreadystatechange = function () {
            if (x.readyState !== XMLHttpRequest.DONE) return;
            if (x.status !== 200) { padRetry.start(); return; }
            var d = JSON.parse(x.responseText);
            padSeq = d.seq;
            for (var i = 0; i < d.events.length; i++) send(d.events[i]);
            pad();
        };
        x.send();
    }

    readonly property var keyWords: ({
        [Qt.Key_Left]: "left", [Qt.Key_Right]: "right", [Qt.Key_Up]: "up", [Qt.Key_Down]: "down",
        [Qt.Key_Return]: "ok", [Qt.Key_Enter]: "ok", [Qt.Key_Space]: "play", [Qt.Key_K]: "play",
        [Qt.Key_Escape]: "back", [Qt.Key_Backspace]: "back", [Qt.Key_Back]: "back",
        [Qt.Key_J]: "prev", [Qt.Key_L]: "next", [Qt.Key_C]: "subs",
        [Qt.Key_Slash]: "search", [Qt.Key_S]: "search", [Qt.Key_P]: "menu",
        [Qt.Key_MediaTogglePlayPause]: "play", [Qt.Key_MediaPlay]: "play", [Qt.Key_MediaPause]: "play"
    })

    Timer { id: bootRetry; interval: 3000; onTriggered: win.boot() }
    Timer { id: padRetry; interval: 2000; onTriggered: win.pad() }
    Timer { id: noteTimer; interval: 6000; onTriggered: win.noteText = "" }

    StackView {
        id: stack
        anchors.fill: parent
        focus: true
        // a cut, not a slide: the pad repeats faster than a transition runs
        pushEnter: null; pushExit: null; popEnter: null; popExit: null; replaceEnter: null; replaceExit: null
        Keys.onPressed: function (event) {
            var word = win.keyWords[event.key];
            if (event.key === Qt.Key_F11) win.visibility = win.visibility === Window.FullScreen ? Window.Windowed : Window.FullScreen;
            if (!word) return;
            event.accepted = true;
            win.send(word);
        }
    }

    Component { id: doorScreen; Door { } }
    Component { id: profilesScreen; Profiles { } }
    Component { id: homeScreen; Rows { home: true } }
    Component { id: rowsScreen; Rows { } }
    Component { id: workScreen; Work { } }
    Component { id: playerScreen; Player { } }

    // A refusal, in the server's own words.
    Rectangle {
        visible: win.noteText !== ""
        anchors.horizontalCenter: parent.horizontalCenter
        anchors.bottom: parent.bottom
        anchors.bottomMargin: 56 * win.u
        width: Math.min(parent.width - 80 * win.u, noteLabel.implicitWidth + 40 * win.u)
        height: noteLabel.height + 24 * win.u
        radius: 8 * win.u
        color: win.panel
        border.color: win.warn
        z: 50
        Text {
            id: noteLabel
            anchors.centerIn: parent
            width: Math.min(implicitWidth, win.width - 120 * win.u)
            wrapMode: Text.WordWrap
            horizontalAlignment: Text.AlignHCenter
            text: win.noteText
            color: win.fg
            font.pixelSize: 17 * win.u
        }
    }

    Component.onCompleted: {
        var args = Qt.application.arguments;
        for (var i = 0; i < args.length; i++)
            if (args[i].indexOf("http://127.0.0.1:") === 0) base = args[i];
            else if (args[i] === "upscale=0") upscaler = "off";
            else if (args[i] === "upscale=fsrcnnx") upscaler = "fsrcnnx";
        boot();
    }
}
