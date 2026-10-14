#!/bin/sh
# Build the tcdpool image.
#
#   scripts/build-image.sh [TARGET] [TAG]
#
# TARGET is final (default), tcd-test or another Dockerfile stage.
#
# Environment:
#   MD380_CORE  path of d02032-core.img (the md380 SRAM core image of
#               md380_vocoder_dynarmic). This repository does not carry
#               it, because it is a copy of the memory of a TYT radio.
#               Default: ~/src/urfd/src/md380_vocoder_dynarmic/firmware/d02032-core.img
#   WORK        a directory for the build contexts (default: a new
#               temporary directory)
#   DOCKER      the docker command (default: docker)
set -eu

target=${1:-final}
case $target in
final) tag=${2:-tcdpool:dev} ;;
*) tag=${2:-tcdpool-$target:dev} ;;
esac
root=$(cd "$(dirname "$0")/.." && pwd)
core=${MD380_CORE:-$HOME/src/urfd/src/md380_vocoder_dynarmic/firmware/d02032-core.img}
docker=${DOCKER:-docker}
work=${WORK:-$(mktemp -d)}

[ -f "$core" ] || { echo "no md380 core image at $core (set MD380_CORE)" >&2; exit 1; }
mkdir -p "$work/md380core" "$work/goproxy"
cp "$core" "$work/md380core/d02032-core.img"

# A GOPROXY directory with only the modules in go.sum, from the local
# module cache (go mod download fills the cache if it is empty).
cd "$root"
go mod download
cache=$(go env GOMODCACHE)/cache/download
go list -m -f '{{if not .Main}}{{.Path}} {{.Version}}{{end}}' all | while read -r path version; do
	[ -n "$path" ] || continue
	esc=$(printf '%s' "$path" | sed 's/\([A-Z]\)/!\1/g' | tr 'A-Z' 'a-z')
	mkdir -p "$work/goproxy/$esc/@v"
	for ext in info mod zip; do
		f="$cache/$esc/@v/$version.$ext"
		[ -f "$f" ] && cp "$f" "$work/goproxy/$esc/@v/"
	done
	printf '%s\n' "$version" >>"$work/goproxy/$esc/@v/list"
done

$docker build \
	--build-context md380core="$work/md380core" \
	--build-context goproxy="$work/goproxy" \
	--target "$target" -t "$tag" "$root"
echo "built $tag"
