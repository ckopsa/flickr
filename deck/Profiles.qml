// Who is watching: the profiles the root document links to, one face each.
import QtQuick

Item {
    id: gate
    property var people: []
    property int at: 0

    function nav(word) {
        if (word === "left") at = Math.max(0, at - 1);
        else if (word === "right") at = Math.min(people.length - 1, at + 1);
        else if (word === "ok" && people[at]) win.choose(people[at].name);
        return true; // who is watching has no way past
    }

    Component.onCompleted: {
        // The profiles hang off the root; the root is read as nobody yet.
        win.api(win.rootHref, null, function (err, doc) {
            if (err) return win.refusal(err);
            win.api(win.href(doc.links.profiles), null, function (err2, list) {
                if (err2) return win.refusal(err2);
                gate.people = list || [];
                for (var i = 0; i < gate.people.length; i++)
                    if (gate.people[i].name === win.profile) gate.at = i;
            });
        });
    }

    Text {
        id: asking
        anchors.horizontalCenter: parent.horizontalCenter
        anchors.bottom: faces.top; anchors.bottomMargin: 48 * win.u
        text: "Who is watching?"
        color: win.fg; font.pixelSize: 40 * win.u; font.weight: Font.DemiBold
    }
    ListView {
        id: faces
        anchors.centerIn: parent
        width: Math.min(parent.width - 80 * win.u, contentWidth)
        height: 230 * win.u
        orientation: ListView.Horizontal
        spacing: 24 * win.u
        model: gate.people
        currentIndex: gate.at
        highlightMoveDuration: 120
        delegate: Rectangle {
            required property var modelData
            required property int index
            width: 180 * win.u; height: 220 * win.u; radius: 14 * win.u
            color: win.panel
            border.width: (index === gate.at ? 4 : 1) * win.u
            border.color: index === gate.at ? win.accent : win.edge
            scale: index === gate.at ? 1.0 : 0.92
            Column {
                anchors.centerIn: parent
                spacing: 10 * win.u
                Text { anchors.horizontalCenter: parent.horizontalCenter; text: modelData.avatar || "🙂"; font.pixelSize: 84 * win.u }
                Text { anchors.horizontalCenter: parent.horizontalCenter; text: modelData.name; color: win.fg; font.pixelSize: 24 * win.u }
                Text {
                    anchors.horizontalCenter: parent.horizontalCenter
                    visible: !!modelData.kid
                    text: "KIDS"; color: win.accent; font.pixelSize: 13 * win.u; font.letterSpacing: 1.5 * win.u
                }
            }
        }
    }
    Text {
        anchors.horizontalCenter: parent.horizontalCenter
        anchors.top: faces.bottom; anchors.topMargin: 40 * win.u
        visible: gate.people.length === 0
        text: "No profiles yet — make one in the browser first."
        color: win.muted; font.pixelSize: 18 * win.u
    }
}
