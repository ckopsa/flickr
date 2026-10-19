// Mpv: libmpv as a Qt Quick item, for the Deck's player.
//
// Qt Multimedia decodes on the GPU here but cannot keep the frame there:
// over GLX its VA-API interop is off, and over EGL its dmabuf import draws
// AMD's tiled surfaces as stripes. mpv imports them right, so the frame goes
// decoder → mpv's renderer → this item's framebuffer without leaving the
// GPU, and mpv runs the upscalers itself — FSRCNNX and ArtCNN from their own
// files, FSR 1 from ours (shaders/mpv/). Qt has to draw with OpenGL over EGL
// for that (QSG_RHI_BACKEND=opengl, QT_XCB_GL_INTEGRATION=xcb_egl).
//
// The item also holds Steam's screen dimmer off while it plays: in Game Mode
// Steam is org.freedesktop.ScreenSaver, and the inhibit goes with the item.
#pragma once

#include <QQuickFramebufferObject>
#include <QSize>
#include <QString>
#include <QStringList>
#include <QVariant>

struct mpv_handle;
struct mpv_render_context;

class MpvItem : public QQuickFramebufferObject
{
    Q_OBJECT
    QML_NAMED_ELEMENT(Mpv)

    Q_PROPERTY(QString source READ source WRITE setSource NOTIFY sourceChanged)
    // "fsr", "fsrcnnx", "artcnn" or "off"
    Q_PROPERTY(QString upscaler READ upscaler WRITE setUpscaler NOTIFY upscalerChanged)
    Q_PROPERTY(bool playing READ playing NOTIFY playingChanged)
    // Opening, buffering or seeking: the picture is not moving for want of data.
    Q_PROPERTY(bool loading READ loading NOTIFY loadingChanged)
    Q_PROPERTY(bool seekable READ seekable NOTIFY seekableChanged)
    Q_PROPERTY(double position READ position NOTIFY positionChanged)   // seconds
    Q_PROPERTY(double duration READ duration NOTIFY durationChanged)   // seconds
    Q_PROPERTY(bool hasVideo READ hasVideo NOTIFY videoSizeChanged)
    Q_PROPERTY(QSize videoSize READ videoSize NOTIFY videoSizeChanged)
    Q_PROPERTY(int subtitleCount READ subtitleCount NOTIFY tracksChanged)
    // What is decoding it: "vaapi" is on the GPU throughout, "vaapi-copy" or
    // "" (software) is not.
    Q_PROPERTY(QString hwdec READ hwdec NOTIFY hwdecChanged)
    // Frames mpv dropped or showed late since the file opened: 0 is smooth.
    Q_PROPERTY(int droppedFrames READ droppedFrames NOTIFY droppedFramesChanged)

public:
    explicit MpvItem(QQuickItem *parent = nullptr);
    ~MpvItem() override;

    Renderer *createRenderer() const override;

    QString source() const { return m_source; }
    void setSource(const QString &url);
    QString upscaler() const { return m_upscaler; }
    void setUpscaler(const QString &name);
    bool playing() const { return m_playing; }
    bool loading() const { return m_loading; }
    bool seekable() const { return m_seekable; }
    double position() const { return m_position; }
    double duration() const { return m_duration; }
    bool hasVideo() const { return !m_videoSize.isEmpty(); }
    QSize videoSize() const { return m_videoSize; }
    int subtitleCount() const { return m_subtitles.size(); }
    QString hwdec() const { return m_hwdec; }
    int droppedFrames() const { return m_dropped + m_delayed; }

    Q_INVOKABLE void play();
    Q_INVOKABLE void pause();
    Q_INVOKABLE void stop();
    Q_INVOKABLE void seek(double seconds);
    // The next subtitle track, or none after the last; answers what is on.
    Q_INVOKABLE QString cycleSubtitles();

    mpv_handle *handle() const { return m_mpv; }

signals:
    void sourceChanged();
    void upscalerChanged();
    void playingChanged();
    void loadingChanged();
    void seekableChanged();
    void positionChanged();
    void durationChanged();
    void videoSizeChanged();
    void tracksChanged();
    void hwdecChanged();
    void droppedFramesChanged();
    void loaded();
    void ended();
    void failed(const QString &reason);
    void redraw();

private slots:
    void drain();
    void renderReady();

private:
    void command(const QStringList &args);
    void load();
    void applyUpscaler();
    void hold(bool awake);
    void handleProperty(const char *name, const QVariant &value);

    mpv_handle *m_mpv = nullptr;
    QString m_source;
    QString m_upscaler = QStringLiteral("fsr");
    QString m_shaderDir;
    bool m_rendering = false;
    bool m_playing = false;
    bool m_paused = false;
    bool m_idle = true;
    bool m_cache = false;
    bool m_seeking = false;
    bool m_loading = false;
    bool m_seekable = false;
    bool m_ended = false;
    double m_position = 0;
    double m_duration = 0;
    QSize m_videoSize;
    QString m_hwdec;
    QList<QVariantMap> m_subtitles;
    int m_subtitle = -1;
    uint m_cookie = 0;
    int m_dropped = 0;
    int m_delayed = 0;
};
