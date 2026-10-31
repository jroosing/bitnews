#!/bin/sh
# Builds libbitnews_lt with the system C++ compiler against the system
# libtorrent-rasterbar (ADR-0014): libtorrent is C++ and its ABI is the
# distro's libstdc++ one, so `zig c++` (which uses its own libc++) must not
# compile code that touches it. pkg-config supplies the defines that have to
# match the installed library. Usage: build-lt.sh <output .so/.dylib>
set -eu
out=$1
here=$(cd "$(dirname "$0")" && pwd)
# Homebrew keeps Boost and friends under its prefix, which the compiler does
# not search by itself; on Debian the packages are in the default paths.
extra=
if command -v brew >/dev/null 2>&1; then
	prefix=$(brew --prefix)
	extra="-I$prefix/include -L$prefix/lib"
fi
install_name=
case "$(uname -s)" in
	Darwin) install_name="-Wl,-install_name,@rpath/$(basename "$out")" ;;
esac
# shellcheck disable=SC2086
exec "${CXX:-c++}" -std=c++17 -O2 -fPIC -shared -Wall -Wextra $extra $install_name \
	"$here/bitnews_lt.cpp" -o "$out" $(pkg-config --cflags --libs libtorrent-rasterbar)
