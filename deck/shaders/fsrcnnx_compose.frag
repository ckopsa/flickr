#version 440
// FSRCNNX's aggregation, with the colour put back. The last pass left four
// luma values per frame texel, one per quarter of it; this picks the quarter
// this 2x pixel is in (the hook's own rule, from FSRCNNX_x2_8-0-4-1.glsl,
// (c) 2017-2021 igv, LGPL-3.0). The frame's RGB, sampled here, keeps its
// chroma: R', G' and B' each carry luma with weight one, so replacing Y' is
// adding the difference to all three.

layout(location = 0) in vec2 qt_TexCoord0;
layout(location = 0) out vec4 fragColor;

layout(std140, binding = 0) uniform buf {
    mat4 qt_Matrix;
    float qt_Opacity;
    vec2 lumaSize;
    vec2 pt;
};
layout(binding = 1) uniform sampler2D subconv1;
layout(binding = 2) uniform sampler2D source;

void main()
{
    vec2 fcoord = fract(qt_TexCoord0 * lumaSize);
    vec2 base = qt_TexCoord0 + (vec2(0.5) - fcoord) * pt;
    vec4 res = texture(subconv1, base);
    bool right = fcoord.x >= 0.5, below = fcoord.y >= 0.5;
    float y = right ? (below ? res.w : res.z) : (below ? res.y : res.x);

    vec3 rgb = texture(source, qt_TexCoord0).rgb;
    rgb += y - dot(rgb, vec3(0.2126, 0.7152, 0.0722));
    fragColor = vec4(clamp(rgb, 0.0, 1.0), 1.0);
}
