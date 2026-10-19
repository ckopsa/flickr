# The Deck client's one compiled piece: libmpv as a QML item (Flickr.Mpv)
# and the small program that runs Main.qml with it. Built inside the
# Flatpak (deck/flatpak/), against the KDE runtime's Qt and the libmpv the
# manifest builds; the QML and the helper stay as they are, files.
cmake_minimum_required(VERSION 3.21)
project(flickr-deck-ui LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_AUTOMOC ON)

find_package(Qt6 6.5 REQUIRED COMPONENTS Gui Quick OpenGL DBus)
find_package(PkgConfig REQUIRED)
pkg_check_modules(MPV REQUIRED IMPORTED_TARGET mpv)

qt_standard_project_setup(REQUIRES 6.5)

qt_add_executable(flickr-deck-ui main.cpp)

# Linked into the program, not loaded as a plugin: one file, nothing for
# QML_IMPORT_PATH to find.
qt_add_qml_module(flickr-deck-ui
    URI Flickr.Mpv
    VERSION 1.0
    SOURCES mpvitem.h mpvitem.cpp
)

target_link_libraries(flickr-deck-ui PRIVATE Qt6::Gui Qt6::Quick Qt6::OpenGL Qt6::DBus PkgConfig::MPV)

install(TARGETS flickr-deck-ui RUNTIME DESTINATION bin)
