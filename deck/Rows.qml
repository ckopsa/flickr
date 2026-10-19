// Shelves: headed rows of tiles, walked with a D-pad. Home is this drawn from
// the library document; an artist is this with one row; a search is this with
// a box on top and the search document's groups underneath.
import QtQuick
import QtQuick.Controls

Item {
    id: screen
    property bool home: false
    property string heading: ""
    property string searchHref: ""
    readonly property bool searching: searchHref !== ""
    property var rows: []        // [{ title, tiles: [tile] }]
    property int row: 0
    property var cols: []        // the column each row was left at
    property bool typing: searching

    function col(r) { return cols[r] || 0; }
    function current() { var r = rows[row]; return r ? r.tiles[col(row)] : null; }

    function nav(word) {
        if (typing) {
            if (word === "down" && rows.length) { typing = false; return true; }
            if (word === "back") return false;
            return word !== "menu";
        }
        if (word === "left" || word === "right") {
            var r = rows[row];
            if (!r) return true;
            var c = Math.max(0, Math.min(r.tiles.length - 1, col(row) + (word === "left" ? -1 : 1)));
            var next = cols.slice(); next[row] = c; cols = next;
            return true;
        }
        if (word === "up") {
            if (row === 0 && searching) typing = true;
            else row = Math.max(0, row - 1);
            return true;
        }
        if (word === "down") { row = Math.min(rows.length - 1, row + 1); return true; }
        if (word === "ok") { var t = current(); if (t) win.open(t.doc); return true; }
        return false;
    }

    // --- home: the library document, in the server's order ----------------------
    function loadHome() {
        var l = win.rootDoc.links;
        win.api(win.href(l.library), null, function (err, lib) {
            if (err) return win.refusal(err);
            var out = [];
            function shelf(title, list) { if (list && list.length) out.push({ title: title, tiles: win.tiles(list) }); }
            shelf("My List", lib.list);
            shelf("New episodes", lib.new_episodes);
            shelf("Recently added", lib.recently_added);
            (lib.bands || []).forEach(function (b) {
                shelf(b.title, (lib.items || []).filter(function (i) { return i.band === b.key; }));
            });
            win.api(win.href(l["continue"]), null, function (err2, cont) {
                if (!err2 && cont.items && cont.items.length)
                    out.unshift({ title: cont.title, tiles: win.tiles(cont.items) });
                screen.rows = out;
                screen.row = Math.min(screen.row, Math.max(0, out.length - 1));
            });
        });
    }

    // --- search: the box's words ride as the query the link is spelled with ----
    function find(q) {
        if (!q) { rows = []; return; }
        var sep = searchHref.indexOf("?") >= 0 ? "&" : "?";
        win.api(searchHref + sep + "q=" + encodeURIComponent(q), null, function (err, found) {
            if (err) return win.refusal(err);
            if (found.query !== undefined && found.query !== box.text.trim()) return; // an older answer
            screen.rows = (found.groups || []).filter(function (g) {
                return g.key !== "lines" && g.items && g.items.length;
            }).map(function (g) { return { title: g.title, tiles: win.tiles(g.items) }; });
            screen.row = 0; screen.cols = [];
        });
    }

    // A place kept while this screen was underneath shows when it comes back.
    StackView.onActivated: if (home) loadHome()

    onTypingChanged: if (typing) box.forceActiveFocus(); else screen.StackView.view.forceActiveFocus()
    Component.onCompleted: if (typing) box.forceActiveFocus()

    Row {
        id: bar
        x: 40 * win.u; y: 22 * win.u
        height: 48 * win.u
        spacing: 16 * win.u
        Text {
            anchors.verticalCenter: parent.verticalCenter
            text: screen.home ? "flickr" : screen.heading
            color: win.fg; font.pixelSize: 26 * win.u; font.weight: Font.DemiBold
        }
        Text {
            anchors.verticalCenter: parent.verticalCenter
            visible: screen.home
            text: win.profile
            color: win.muted; font.pixelSize: 18 * win.u
        }
        TextField {
            id: box
            visible: screen.searching
            anchors.verticalCenter: parent.verticalCenter
            width: 520 * win.u; height: 44 * win.u
            font.pixelSize: 20 * win.u
            color: win.fg
            placeholderText: "Titles, people, episodes…"
            placeholderTextColor: win.muted
            background: Rectangle {
                radius: 8 * win.u; color: win.panel
                border.width: (screen.typing ? 3 : 1) * win.u
                border.color: screen.typing ? win.accent : win.edge
            }
            onTextChanged: debounce.restart()
            Keys.onPressed: function (event) {
                if (event.key === Qt.Key_Down || event.key === Qt.Key_Return || event.key === Qt.Key_Enter) {
                    event.accepted = true; win.send("down");
                } else if (event.key === Qt.Key_Escape) {
                    event.accepted = true; win.send("back");
                }
            }
        }
    }
    Timer { id: debounce; interval: 300; onTriggered: screen.find(box.text.trim()) }

    ListView {
        id: shelves
        anchors { left: parent.left; right: parent.right; top: bar.bottom; bottom: hints.top; topMargin: 10 * win.u }
        clip: true
        model: screen.rows
        currentIndex: screen.row
        highlightMoveDuration: 160
        preferredHighlightBegin: 0
        preferredHighlightEnd: height
        highlightRangeMode: ListView.ApplyRange
        cacheBuffer: 600 * win.u
        delegate: Item {
            id: shelf
            required property var modelData
            required property int index
            readonly property bool here: index === screen.row && !screen.typing
            width: shelves.width
            height: 334 * win.u
            Text {
                x: 40 * win.u
                text: shelf.modelData.title
                color: shelf.here ? win.fg : win.muted
                font.pixelSize: 20 * win.u; font.weight: Font.DemiBold
            }
            ListView {
                id: strip
                x: 0; y: 34 * win.u
                width: parent.width; height: 296 * win.u
                orientation: ListView.Horizontal
                leftMargin: 40 * win.u; rightMargin: 40 * win.u
                spacing: 16 * win.u
                model: shelf.modelData.tiles
                currentIndex: screen.col(shelf.index)
                highlightMoveDuration: 140
                preferredHighlightBegin: 40 * win.u
                preferredHighlightEnd: width - 40 * win.u
                highlightRangeMode: ListView.ApplyRange
                cacheBuffer: 800 * win.u
                delegate: Item {
                    id: card
                    required property var modelData
                    required property int index
                    readonly property bool lit: shelf.here && index === screen.col(shelf.index)
                    width: (modelData.shape === "square" ? 225 : 150) * win.u
                    height: 296 * win.u
                    Rectangle {
                        id: art
                        width: parent.width; height: 225 * win.u
                        radius: 8 * win.u
                        color: win.panel
                        border.width: (card.lit ? 4 : 1) * win.u
                        border.color: card.lit ? win.accent : win.edge
                        scale: card.lit ? 1.06 : 1.0
                        transformOrigin: Item.Center
                        clip: true
                        Text {
                            anchors.fill: parent; anchors.margins: 10 * win.u
                            visible: cover.status !== Image.Ready
                            text: card.modelData.title
                            color: win.muted; font.pixelSize: 16 * win.u
                            wrapMode: Text.WordWrap; elide: Text.ElideRight
                            horizontalAlignment: Text.AlignHCenter; verticalAlignment: Text.AlignVCenter
                        }
                        Image {
                            id: cover
                            anchors.fill: parent; anchors.margins: art.border.width
                            source: win.picture(card.modelData.art)
                            asynchronous: true
                            fillMode: Image.PreserveAspectCrop
                            sourceSize.height: 340 * win.u
                        }
                        Rectangle {   // the place kept in it
                            visible: card.modelData.percent > 0
                            anchors { left: parent.left; right: parent.right; bottom: parent.bottom; margins: art.border.width }
                            height: 5 * win.u; color: "#55000000"
                            Rectangle { width: parent.width * Math.min(1, card.modelData.percent / 100); height: parent.height; color: win.accent }
                        }
                    }
                    Text {
                        anchors { top: art.bottom; topMargin: 12 * win.u }
                        width: parent.width
                        text: card.modelData.title
                        color: card.lit ? win.fg : win.muted
                        font.pixelSize: 15 * win.u; elide: Text.ElideRight
                    }
                    Text {
                        anchors { top: art.bottom; topMargin: 32 * win.u }
                        width: parent.width
                        text: card.modelData.subtitle
                        color: win.muted; opacity: 0.8
                        font.pixelSize: 13 * win.u; elide: Text.ElideRight
                    }
                }
            }
        }
    }

    Text {
        anchors.centerIn: shelves
        visible: screen.rows.length === 0
        text: screen.searching ? (box.text.trim() ? "Nothing found." : "Type to search — Steam + X opens the keyboard.") : "Loading…"
        color: win.muted; font.pixelSize: 18 * win.u
    }

    Text {
        id: hints
        anchors { left: parent.left; right: parent.right; bottom: parent.bottom; leftMargin: 40 * win.u; bottomMargin: 12 * win.u }
        height: 24 * win.u
        text: screen.home ? "A  Open      Y  Search      Start  Profiles" : "A  Open      B  Back"
        color: win.muted; font.pixelSize: 14 * win.u
    }
}
