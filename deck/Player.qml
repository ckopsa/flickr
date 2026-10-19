// The device: mpv (Flickr.Mpv, deck/mpv/) handed a session document.
//
// The session says what to load (`url`), how it was decided (`decision`),
// where the intro is (`skips`) and what may be done to it (`actions`): the
// place is written to `progress`, leaving is `stop`, and what follows is
// `next`. A direct play seeks in the file; a transcode's clock starts where
// the server was told to seek, so a jump past what it has made is the play
// action asked again from there — the rule web/player.js keeps.
import QtQuick
import QtQuick.Controls
import Flickr.Mpv

Item {
    id: screen
    readonly property bool isPlayer: true
    property var session: null
    property var playAction: null      // the action this session came from
    property var itemDoc: null
    property real startAt: 0

    property real offset: 0            // where a transcode's own clock starts
    property real pendingSeek: -1
    property bool ended: false
    property string failure: ""
    readonly property bool hls: !!session && !!session.decision && session.decision.method === "transcode"
    readonly property real position: offset + player.position
    readonly property real duration: itemDoc && itemDoc.duration_seconds ? itemDoc.duration_seconds
                                                                          : offset + player.duration
    readonly property var skip: {
        var list = session && session.skips ? session.skips : [];
        for (var i = 0; i < list.length; i++)
            if (position >= list[i].start && position < list[i].end - 1) return list[i];
        return null;
    }

    function nav(word) {
        wake();
        if (word === "back") { leave(); return true; }
        if (failure) return true;
        if (word === "ok") { if (skip) seekTo(skip.end); else toggle(); }
        else if (word === "play") toggle();
        else if (word === "left") seekTo(position - 10);
        else if (word === "right") seekTo(position + 10);
        else if (word === "prev") seekTo(position - 30);
        else if (word === "next") seekTo(position + 30);
        else if (word === "subs") cycleSubs();
        else if (word === "up") toggleUpscale();
        return true; // the device owns every word while it is up
    }

    function toggle() {
        if (player.playing) player.pause(); else player.play();
    }

    // The session document is adopted whole: a new one — a seek that restarted
    // the transcode, the next episode — replaces the last without the screen
    // being torn down.
    function adopt(s, action, item, at) {
        session = s;
        if (action) playAction = action;
        if (item) itemDoc = item;
        ended = false; failure = "";
        offset = hls ? at : 0;
        pendingSeek = (!hls && at > 0) ? at : -1;
        // The stream is the session's own address, opened as a device opens it.
        player.source = /^https?:/.test(s.url) ? s.url : win.upstream + s.url;
        player.play();
        wake();
    }

    function seekTo(t) {
        if (!session) return;
        t = Math.max(0, Math.min(t, Math.max(0, duration - 2)));
        var rel = t - offset;
        if (!hls || (rel >= 0 && rel <= player.duration - 0.5)) { player.seek(rel); return; }
        if (!playAction) return;
        win.note("Seeking to " + clock(t) + "…");
        var a = playAction, item = itemDoc;
        stop(false);
        win.play(a, t, item);
    }

    function keep() {
        var a = session && session.actions ? session.actions.progress : null;
        if (!a || position < 1) return;
        win.act(a, { position_seconds: Math.floor(position) }, null);
    }
    function stop(saving) {
        if (!session) return;
        if (saving) keep();
        var a = session.actions ? session.actions.stop : null;
        if (a) win.act(a, undefined, null);
    }
    function leave() {
        player.stop();
        stop(true);
        session = null;
        screen.StackView.view.pop();
    }

    // The end of one is the start of the next, where the session affords one.
    function finished() {
        if (ended || !session) return;
        ended = true;
        keep();
        var a = session.actions ? session.actions.next : null;
        if (!a) { leave(); return; }
        var was = session;
        win.act(a, { capabilities: win.capabilities, client_id: win.profile }, function (err, s) {
            if (err) { win.refusal(err); leave(); return; }
            if (was.actions.stop) win.act(was.actions.stop, undefined, null);
            // The next one's own play action and length are its item's.
            playAction = null; itemDoc = null;
            adopt(s, null, null, s.seek_seconds || 0);
            win.api(win.href(s.links.item), null, function (err2, item) {
                if (err2 || session !== s) return;
                itemDoc = item;
                playAction = item.actions ? item.actions.play : null;
            });
        });
    }

    function cycleSubs() {
        if (!player.subtitleCount) { win.note("This one carries no subtitles the player can show."); return; }
        var name = player.cycleSubtitles();
        win.note(name ? "Subtitles: " + name : "Subtitles off");
    }

    // Up goes FSR 1 → FSRCNNX → ArtCNN → off → FSR 1.
    readonly property var upscalers: ["fsr", "fsrcnnx", "artcnn", "off"]
    readonly property var upscalerNames: ({ fsr: "FSR 1", fsrcnnx: "FSRCNNX", artcnn: "ArtCNN" })
    function toggleUpscale() {
        upscaler = upscalers[(upscalers.indexOf(upscaler) + 1) % upscalers.length];
        var name = upscalerNames[upscaler], size = frame.height + "p to " + Math.round(frame.height * fit) + "p";
        if (upscaler === "off") win.note("Upscaling off");
        else if (fit <= 1.05) win.note("Upscaling on — this one is already as big as the screen");
        else if (upscaler === "fsr" || cnn) win.note("Upscaling: " + name + ", " + size);
        else win.note("Upscaling: " + name + " — this one is close enough to the screen's size that a plain lanczos scale does it");
    }

    function clock(s) {
        s = Math.max(0, Math.floor(s || 0));
        var h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), x = s % 60;
        return (h ? h + ":" + (m < 10 ? "0" : "") : "") + m + ":" + (x < 10 ? "0" : "") + x;
    }
    function wake() { chrome.shown = true; hide.restart(); }

    Component.onCompleted: adopt(session, playAction, itemDoc, startAt)
    Component.onDestruction: if (session) stop(true)

    Rectangle { anchors.fill: parent; color: "black" }

    // The picture, and the upscaling with it: mpv draws the frame to the
    // screen's size on the GPU, from the decoder's own surface, and the
    // upscaler is a choice of its shaders (shaders/mpv/, and the two
    // networks' own files). FSR 1: EASU stretches the frame along its edges
    // to the size it fills, RCAS sharpens what that softened. FSRCNNX and
    // ArtCNN: a small trained network doubles it, past 1.3x (mpv's own rule),
    // and ewa_lanczos takes it the rest of the way. Both read text and line
    // art far better than FSR; FSRCNNX keeps a clean source's detail best,
    // ArtCNN (trained to denoise as it goes) does best through a stream's
    // compression. Up cycles the three and off, the plain bilinear stretch;
    // a frame as big as the screen is shown as it always was. While it plays
    // the item holds Steam's dimmer off: nobody touches a pad during a film.
    Mpv {
        id: player
        anchors.fill: parent
        upscaler: screen.upscaler
        onLoaded: if (screen.pendingSeek >= 0) { seek(screen.pendingSeek); screen.pendingSeek = -1; }
        onEnded: screen.finished()
        onFailed: function (reason) {
            screen.failure = reason || "The stream could not be played.";
            chrome.shown = true;
        }
    }
    property string upscaler: win.upscaler   // "fsr", "fsrcnnx", "artcnn" or "off"
    readonly property size frame: player.videoSize
    readonly property real fit: frame.width > 0 && frame.height > 0
                                ? Math.min(width * Screen.devicePixelRatio / frame.width, height * Screen.devicePixelRatio / frame.height) : 0
    readonly property bool upscaling: upscaler !== "off" && player.hasVideo && fit > 1.05
    readonly property bool cnn: upscaling && (upscaler === "fsrcnnx" || upscaler === "artcnn") && fit > 1.3

    // Something with no picture still has a face.
    Image {
        anchors.centerIn: parent
        visible: !player.hasVideo && screen.session
        width: 360 * win.u; height: 360 * win.u
        fillMode: Image.PreserveAspectFit; asynchronous: true
        source: screen.session ? win.picture(win.href(screen.session.links.artwork)) : ""
    }

    Timer { id: hide; interval: 4000; onTriggered: if (player.playing && !screen.failure) chrome.shown = false }
    Timer { interval: 10000; repeat: true; running: player.playing; onTriggered: screen.keep() }

    Rectangle {   // the skip the session names, while its span is on
        visible: !!screen.skip
        anchors { right: parent.right; bottom: parent.bottom; rightMargin: 40 * win.u; bottomMargin: (chrome.shown ? 150 : 48) * win.u }
        width: skipLabel.implicitWidth + 40 * win.u; height: 50 * win.u; radius: 8 * win.u
        color: win.accent
        Text {
            id: skipLabel
            anchors.centerIn: parent
            text: "A   " + (screen.skip ? screen.skip.label : "")
            color: win.bg; font.pixelSize: 19 * win.u; font.weight: Font.DemiBold
        }
    }

    Item {
        id: chrome
        property bool shown: true
        anchors.fill: parent
        opacity: shown || !player.hasVideo ? 1 : 0
        Behavior on opacity { NumberAnimation { duration: 180 } }

        Rectangle {
            anchors { left: parent.left; right: parent.right; top: parent.top }
            height: 120 * win.u
            gradient: Gradient { GradientStop { position: 0; color: "#cc000000" } GradientStop { position: 1; color: "#00000000" } }
        }
        Column {
            x: 40 * win.u; y: 26 * win.u
            width: parent.width - 80 * win.u
            spacing: 4 * win.u
            Text {
                width: parent.width; elide: Text.ElideRight
                text: screen.session ? (screen.session.links.work && screen.session.links.work.title !== screen.session.title
                                        ? screen.session.links.work.title + "  ·  " + screen.session.title : screen.session.title) : ""
                color: win.fg; font.pixelSize: 26 * win.u; font.weight: Font.DemiBold
            }
            Text {
                visible: text !== ""
                width: parent.width; wrapMode: Text.WordWrap
                text: screen.failure
                color: win.warn; font.pixelSize: 18 * win.u
            }
        }

        Rectangle {
            anchors { left: parent.left; right: parent.right; bottom: parent.bottom }
            height: 150 * win.u
            gradient: Gradient { GradientStop { position: 0; color: "#00000000" } GradientStop { position: 1; color: "#dd000000" } }
        }
        Item {
            anchors { left: parent.left; right: parent.right; bottom: parent.bottom; leftMargin: 40 * win.u; rightMargin: 40 * win.u; bottomMargin: 22 * win.u }
            height: 86 * win.u
            Text {
                id: state
                anchors { left: parent.left; top: parent.top }
                text: (player.playing ? "▶" : "❚❚") + "   " + screen.clock(screen.position) + " / " + screen.clock(screen.duration)
                color: win.fg; font.pixelSize: 20 * win.u
            }
            Text {
                anchors { right: parent.right; top: parent.top }
                visible: player.loading
                text: "Loading…"
                color: win.muted; font.pixelSize: 16 * win.u
            }
            Rectangle {
                id: bar
                anchors { left: parent.left; right: parent.right; top: state.bottom; topMargin: 12 * win.u }
                height: 6 * win.u; radius: 3 * win.u; color: "#55ffffff"
                Rectangle {
                    width: screen.duration > 0 ? parent.width * Math.min(1, screen.position / screen.duration) : 0
                    height: parent.height; radius: parent.radius; color: win.accent
                }
            }
            Text {
                anchors { left: parent.left; top: bar.bottom; topMargin: 12 * win.u }
                text: "X  Play / pause      ◀ ▶  10s      L1 R1  30s      ▲  Upscale" + (screen.cnn ? " (" + screen.upscalerNames[screen.upscaler] + ")" : screen.upscaling ? " (FSR)" : "") + "      Select  Subtitles      B  Back"
                color: win.muted; font.pixelSize: 14 * win.u
            }
        }
    }
}
