#!/usr/bin/env bash

set -euo pipefail

printf '%s\n' >&2 \
    'Linux AppImage packaging is disabled: the previous helper downloaded and executed an unpinned moving linuxdeploy build.' \
    'Re-enable it only with an immutable, verified tool digest plus signing/provenance and clean-machine package qualification.'
exit 1
