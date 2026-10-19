// FSR 1 for mpv: AMD FidelityFX Super Resolution 1.0 (the float path of
// ffx_fsr1.h, MIT, (c) Advanced Micro Devices), as two user-shader passes on
// MAIN. EASU stretches the frame along its edges straight to the screen's
// size; RCAS then sharpens what that softened. Neither runs unless the frame
// is smaller than the screen. RCAS sharpness is 1.0 stops, not AMD's 0.2:
// at 0.2 it sharpens a stream's compression blocks too (scored at 4K against
// the original, a compressed 720p photo fell below bilinear; at 1.0 it ties).

//!DESC FSR 1 EASU
//!HOOK MAIN
//!BIND HOOKED
//!SAVE EASUTEX
//!WIDTH OUTPUT.w
//!HEIGHT OUTPUT.h
//!WHEN OUTPUT.w MAIN.w 1.05 * > OUTPUT.h MAIN.h 1.05 * > *
//!COMPONENTS 4

vec3 easu_at(vec2 fp, vec2 off) { return HOOKED_tex((fp + off + 0.5) * HOOKED_pt).rgb; }
float easu_luma(vec3 c) { return c.b * 0.5 + (c.r * 0.5 + c.g); }

// One of the four bilinear corners' say in the edge's direction and length.
void easu_edge(inout vec2 dir, inout float len, float w,
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

void easu_tap(inout vec3 aC, inout float aW, vec2 off, vec2 dir, vec2 len2,
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

vec4 hook()
{
    vec2 pp = HOOKED_pos * HOOKED_size - 0.5;
    vec2 fp = floor(pp);
    pp -= fp;

    vec3 b = easu_at(fp, vec2( 0.0, -1.0)), c = easu_at(fp, vec2( 1.0, -1.0));
    vec3 e = easu_at(fp, vec2(-1.0,  0.0)), f = easu_at(fp, vec2( 0.0,  0.0));
    vec3 g = easu_at(fp, vec2( 1.0,  0.0)), h = easu_at(fp, vec2( 2.0,  0.0));
    vec3 i = easu_at(fp, vec2(-1.0,  1.0)), j = easu_at(fp, vec2( 0.0,  1.0));
    vec3 k = easu_at(fp, vec2( 1.0,  1.0)), l = easu_at(fp, vec2( 2.0,  1.0));
    vec3 n = easu_at(fp, vec2( 0.0,  2.0)), o = easu_at(fp, vec2( 1.0,  2.0));

    float bL = easu_luma(b), cL = easu_luma(c), eL = easu_luma(e), fL = easu_luma(f);
    float gL = easu_luma(g), hL = easu_luma(h), iL = easu_luma(i), jL = easu_luma(j);
    float kL = easu_luma(k), lL = easu_luma(l), nL = easu_luma(n), oL = easu_luma(o);

    vec2 dir = vec2(0.0);
    float len = 0.0;
    easu_edge(dir, len, (1.0 - pp.x) * (1.0 - pp.y), bL, eL, fL, gL, jL);
    easu_edge(dir, len, pp.x * (1.0 - pp.y),         cL, fL, gL, hL, kL);
    easu_edge(dir, len, (1.0 - pp.x) * pp.y,         fL, iL, jL, kL, nL);
    easu_edge(dir, len, pp.x * pp.y,                 gL, jL, kL, lL, oL);

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
    easu_tap(aC, aW, vec2( 0.0, -1.0) - pp, dir, len2, lob, clp, b);
    easu_tap(aC, aW, vec2( 1.0, -1.0) - pp, dir, len2, lob, clp, c);
    easu_tap(aC, aW, vec2(-1.0,  1.0) - pp, dir, len2, lob, clp, i);
    easu_tap(aC, aW, vec2( 0.0,  1.0) - pp, dir, len2, lob, clp, j);
    easu_tap(aC, aW, vec2( 0.0,  0.0) - pp, dir, len2, lob, clp, f);
    easu_tap(aC, aW, vec2(-1.0,  0.0) - pp, dir, len2, lob, clp, e);
    easu_tap(aC, aW, vec2( 1.0,  1.0) - pp, dir, len2, lob, clp, k);
    easu_tap(aC, aW, vec2( 2.0,  1.0) - pp, dir, len2, lob, clp, l);
    easu_tap(aC, aW, vec2( 2.0,  0.0) - pp, dir, len2, lob, clp, h);
    easu_tap(aC, aW, vec2( 1.0,  0.0) - pp, dir, len2, lob, clp, g);
    easu_tap(aC, aW, vec2( 1.0,  2.0) - pp, dir, len2, lob, clp, o);
    easu_tap(aC, aW, vec2( 0.0,  2.0) - pp, dir, len2, lob, clp, n);

    return vec4(min(mx, max(mn, aC / aW)), 1.0);
}

//!DESC FSR 1 RCAS
//!HOOK MAIN
//!BIND EASUTEX
//!WIDTH OUTPUT.w
//!HEIGHT OUTPUT.h
//!WHEN OUTPUT.w MAIN.w 1.05 * > OUTPUT.h MAIN.h 1.05 * > *
//!COMPONENTS 4

#define RCAS_SHARPNESS 1.0
#define RCAS_LIMIT (0.25 - 1.0 / 16.0)

vec4 hook()
{
    vec3 b = EASUTEX_texOff(vec2( 0.0, -1.0)).rgb;
    vec3 d = EASUTEX_texOff(vec2(-1.0,  0.0)).rgb;
    vec3 e = EASUTEX_texOff(vec2( 0.0,  0.0)).rgb;
    vec3 f = EASUTEX_texOff(vec2( 1.0,  0.0)).rgb;
    vec3 h = EASUTEX_texOff(vec2( 0.0,  1.0)).rgb;

    vec3 mn4 = min(min(b, d), min(f, h));
    vec3 mx4 = max(max(b, d), max(f, h));
    vec3 hitMin = mn4 / max(4.0 * mx4, vec3(1e-5));
    vec3 hitMax = (1.0 - mx4) / min(4.0 * mn4 - 4.0, vec3(-1e-5));
    vec3 lobeC = max(-hitMin, hitMax);
    float lobe = max(-RCAS_LIMIT, min(max(lobeC.r, max(lobeC.g, lobeC.b)), 0.0)) * exp2(-RCAS_SHARPNESS);

    vec3 c = (lobe * (b + d + f + h) + e) / (4.0 * lobe + 1.0);
    return vec4(clamp(c, 0.0, 1.0), 1.0);
}
