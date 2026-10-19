#!/bin/sh
# The player's shaders, baked once: Qt Quick reads a ShaderEffect's shader as
# a .qsb (SPIR-V plus the GLSL, HLSL and MSL cross-compiled from it), and the
# baked files are committed, so the Deck still runs flickr with nothing built.
# Run this after touching a .frag. qsb is Qt 6's own (qt6-shadertools).
here=$(dirname "$(readlink -f "$0")")
qsb=${QSB:-/usr/lib/qt6/bin/qsb}
for f in "$here"/*.frag; do
    "$qsb" --glsl "100 es,120,150" --hlsl 50 --msl 12 -o "$f.qsb" "$f" || exit 1
done
