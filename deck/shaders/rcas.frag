#version 440
// FSR 1 RCAS — Robust Contrast Adaptive Sharpening, the pass that follows
// EASU: the float path of ffx_fsr1.h (MIT, (c) Advanced Micro Devices). It
// runs at the output size over EASU's picture. The lobe is the most each of
// the four neighbours may be subtracted without clipping, so it sharpens what
// EASU softened and stops short of a halo. `sharpness` is in stops: 0 is the
// most, each +1 halves it.
//
//      b
//    d e f
//      h

layout(location = 0) in vec2 qt_TexCoord0;
layout(location = 0) out vec4 fragColor;

layout(std140, binding = 0) uniform buf {
    mat4 qt_Matrix;
    float qt_Opacity;
    vec2 outSize;
    float sharpness;
};
layout(binding = 1) uniform sampler2D source;

const float LIMIT = 0.25 - 1.0 / 16.0;

void main()
{
    vec2 px = 1.0 / outSize;
    vec3 b = texture(source, qt_TexCoord0 + vec2(0.0, -px.y)).rgb;
    vec3 d = texture(source, qt_TexCoord0 + vec2(-px.x, 0.0)).rgb;
    vec3 e = texture(source, qt_TexCoord0).rgb;
    vec3 f = texture(source, qt_TexCoord0 + vec2(px.x, 0.0)).rgb;
    vec3 h = texture(source, qt_TexCoord0 + vec2(0.0, px.y)).rgb;

    vec3 mn4 = min(min(b, d), min(f, h));
    vec3 mx4 = max(max(b, d), max(f, h));
    vec3 hitMin = mn4 / max(4.0 * mx4, vec3(1e-5));
    vec3 hitMax = (1.0 - mx4) / min(4.0 * mn4 - 4.0, vec3(-1e-5));
    vec3 lobeC = max(-hitMin, hitMax);
    float lobe = max(-LIMIT, min(max(lobeC.r, max(lobeC.g, lobeC.b)), 0.0)) * exp2(-sharpness);

    vec3 c = (lobe * (b + d + f + h) + e) / (4.0 * lobe + 1.0);
    fragColor = vec4(clamp(c, 0.0, 1.0), 1.0) * qt_Opacity;
}
