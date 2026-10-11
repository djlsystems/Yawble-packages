#!/bin/sh
# Builds the published twilio plugin, packages/twilio/plugins/twilio, into <out>: this team hires
# that plugin as it is, so its source lives there and nowhere else.
#   ./build.sh <out folder>
set -e
here=$(cd "$(dirname "$0")" && pwd)
exec sh "$here/../../../twilio/plugins/twilio/build.sh" "${1:?usage: build.sh <out folder>}"
