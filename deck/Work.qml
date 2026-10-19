// A work: the document's fields, a button per action it affords, and its
// members in the server's order. What it does not afford is said in the
// document's own words rather than drawn as a dead button.
import QtQuick
import QtQuick.Controls

Item {
    id: screen
    property string address: ""
    property var doc: null
    property var buttons: []          // [{ name, action }]
    property int button: 0
    property int member: -1           // -1 while the buttons hold the ring
    readonly property var members: doc && doc.members && doc.members.length > 1 ? doc.members : []

    function nav(word) {
        if (!doc) return false;
        if (member < 0) {
            if (word === "left") button = Math.max(0, button - 1);
            else if (word === "right") button = Math.min(buttons.length - 1, button + 1);
            else if (word === "down" && members.length) member = Math.max(0, upNext());
            else if (word === "ok" && buttons[button]) press(buttons[button]);
            else return word === "up";
            return true;
        }
        if (word === "up") member = Math.max(buttons.length ? -1 : 0, member - 1); // off the top is back to the buttons
        else if (word === "down") member = Math.min(members.length - 1, member + 1);
        else if (word === "next") member = Math.min(members.length - 1, member + 6);
        else if (word === "prev") member = Math.max(0, member - 6);
        else if (word === "ok") win.open(members[member]);
        else return false;
        return true;
    }

    // The member the work's own progress points at, or the first.
    function upNext() {
        var next = doc.progress && doc.progress.next ? doc.progress.next.href : "";
        for (var i = 0; i < members.length; i++) if (members[i].self === next) return i;
        return 0;
    }

    function press(b) {
        if (b.name !== "play") {
            // save / unsave: the write, then the document that changed
            win.act(b.action, undefined, function (err) { if (err) win.refusal(err); load(); });
            return;
        }
        // The work's play is one member's play: that member holds the place.
        var all = doc.members || [], from = null;
        for (var i = 0; i < all.length; i++)
            if (all[i].actions && all[i].actions.play && all[i].actions.play.href === b.action.href) from = all[i];
        win.play(b.action, from && from.resume ? from.resume.position_seconds : 0, from);
    }

    function load() {
        win.api(address, null, function (err, d) {
            if (err) { win.refusal(err); return; }
            doc = d;
            var a = d.actions || {}, out = [];
            if (a.play) out.push({ name: "play", action: a.play });
            if (a.save) out.push({ name: "save", action: a.save });
            if (a.unsave) out.push({ name: "unsave", action: a.unsave });
            buttons = out;
            button = Math.min(button, Math.max(0, out.length - 1));
            if (!out.length && members.length && member < 0) member = upNext();
        });
    }
    function clock(s) {
        s = Math.round(s || 0);
        var h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
        return h ? h + "h " + m + "m" : m + "m";
    }
    function facts(d) {
        var out = [];
        if (d.year) out.push(d.year);
        if (d.certification) out.push(d.certification);
        if (d.runtime_minutes) out.push(d.runtime_minutes + " min");
        if (d.author) out.push(d.author);
        if (d.genres && d.genres.length) out.push(d.genres.join(", "));
        return out.join("  ·  ");
    }
    function refusedWhy(d) {
        var un = d.unavailable || {};
        if (d.actions && d.actions.play) return "";
        if (d.medium === "text") return "Books are read in the browser for now — this app plays, it does not read yet.";
        return un.play ? un.play.reason : "";
    }

    // Coming back from the player, the places have moved.
    StackView.onActivated: load()

    Image {
        anchors.fill: parent
        source: screen.doc ? win.picture(win.href(screen.doc.links.backdrop)) : ""
        asynchronous: true; fillMode: Image.PreserveAspectCrop; opacity: 0.22
    }

    Item {
        id: head
        x: 40 * win.u; y: 36 * win.u
        width: parent.width - 80 * win.u
        height: 300 * win.u
        visible: !!screen.doc

        Rectangle {
            id: poster
            width: 190 * win.u; height: 285 * win.u; radius: 8 * win.u
            color: win.panel; border.color: win.edge; clip: true
            Image {
                anchors.fill: parent
                source: screen.doc ? win.picture(win.href(screen.doc.links.poster) || win.href(screen.doc.links.cover) || win.href(screen.doc.links.artwork)) : ""
                asynchronous: true; fillMode: Image.PreserveAspectCrop
            }
        }
        Column {
            anchors { left: poster.right; leftMargin: 28 * win.u; right: parent.right; top: parent.top }
            spacing: 10 * win.u
            Text {
                width: parent.width; elide: Text.ElideRight
                text: screen.doc ? screen.doc.title : ""
                color: win.fg; font.pixelSize: 38 * win.u; font.weight: Font.DemiBold
            }
            Text {
                width: parent.width; elide: Text.ElideRight
                text: screen.doc ? screen.facts(screen.doc) : ""
                color: win.muted; font.pixelSize: 17 * win.u
            }
            Text {
                visible: text !== ""
                text: screen.doc && screen.doc.progress && screen.doc.progress.text ? screen.doc.progress.text : ""
                color: win.accent; font.pixelSize: 16 * win.u
            }
            Text {
                width: parent.width; height: 92 * win.u
                wrapMode: Text.WordWrap; elide: Text.ElideRight; maximumLineCount: 4
                text: screen.doc ? (screen.doc.overview || "") : ""
                color: win.fg; opacity: 0.9; font.pixelSize: 17 * win.u; lineHeight: 1.2
            }
            Row {
                spacing: 14 * win.u
                Repeater {
                    model: screen.buttons
                    Rectangle {
                        required property var modelData
                        required property int index
                        readonly property bool lit: screen.member < 0 && index === screen.button
                        width: label.implicitWidth + 44 * win.u; height: 50 * win.u; radius: 8 * win.u
                        color: lit ? win.accent : win.panel
                        border.color: lit ? win.accent : win.edge
                        Text {
                            id: label
                            anchors.centerIn: parent
                            text: modelData.action.label
                            color: parent.lit ? win.bg : win.fg
                            font.pixelSize: 19 * win.u; font.weight: Font.DemiBold
                        }
                    }
                }
                Text {
                    anchors.verticalCenter: parent.verticalCenter
                    width: 700 * win.u; wrapMode: Text.WordWrap
                    visible: text !== ""
                    text: screen.doc ? screen.refusedWhy(screen.doc) : ""
                    color: win.warn; font.pixelSize: 16 * win.u
                }
            }
        }
    }

    ListView {
        id: list
        anchors { left: parent.left; right: parent.right; top: head.bottom; bottom: parent.bottom
                  leftMargin: 40 * win.u; rightMargin: 40 * win.u; topMargin: 18 * win.u; bottomMargin: 12 * win.u }
        clip: true
        model: screen.members
        currentIndex: Math.max(0, screen.member)
        highlightMoveDuration: 120
        preferredHighlightBegin: 70 * win.u
        preferredHighlightEnd: height - 70 * win.u
        highlightRangeMode: ListView.ApplyRange
        spacing: 6 * win.u
        delegate: Rectangle {
            id: line
            required property var modelData
            required property int index
            readonly property bool lit: index === screen.member
            readonly property real kept: modelData.resume && modelData.duration_seconds
                ? Math.min(1, modelData.resume.position_seconds / modelData.duration_seconds) : 0
            width: list.width; height: 64 * win.u; radius: 8 * win.u
            color: lit ? "#262a36" : "transparent"
            border.width: (lit ? 3 : 0) * win.u
            border.color: win.accent
            Text {
                id: name
                anchors { left: parent.left; leftMargin: 18 * win.u; right: length.left; rightMargin: 16 * win.u; top: parent.top; topMargin: 9 * win.u }
                text: line.modelData.label || line.modelData.title
                color: win.fg; font.pixelSize: 18 * win.u; elide: Text.ElideRight
            }
            Text {
                anchors { left: name.left; right: name.right; top: name.bottom; topMargin: 3 * win.u }
                text: line.modelData.overview || line.modelData.tech || ""
                color: win.muted; font.pixelSize: 13 * win.u; elide: Text.ElideRight
            }
            Text {
                id: length
                anchors { right: parent.right; rightMargin: 18 * win.u; verticalCenter: parent.verticalCenter }
                text: line.modelData.duration_seconds ? screen.clock(line.modelData.duration_seconds) : ""
                color: win.muted; font.pixelSize: 15 * win.u
            }
            Rectangle {
                visible: line.kept > 0
                anchors { left: parent.left; right: parent.right; bottom: parent.bottom; leftMargin: 18 * win.u; rightMargin: 18 * win.u; bottomMargin: 4 * win.u }
                height: 3 * win.u; color: win.edge
                Rectangle { width: parent.width * line.kept; height: parent.height; color: win.accent }
            }
        }
    }
}
