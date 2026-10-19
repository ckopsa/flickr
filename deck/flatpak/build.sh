#!/bin/sh
# Build the Deck client's Flatpak and install it for this user. Needs, once:
#   flatpak install --user flathub org.kde.Sdk//6.10 org.kde.Platform//6.10 org.flatpak.Builder
# The first build compiles FFmpeg, libplacebo and mpv (the longest part);
# later ones only what changed. Build state stays out of the repo.
here=$(dirname "$(readlink -f "$0")")
cache=${XDG_CACHE_HOME:-$HOME/.cache}/flickr-deck-flatpak
mkdir -p "$cache"
exec flatpak run --user org.flatpak.Builder --user --install --force-clean \
    --state-dir="$cache/state" "$cache/build" "$here/dev.kopsa.flickr.Deck.yml" "$@"
