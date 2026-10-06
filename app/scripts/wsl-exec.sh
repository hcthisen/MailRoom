#!/usr/bin/env bash
# go test -exec wrapper for Windows hosts where app-control policy blocks
# freshly built .exe test binaries. Tests are cross-compiled for Linux
# (GOOS=linux) and run inside WSL instead, without a shell (--exec), so
# arguments such as -run 'A|B' arrive unchanged. Usage: see scripts/test.sh.
set -euo pipefail

# to_wsl converts any Git Bash or Windows path to /mnt/<drive>/...
# cygpath -m is used because Git Bash mounts some folders (e.g. %TEMP% as /tmp).
to_wsl() {
  local p
  p=$(cygpath -m "$1")
  if [[ "$p" =~ ^([a-zA-Z]):/(.*)$ ]]; then
    echo "/mnt/${BASH_REMATCH[1],,}/${BASH_REMATCH[2]}"
  else
    echo "$p"
  fi
}

bin="$1"
shift
distro="${MAILROOM_WSL_DISTRO:-Ubuntu}"
MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*" exec wsl.exe -d "$distro" --cd "$(to_wsl "$PWD")" --exec "$(to_wsl "$bin")" "$@"
