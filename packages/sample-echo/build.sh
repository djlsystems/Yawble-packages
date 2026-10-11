#!/bin/sh
# Lays out one installable version of the plugin in <out>: the manifest, the launcher it names, the
# skill, and the framework-dependent build of src/ in lib/. Needs the .NET SDK; the manifest
# `requires` dotnet from the image, and one build serves both processors.
#   ./build.sh ~/plugins-build/sample-echo/0.1.0
set -e
here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build.sh <out folder>}
mkdir -p "$out/lib" "$out/skills"
out=$(cd "$out" && pwd)
# Intermediate files go to a temporary folder, so the source folder stays clean; the build is
# deterministic and maps its paths away, so the same commit gives the same bytes.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM
DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1 dotnet publish "$here/src/SampleEcho.csproj" \
  -c Release --nologo -v quiet --artifacts-path "$work" -o "$out/lib" \
  -p:ContinuousIntegrationBuild=true -p:DebugType=none -p:GenerateDocumentationFile=false >&2
cp "$here/plugin.json" "$here/sample-echo" "$out/"
chmod 755 "$out/sample-echo"
cp "$here/skills/sample-echo.md" "$out/skills/"
