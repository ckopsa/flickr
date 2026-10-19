// Drives the client with no screen and no hands: loads Main.qml, says the
// words a script names, and saves what the window drew.
//
//   QT_QPA_PLATFORM=offscreen QT_QUICK_BACKEND=software qml6 deck/dev/Harness.qml -- \
//       http://127.0.0.1:PORT --out=/tmp/shots --script=wait:1500,shot:home,right,ok,wait:800,shot:work
import QtQuick
import QtQuick.Window

Item {
    id: harness
    property var steps: []
    property string out: "."
    property int at: 0

    Loader { id: app; source: "../Main.qml" }

    function step() {
        if (at >= steps.length) { Qt.quit(); return; }
        var s = steps[at++].split(":");
        if (s[0] === "wait") { timer.interval = parseInt(s[1]); timer.start(); return; }
        if (s[0] === "shot") {
            app.item.contentItem.grabToImage(function (r) {
                r.saveToFile(harness.out + "/" + s[1] + ".png");
                timer.interval = 50; timer.start();
            });
            return;
        }
        app.item.send(s[0]);
        timer.interval = 120; timer.start();
    }
    Timer { id: timer; onTriggered: harness.step() }

    Component.onCompleted: {
        var args = Qt.application.arguments;
        for (var i = 0; i < args.length; i++) {
            if (args[i].indexOf("--script=") === 0) steps = args[i].slice(9).split(",");
            if (args[i].indexOf("--out=") === 0) out = args[i].slice(6);
        }
        app.item.visibility = Window.Windowed;
        app.item.width = 1280; app.item.height = 800;
        timer.interval = 300; timer.start();
    }
}
