// The door: a 401's remedy, walked the way a box with a D-pad can walk it.
// The helper asks the household's provider for a device code; the screen
// shows it, and a phone does the typing.
import QtQuick

Item {
    id: door
    readonly property bool isDoor: true
    property var state: ({})

    function nav(word) {
        if (word === "ok" && !state.pending) ask();
        return true; // who is signed in has no way past
    }
    function ask() { win.api("/_deck/signin", { method: "POST", own: true }, took); }
    function took(err, s) {
        if (err) { state = { error: err.detail }; return; }
        state = s;
        if (s.signed_in) { poll.stop(); win.boot(); }
    }

    Timer {
        id: poll
        interval: 2000; repeat: true; running: true
        onTriggered: win.api("/_deck/state", { own: true }, door.took)
    }
    Component.onCompleted: ask()

    Column {
        anchors.centerIn: parent
        width: parent.width * 0.8
        spacing: 22 * win.u

        Text {
            width: parent.width; horizontalAlignment: Text.AlignHCenter
            text: "Sign in to flickr"
            color: win.fg; font.pixelSize: 40 * win.u; font.weight: Font.DemiBold
        }
        Text {
            visible: !!door.state.pending
            width: parent.width; horizontalAlignment: Text.AlignHCenter; wrapMode: Text.WordWrap
            text: "On a phone or a computer, open"
            color: win.muted; font.pixelSize: 22 * win.u
        }
        Text {
            visible: !!door.state.pending
            width: parent.width; horizontalAlignment: Text.AlignHCenter; wrapMode: Text.WrapAnywhere
            text: door.state.verification_uri || ""
            color: win.accent; font.pixelSize: 26 * win.u
        }
        Text {
            visible: !!door.state.pending
            width: parent.width; horizontalAlignment: Text.AlignHCenter
            text: "and enter this code"
            color: win.muted; font.pixelSize: 22 * win.u
        }
        Text {
            visible: !!door.state.pending
            width: parent.width; horizontalAlignment: Text.AlignHCenter
            text: door.state.user_code || ""
            color: win.fg; font.pixelSize: 84 * win.u; font.weight: Font.Bold; font.letterSpacing: 6 * win.u
            font.family: "monospace"
        }
        Text {
            visible: !door.state.pending && !!door.state.error
            width: parent.width; horizontalAlignment: Text.AlignHCenter; wrapMode: Text.WordWrap
            text: door.state.error || ""
            color: win.warn; font.pixelSize: 22 * win.u
        }
        Text {
            width: parent.width; horizontalAlignment: Text.AlignHCenter
            text: door.state.pending ? "Waiting for the code to be approved…"
                : door.state.error ? "Press A to try again" : "Asking for a code…"
            color: win.muted; font.pixelSize: 18 * win.u
        }
    }
}
