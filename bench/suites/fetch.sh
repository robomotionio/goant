#!/bin/sh
# Fetches the third-party benchmark suites goant-bench can run. They are not
# vendored: they are large, they are not ours, and pinning a commit here is
# enough to make a run reproducible.
set -e
dir=$(dirname "$0")

fetch() { # name url commit
  if [ -d "$dir/$1" ]; then echo "$1: already present"; return; fi
  echo "$1: cloning"
  git clone -q "$2" "$dir/$1"
  git -C "$dir/$1" checkout -q "$3"
  echo "$1: at $3"
}

# Octane 2.0 — the V8 team's suite. Retired as a V8 optimisation target, still
# the most portable cross-engine score there is: pure JavaScript, no host APIs
# beyond Date.now.
fetch octane https://github.com/chromium/octane 570ad1ccfe86e3eecba0636c8f932ac08edec517
