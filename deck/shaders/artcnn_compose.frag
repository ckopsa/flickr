#version 440
// ArtCNN's depth-to-space, with the colour put back. The last layer left
// four luma values per frame texel, one per quarter of it, in row order —
// the hook's own rule, from ArtCNN_C4F16_DS.glsl ((c) 2024 Joao Chrisostomo,
// Kacper Michajłow, MIT). The frame's RGB keeps its chroma, as in
// fsrcnnx_compose.frag: replacing Y' is adding the difference to R', G', B'.

layout(location = 0) in vec2 qt_TexCoord0;
layout(location = 0) out vec4 fragColor;

layout(std140, binding = 0) uniform buf {
    mat4 qt_Matrix;
    float qt_Opacity;
    vec2 lumaSize;
    vec2 pt;
};
layout(binding = 1) uniform sampler2D subpixels;
layout(binding = 2) uniform sampler2D source;

void main()
{
    vec2 fcoord = fract(qt_TexCoord0 * lumaSize);
    vec4 res = texture(subpixels, qt_TexCoord0 + (vec2(0.5) - fcoord) * pt);
    bool right = fcoord.x >= 0.5, below = fcoord.y >= 0.5;
    float y = clamp(below ? (right ? res.w : res.z) : (right ? res.y : res.x), 0.0, 1.0);

    vec3 rgb = texture(source, qt_TexCoord0).rgb;
    rgb += y - dot(rgb, vec3(0.2126, 0.7152, 0.0722));
    fragColor = vec4(clamp(rgb, 0.0, 1.0), 1.0);
}
