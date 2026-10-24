#!/bin/sh
# Builds the plugin package packages/twilio-plugin into <out>: this team ships that plugin as it is, so
# its source lives there and nowhere else.
#   ./build.sh <out folder>
set -e
here=$(cd "$(dirname "$0")" && pwd)
exec sh "$here/../../../twilio-plugin/build.sh" "${1:?usage: build.sh <out folder>}"
