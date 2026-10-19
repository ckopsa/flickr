// flickr-deck-ui: Main.qml on Qt Quick, with the Mpv item.
//
// What qml6 was to the client before, plus the two things libmpv needs
// before any window exists: Qt drawing with OpenGL over EGL (mpv renders
// into Qt's GL context, and its VA-API interop is EGL's), and C numerics
// (mpv refuses to start under a locale that writes 0,5). Arguments after
// the file are the client's own (`--`, the helper's address, `upscale=`).
#include <QGuiApplication>
#include <QQmlApplicationEngine>
#include <QQuickWindow>
#include <QUrl>

#include <clocale>

int main(int argc, char *argv[])
{
    qputenv("QT_XCB_GL_INTEGRATION", "xcb_egl");
    QQuickWindow::setGraphicsApi(QSGRendererInterface::OpenGL);

    QGuiApplication app(argc, argv);
    QGuiApplication::setApplicationName(QStringLiteral("flickr"));
    std::setlocale(LC_NUMERIC, "C");

    const QStringList args = QGuiApplication::arguments();
    if (args.size() < 2) {
        qWarning("usage: flickr-deck-ui Main.qml [-- args]");
        return 2;
    }
    QQmlApplicationEngine engine;
    QObject::connect(&engine, &QQmlApplicationEngine::objectCreationFailed, &app,
                     [] { QCoreApplication::exit(1); }, Qt::QueuedConnection);
    engine.load(QUrl::fromLocalFile(args.at(1)));
    return app.exec();
}
