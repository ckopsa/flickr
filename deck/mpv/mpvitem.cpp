#include "mpvitem.h"

#include <QCoreApplication>
#include <QDBusConnection>
#include <QDBusInterface>
#include <QDBusReply>
#include <QDir>
#include <QGuiApplication>
#include <QOpenGLContext>
#include <QOpenGLFramebufferObject>
#include <QQuickOpenGLUtils>
#include <QQuickWindow>

#include <mpv/client.h>
#include <mpv/render_gl.h>

namespace {

QVariant fromNode(const mpv_node &n)
{
    switch (n.format) {
    case MPV_FORMAT_STRING: return QString::fromUtf8(n.u.string);
    case MPV_FORMAT_FLAG: return bool(n.u.flag);
    case MPV_FORMAT_INT64: return qlonglong(n.u.int64);
    case MPV_FORMAT_DOUBLE: return n.u.double_;
    case MPV_FORMAT_NODE_ARRAY: {
        QVariantList list;
        for (int i = 0; i < n.u.list->num; i++)
            list.append(fromNode(n.u.list->values[i]));
        return list;
    }
    case MPV_FORMAT_NODE_MAP: {
        QVariantMap map;
        for (int i = 0; i < n.u.list->num; i++)
            map.insert(QString::fromUtf8(n.u.list->keys[i]), fromNode(n.u.list->values[i]));
        return map;
    }
    default: return {};
    }
}

void *procAddress(void *, const char *name)
{
    QOpenGLContext *gl = QOpenGLContext::currentContext();
    return gl ? reinterpret_cast<void *>(gl->getProcAddress(QByteArray(name))) : nullptr;
}

// Draws mpv's picture into the item's framebuffer, on the render thread. It
// holds its own client of the core, so the core outlives the item until the
// render context is freed — mpv requires that order.
class MpvRenderer : public QQuickFramebufferObject::Renderer
{
public:
    explicit MpvRenderer(MpvItem *item)
        : m_item(item), m_mpv(mpv_create_client(item->handle(), "render")) {}

    ~MpvRenderer() override
    {
        if (m_ctx) mpv_render_context_free(m_ctx);
        mpv_destroy(m_mpv);
    }

    QOpenGLFramebufferObject *createFramebufferObject(const QSize &size) override
    {
        if (!m_ctx) {
            mpv_opengl_init_params gl{procAddress, nullptr};
            int advanced = 1;
            QList<mpv_render_param> params{
                {MPV_RENDER_PARAM_API_TYPE, const_cast<char *>(MPV_RENDER_API_TYPE_OPENGL)},
                {MPV_RENDER_PARAM_OPENGL_INIT_PARAMS, &gl},
                {MPV_RENDER_PARAM_ADVANCED_CONTROL, &advanced},
            };
            // VA-API's interop needs the display Qt's EGL context is on.
            if (auto *x11 = qGuiApp->nativeInterface<QNativeInterface::QX11Application>())
                params.append({MPV_RENDER_PARAM_X11_DISPLAY, x11->display()});
            if (auto *wl = qGuiApp->nativeInterface<QNativeInterface::QWaylandApplication>())
                params.append({MPV_RENDER_PARAM_WL_DISPLAY, wl->display()});
            params.append({MPV_RENDER_PARAM_INVALID, nullptr});
            if (mpv_render_context_create(&m_ctx, m_mpv, params.data()) < 0) {
                qWarning("Mpv: could not make a render context");
                m_ctx = nullptr;
            } else {
                mpv_render_context_set_update_callback(m_ctx, [](void *item) {
                    QMetaObject::invokeMethod(static_cast<MpvItem *>(item), &QQuickItem::update, Qt::QueuedConnection);
                }, m_item);
                QMetaObject::invokeMethod(m_item, "renderReady", Qt::QueuedConnection);
            }
        }
        return QQuickFramebufferObject::Renderer::createFramebufferObject(size);
    }

    void render() override
    {
        if (!m_ctx) return;
        QOpenGLFramebufferObject *target = framebufferObject();
        mpv_opengl_fbo fbo{int(target->handle()), target->width(), target->height(), 0};
        int flip = 0;   // Qt 6's GL scene graph samples the FBO the way mpv draws it
        mpv_render_param params[] = {
            {MPV_RENDER_PARAM_OPENGL_FBO, &fbo},
            {MPV_RENDER_PARAM_FLIP_Y, &flip},
            {MPV_RENDER_PARAM_INVALID, nullptr},
        };
        mpv_render_context_render(m_ctx, params);
        QQuickOpenGLUtils::resetOpenGLState();   // mpv leaves GL as it likes it; Qt draws next
    }

private:
    MpvItem *m_item;
    mpv_handle *m_mpv;
    mpv_render_context *m_ctx = nullptr;
};

} // namespace

MpvItem::MpvItem(QQuickItem *parent)
    : QQuickFramebufferObject(parent), m_mpv(mpv_create())
{
    // Where the upscalers' shader files are: beside the app, or named.
    m_shaderDir = qEnvironmentVariable("FLICKR_DECK_SHADERS",
        QCoreApplication::applicationDirPath() + QStringLiteral("/../share/flickr-deck/shaders/mpv"));

    auto set = [this](const char *name, const char *value) { mpv_set_option_string(m_mpv, name, value); };
    set("vo", "libmpv");
    set("hwdec", "vaapi");          // on the GPU, frame and all; mpv falls back itself
    set("keep-open", "yes");        // the end is reported, not unloaded
    set("config", "no");
    set("terminal", "no");
    set("msg-level", "all=warn");
    set("input-default-bindings", "no");
    set("input-vo-keyboard", "no");
    set("osc", "no");
    set("ytdl", "no");
    set("load-scripts", "no");
    set("load-stats-overlay", "no");
    set("load-console", "no");
    set("load-auto-profiles", "no");
    set("sub-auto", "no");
    if (mpv_initialize(m_mpv) < 0) {
        qWarning("Mpv: could not start mpv");
        return;
    }
    // FLICKR_DECK_MPV_LOG=v (or info, debug…) has mpv's own log printed.
    const QByteArray log = qgetenv("FLICKR_DECK_MPV_LOG");
    if (!log.isEmpty()) mpv_request_log_messages(m_mpv, log.constData());
    for (const char *name : {"time-pos", "duration", "pause", "idle-active", "paused-for-cache", "seeking",
                             "seekable", "eof-reached", "video-params/w", "video-params/h", "hwdec-current", "track-list",
                             "frame-drop-count", "vo-delayed-frame-count"})
        mpv_observe_property(m_mpv, 0, name, MPV_FORMAT_NODE);
    mpv_set_wakeup_callback(m_mpv, [](void *item) {
        QMetaObject::invokeMethod(static_cast<MpvItem *>(item), "drain", Qt::QueuedConnection);
    }, this);
    applyUpscaler();
}

MpvItem::~MpvItem()
{
    hold(false);
    if (!m_mpv) return;
    mpv_set_wakeup_callback(m_mpv, nullptr, nullptr);
    command({QStringLiteral("stop")});
    mpv_destroy(m_mpv);   // the renderer's own client keeps the core until it goes
}

QQuickFramebufferObject::Renderer *MpvItem::createRenderer() const
{
    return new MpvRenderer(const_cast<MpvItem *>(this));
}

void MpvItem::command(const QStringList &args)
{
    QList<QByteArray> bytes;
    for (const QString &a : args) bytes.append(a.toUtf8());
    QList<const char *> argv;
    for (const QByteArray &b : bytes) argv.append(b.constData());
    argv.append(nullptr);
    mpv_command_async(m_mpv, 0, argv.data());
}

void MpvItem::setSource(const QString &url)
{
    if (url == m_source) return;
    m_source = url;
    m_ended = false;
    m_loading = !url.isEmpty();
    emit loadingChanged();
    load();
    emit sourceChanged();
}

// mpv picks its video output as a file opens; before the renderer has made
// its context there is none, and mpv would drop the picture for good. So a
// source set early waits for renderReady().
void MpvItem::load()
{
    if (!m_rendering) return;
    if (m_source.isEmpty()) command({QStringLiteral("stop")});
    else command({QStringLiteral("loadfile"), m_source});
}

void MpvItem::renderReady()
{
    if (m_rendering) return;
    m_rendering = true;
    load();
}

// mpv draws the picture to the screen's size itself, so each upscaler is a
// choice of shaders and of the scaler for whatever they leave: FSRCNNX and
// ArtCNN double the luma (mpv's own rule: only past 1.3x) and ewa_lanczos
// takes it the rest of the way; FSR 1 goes straight to the screen's size.
// "off" is the plain bilinear stretch the player always had.
void MpvItem::setUpscaler(const QString &name)
{
    if (name == m_upscaler) return;
    m_upscaler = name;
    applyUpscaler();
    emit upscalerChanged();
}

void MpvItem::applyUpscaler()
{
    QString shader, scale = QStringLiteral("ewa_lanczos"), cscale = QStringLiteral("ewa_lanczos");
    if (m_upscaler == QLatin1String("fsr")) shader = QStringLiteral("fsr.glsl");
    else if (m_upscaler == QLatin1String("fsrcnnx")) shader = QStringLiteral("FSRCNNX_x2_8-0-4-1.glsl");
    else if (m_upscaler == QLatin1String("artcnn")) shader = QStringLiteral("ArtCNN_C4F16_DS.glsl");
    else scale = cscale = QStringLiteral("bilinear");
    const QByteArray path = shader.isEmpty() ? QByteArray() : QDir(m_shaderDir).filePath(shader).toUtf8();
    mpv_set_property_string(m_mpv, "glsl-shaders", path.constData());
    mpv_set_property_string(m_mpv, "scale", scale.toUtf8().constData());
    mpv_set_property_string(m_mpv, "cscale", cscale.toUtf8().constData());
}

void MpvItem::play()
{
    if (m_ended) seek(0);
    mpv_set_property_string(m_mpv, "pause", "no");
}

void MpvItem::pause() { mpv_set_property_string(m_mpv, "pause", "yes"); }

void MpvItem::stop()
{
    m_source.clear();
    emit sourceChanged();
    command({QStringLiteral("stop")});
}

void MpvItem::seek(double seconds)
{
    m_ended = false;
    command({QStringLiteral("seek"), QString::number(seconds, 'f', 3), QStringLiteral("absolute")});
}

QString MpvItem::cycleSubtitles()
{
    if (m_subtitles.isEmpty()) return {};
    m_subtitle = m_subtitle + 1 >= m_subtitles.size() ? -1 : m_subtitle + 1;
    if (m_subtitle < 0) {
        mpv_set_property_string(m_mpv, "sid", "no");
        return {};
    }
    const QVariantMap &t = m_subtitles.at(m_subtitle);
    mpv_set_property_string(m_mpv, "sid", t.value(QStringLiteral("id")).toString().toUtf8().constData());
    QString name = t.value(QStringLiteral("title")).toString();
    if (name.isEmpty()) name = t.value(QStringLiteral("lang")).toString();
    return name.isEmpty() ? QStringLiteral("track %1").arg(m_subtitle + 1) : name;
}

void MpvItem::hold(bool awake)
{
    QDBusInterface saver(QStringLiteral("org.freedesktop.ScreenSaver"), QStringLiteral("/org/freedesktop/ScreenSaver"),
                         QStringLiteral("org.freedesktop.ScreenSaver"), QDBusConnection::sessionBus());
    if (awake && !m_cookie && saver.isValid()) {
        QDBusReply<uint> r = saver.call(QStringLiteral("Inhibit"), QStringLiteral("flickr"), QStringLiteral("Playing"));
        if (r.isValid()) m_cookie = r.value();
    } else if (!awake && m_cookie) {
        saver.call(QStringLiteral("UnInhibit"), m_cookie);   // a gone Steam has let go already
        m_cookie = 0;
    }
}

void MpvItem::drain()
{
    while (m_mpv) {
        mpv_event *e = mpv_wait_event(m_mpv, 0);
        if (e->event_id == MPV_EVENT_NONE) break;
        if (e->event_id == MPV_EVENT_PROPERTY_CHANGE) {
            auto *p = static_cast<mpv_event_property *>(e->data);
            handleProperty(p->name, p->format == MPV_FORMAT_NODE ? fromNode(*static_cast<mpv_node *>(p->data)) : QVariant());
        } else if (e->event_id == MPV_EVENT_LOG_MESSAGE) {
            auto *m = static_cast<mpv_event_log_message *>(e->data);
            qInfo("mpv [%s] %s", m->prefix, QByteArray(m->text).trimmed().constData());
        } else if (e->event_id == MPV_EVENT_FILE_LOADED) {
            m_loading = m_cache || m_seeking;
            emit loadingChanged();
            emit loaded();
        } else if (e->event_id == MPV_EVENT_END_FILE) {
            auto *end = static_cast<mpv_event_end_file *>(e->data);
            if (end->reason == MPV_END_FILE_REASON_ERROR) {
                m_loading = false;
                emit loadingChanged();
                emit failed(QString::fromUtf8(mpv_error_string(end->error)));
            }
        }
    }
}

void MpvItem::handleProperty(const char *name, const QVariant &v)
{
    const QByteArray n(name);
    const bool wasPlaying = m_playing, wasLoading = m_loading;
    if (n == "time-pos") { m_position = v.toDouble(); emit positionChanged(); }
    else if (n == "duration") { m_duration = v.toDouble(); emit durationChanged(); }
    else if (n == "pause") m_paused = v.toBool();
    else if (n == "idle-active") m_idle = v.toBool();
    else if (n == "paused-for-cache") m_cache = v.toBool();
    else if (n == "seeking") m_seeking = v.toBool();
    else if (n == "seekable") { m_seekable = v.toBool(); emit seekableChanged(); }
    else if (n == "eof-reached") {
        if (v.toBool() && !m_ended) { m_ended = true; emit ended(); }
    } else if (n == "video-params/w" || n == "video-params/h") {
        QSize s = m_videoSize;
        if (n == "video-params/w") s.setWidth(v.toInt()); else s.setHeight(v.toInt());
        if (s != m_videoSize) { m_videoSize = s; emit videoSizeChanged(); }
    } else if (n == "hwdec-current") { m_hwdec = v.toString(); emit hwdecChanged(); }
    else if (n == "frame-drop-count") { m_dropped = v.toInt(); emit droppedFramesChanged(); }
    else if (n == "vo-delayed-frame-count") { m_delayed = v.toInt(); emit droppedFramesChanged(); }
    else if (n == "track-list") {
        m_subtitles.clear();
        for (const QVariant &t : v.toList()) {
            QVariantMap m = t.toMap();
            if (m.value(QStringLiteral("type")).toString() == QLatin1String("sub")) m_subtitles.append(m);
        }
        m_subtitle = -1;
        for (int i = 0; i < m_subtitles.size(); i++)
            if (m_subtitles.at(i).value(QStringLiteral("selected")).toBool()) m_subtitle = i;
        emit tracksChanged();
    }
    m_playing = !m_paused && !m_idle && !m_ended;
    if (n != "pause" && n != "idle-active" && n != "eof-reached" && n != "paused-for-cache" && n != "seeking") return;
    m_loading = m_cache || m_seeking;
    if (m_playing != wasPlaying) { hold(m_playing); emit playingChanged(); }
    if (m_loading != wasLoading) emit loadingChanged();
}
