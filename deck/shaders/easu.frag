#version 440
// FSR 1 EASU — AMD FidelityFX Super Resolution 1.0, Edge Adaptive Spatial
// Upsampling: the float path of ffx_fsr1.h (MIT, (c) Advanced Micro Devices)
// written out for a Qt Quick ShaderEffect. `source` is the frame at its own
// size (`inSize`, in pixels); the item it draws is the size it is wanted at.
// Twelve taps around the nearest texel, a direction and a length read from
// their luma, then a Lanczos-like lobe stretched along the edge, clamped to
// the four nearest so it never rings.
//
//      b c
//    e f g h      f is the texel at or above-left of where this pixel lands
//    i j k l
//      n o

layout(location = 0) in vec2 qt_TexCoord0;
layout(location = 0) out vec4 fragColor;

layout(std140, binding = 0) uniform buf {
    mat4 qt_Matrix;
    float qt_Opacity;
    vec2 inSize;
};
layout(binding = 1) uniform sampler2D source;

vec3 at(vec2 fp, vec2 off) { return texture(source, (fp + off + 0.5) / inSize).rgb; }
float luma(vec3 c) { return c.b * 0.5 + (c.r * 0.5 + c.g); }

// One of the four bilinear corners' say in the edge's direction and length.
void edge(inout vec2 dir, inout float len, float w,
          float lA, float lB, float lC, float lD, float lE)
{
    float dc = lD - lC, cb = lC - lB;
    float lenX = 1.0 / max(max(abs(dc), abs(cb)), 1e-5);
    float dirX = lD - lB;
    dir.x += dirX * w;
    lenX = clamp(abs(dirX) * lenX, 0.0, 1.0);
    len += lenX * lenX * w;

    float ec = lE - lC, ca = lC - lA;
    float lenY = 1.0 / max(max(abs(ec), abs(ca)), 1e-5);
    float dirY = lE - lA;
    dir.y += dirY * w;
    lenY = clamp(abs(dirY) * lenY, 0.0, 1.0);
    len += lenY * lenY * w;
}

void tap(inout vec3 aC, inout float aW, vec2 off, vec2 dir, vec2 len2,
         float lob, float clp, vec3 c)
{
    vec2 v = vec2(off.x * dir.x + off.y * dir.y, off.x * -dir.y + off.y * dir.x) * len2;
    float d2 = min(v.x * v.x + v.y * v.y, clp);
    float wB = 2.0 / 5.0 * d2 - 1.0;
    float wA = lob * d2 - 1.0;
    wB *= wB;
    wA *= wA;
    wB = 25.0 / 16.0 * wB - (25.0 / 16.0 - 1.0);
    float w = wB * wA;
    aC += c * w;
    aW += w;
}

void main()
{
    vec2 pp = qt_TexCoord0 * inSize - 0.5;
    vec2 fp = floor(pp);
    pp -= fp;

    vec3 b = at(fp, vec2( 0.0, -1.0)), c = at(fp, vec2( 1.0, -1.0));
    vec3 e = at(fp, vec2(-1.0,  0.0)), f = at(fp, vec2( 0.0,  0.0));
    vec3 g = at(fp, vec2( 1.0,  0.0)), h = at(fp, vec2( 2.0,  0.0));
    vec3 i = at(fp, vec2(-1.0,  1.0)), j = at(fp, vec2( 0.0,  1.0));
    vec3 k = at(fp, vec2( 1.0,  1.0)), l = at(fp, vec2( 2.0,  1.0));
    vec3 n = at(fp, vec2( 0.0,  2.0)), o = at(fp, vec2( 1.0,  2.0));

    float bL = luma(b), cL = luma(c), eL = luma(e), fL = luma(f);
    float gL = luma(g), hL = luma(h), iL = luma(i), jL = luma(j);
    float kL = luma(k), lL = luma(l), nL = luma(n), oL = luma(o);

    vec2 dir = vec2(0.0);
    float len = 0.0;
    edge(dir, len, (1.0 - pp.x) * (1.0 - pp.y), bL, eL, fL, gL, jL);
    edge(dir, len, pp.x * (1.0 - pp.y),         cL, fL, gL, hL, kL);
    edge(dir, len, (1.0 - pp.x) * pp.y,         fL, iL, jL, kL, nL);
    edge(dir, len, pp.x * pp.y,                 gL, jL, kL, lL, oL);

    float dirR = dot(dir, dir);
    bool flat_ = dirR < 1.0 / 32768.0;
    dirR = flat_ ? 1.0 : inversesqrt(dirR);
    dir.x = flat_ ? 1.0 : dir.x;
    dir *= dirR;

    len = len * 0.5;
    len *= len;
    float stretch = dot(dir, dir) / max(abs(dir.x), abs(dir.y));
    vec2 len2 = vec2(1.0 + (stretch - 1.0) * len, 1.0 - 0.5 * len);
    float lob = 0.5 + ((1.0 / 4.0 - 0.04) - 0.5) * len;
    float clp = 1.0 / lob;

    vec3 mn = min(min(f, g), min(j, k));
    vec3 mx = max(max(f, g), max(j, k));

    vec3 aC = vec3(0.0);
    float aW = 0.0;
    tap(aC, aW, vec2( 0.0, -1.0) - pp, dir, len2, lob, clp, b);
    tap(aC, aW, vec2( 1.0, -1.0) - pp, dir, len2, lob, clp, c);
    tap(aC, aW, vec2(-1.0,  1.0) - pp, dir, len2, lob, clp, i);
    tap(aC, aW, vec2( 0.0,  1.0) - pp, dir, len2, lob, clp, j);
    tap(aC, aW, vec2( 0.0,  0.0) - pp, dir, len2, lob, clp, f);
    tap(aC, aW, vec2(-1.0,  0.0) - pp, dir, len2, lob, clp, e);
    tap(aC, aW, vec2( 1.0,  1.0) - pp, dir, len2, lob, clp, k);
    tap(aC, aW, vec2( 2.0,  1.0) - pp, dir, len2, lob, clp, l);
    tap(aC, aW, vec2( 2.0,  0.0) - pp, dir, len2, lob, clp, h);
    tap(aC, aW, vec2( 1.0,  0.0) - pp, dir, len2, lob, clp, g);
    tap(aC, aW, vec2( 1.0,  2.0) - pp, dir, len2, lob, clp, o);
    tap(aC, aW, vec2( 0.0,  2.0) - pp, dir, len2, lob, clp, n);

    fragColor = vec4(min(mx, max(mn, aC / aW)), 1.0) * qt_Opacity;
}
