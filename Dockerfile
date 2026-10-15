# syntax=docker/dockerfile:1
#
# tcdpool image: the tcdpool server and the nostar tcd transcoder
# (branch tcd-nohw, software vocoders only), with the patches in
# patches/tcd-nohw applied.
#
# Build (see scripts/build-image.sh):
#   docker build --build-context md380core=<dir with d02032-core.img> \
#     --build-context goproxy=<GOPROXY dir> -t tcdpool:dev .
#
# Targets:
#   final (default)  tcdpool + patched tcd, runs as a non-root user
#   gobuild          tcdpool and tcdpool-client (static Go binaries)
#   tcd-test         wire tests for the patched and the unpatched tcd

ARG DEBIAN_IMAGE=debian:trixie@sha256:913f6706df59a68922d1dd08f78c2476560a8d367897200a6005b00e5f67c2d5
ARG DEBIAN_SLIM_IMAGE=debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f
ARG GOLANG_IMAGE=golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e

# Pinned git sources.
ARG TCD_REPO=https://github.com/nostar/tcd.git
ARG TCD_COMMIT=e347df69c0775a1f3e16298e098bfba313186ab6
ARG DYNARMIC_REPO=https://github.com/yuzu-mirror/dynarmic.git
ARG DYNARMIC_COMMIT=9d4582339990d4eae53f1dc7160686920fc2075c
# SHA-256 of the md380 SRAM core image (from md380_vocoder_dynarmic).
ARG MD380_CORE_SHA256=d40398183e9aa8db6288a62d9a0e0c233cb143ef5633680abd829cc03c719f66

########################################################################
FROM ${DEBIAN_IMAGE} AS base
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential cmake git ca-certificates curl unzip python3 xxd \
        libboost-dev \
    && rm -rf /var/lib/apt/lists/*

########################################################################
# IMBE vocoder (OP25, GPL-3.0-or-later), from third_party/imbe_vocoder.
FROM base AS imbe
COPY third_party/imbe_vocoder /build/imbe_vocoder
WORKDIR /build/imbe_vocoder
RUN make clean && make \
    && mkdir -p /out/lib /out/include \
    && cp libimbe_vocoder.a /out/lib/ && cp *.h /out/include/

########################################################################
# AMBE+2 through the md380 firmware on dynarmic.
FROM base AS md380
ARG DYNARMIC_REPO
ARG DYNARMIC_COMMIT
ARG MD380_CORE_SHA256
RUN git init -q /build/dynarmic && cd /build/dynarmic \
    && git remote add origin "${DYNARMIC_REPO}" \
    && git fetch -q --depth 1 origin "${DYNARMIC_COMMIT}" \
    && git checkout -q FETCH_HEAD \
    && test "$(git rev-parse HEAD)" = "${DYNARMIC_COMMIT}"
COPY third_party/md380_vocoder_dynarmic /build/md380_vocoder_dynarmic
COPY --from=md380core d02032-core.img /build/md380_vocoder_dynarmic/firmware/d02032-core.img
RUN echo "${MD380_CORE_SHA256}  /build/md380_vocoder_dynarmic/firmware/d02032-core.img" | sha256sum -c -
WORKDIR /build/md380_vocoder_dynarmic/build
# download.sh fetches the TYT MD-380 firmware 2.32 from md380.org and
# checks its SHA-256 before it unwraps it.
RUN cmake -DFETCHCONTENT_SOURCE_DIR_DYNARMIC=/build/dynarmic -DCMAKE_BUILD_TYPE=Release .. \
    && make -j"$(nproc)" \
    && sh -x ../makelib.sh \
    && mkdir -p /out/lib /out/include \
    && cp libmd380_vocoder.a /out/lib/ && cp ../md380_vocoder.h /out/include/

########################################################################
# nostar tcd, branch tcd-nohw, at the pinned commit.
FROM base AS tcd-src
ARG TCD_REPO
ARG TCD_COMMIT
RUN git init -q /src/tcd-upstream && cd /src/tcd-upstream \
    && git remote add origin "${TCD_REPO}" \
    && git fetch -q --depth 1 origin "${TCD_COMMIT}" \
    && git checkout -q FETCH_HEAD \
    && test "$(git rev-parse HEAD)" = "${TCD_COMMIT}" \
    && cp config/tcd.mk tcd.mk \
    && rm -rf .git
COPY patches/tcd-nohw /src/patches
RUN cp -a /src/tcd-upstream /src/tcd \
    && cd /src/tcd \
    && for p in /src/patches/*.patch; do echo "applying $p"; patch -p1 --no-backup-if-mismatch < "$p" || exit 1; done

FROM base AS tcd-libs
COPY --from=imbe /out/ /usr/local/
COPY --from=md380 /out/ /usr/local/

# The unpatched tcd, for the link tests only.
FROM tcd-libs AS tcd-upstream
COPY --from=tcd-src /src/tcd-upstream /build/tcd
WORKDIR /build/tcd
RUN make -j"$(nproc)" && strip tcd

# The patched tcd that the image ships.
FROM tcd-libs AS tcd
COPY --from=tcd-src /src/tcd /build/tcd
WORKDIR /build/tcd
RUN make -j"$(nproc)" && strip tcd

########################################################################
# Link tests: codec unit test (shared vectors) and wiretest against the
# patched and the unpatched tcd. Run with:
#   docker run --rm tcdpool-tcd-test:dev
FROM tcd-libs AS tcd-test
COPY --from=tcd-src /src/tcd-upstream /src/tcd-upstream
COPY --from=tcd-src /src/tcd /src/tcd
COPY --from=tcd /build/tcd/tcd /usr/local/bin/tcd
COPY --from=tcd-upstream /build/tcd/tcd /usr/local/bin/tcd-upstream
COPY tests/tcd-wire /src/tests
COPY testdata/tcd-wire-vectors.txt /src/testdata/tcd-wire-vectors.txt
WORKDIR /src/tests
RUN g++ -std=c++17 -O2 -Wall -W -Werror -I/src/tcd-upstream genvectors.cpp -o /usr/local/bin/genvectors \
    && g++ -std=c++17 -O2 -Wall -W -Werror -I/src/tcd codec_test.cpp /src/tcd/TCPacketCodec.cpp /src/tcd/TCFraming.cpp /src/tcd/TranscoderPacket.cpp -o /usr/local/bin/codec_test \
    && g++ -std=c++17 -O2 -Wall -W -Werror -I/src/tcd wiretest.cpp /src/tcd/TCPacketCodec.cpp /src/tcd/TCFraming.cpp -o /usr/local/bin/wiretest \
    && g++ -std=c++17 -O2 -Wall -W -Werror md380_state.cpp -lmd380_vocoder -pthread -o /usr/local/bin/md380_state
COPY tests/tcd-wire/run.sh /usr/local/bin/run-tcd-tests
CMD ["/usr/local/bin/run-tcd-tests"]

########################################################################
# tcdpool (pure Go, no cgo). The Go modules come from the "goproxy"
# build context, a GOPROXY directory that scripts/build-image.sh makes
# from the local module cache, so the build downloads no Go code. The
# go.sum file checks each module.
FROM ${GOLANG_IMAGE} AS gobuild
COPY --from=goproxy . /goproxy
ENV GOPROXY=file:///goproxy GOFLAGS=-mod=mod GOSUMDB=off GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# VERSION is shown in GET /v1/status (scripts/build-image.sh sets it).
ARG VERSION=dev
RUN go vet ./... \
    && go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/tcdpool ./cmd/tcdpool \
    && go build -trimpath -ldflags="-s -w" -o /out/tcdpool-client ./cmd/tcdpool-client

########################################################################
# The image: tcdpool and the patched tcd, as a non-root user.
FROM ${DEBIAN_SLIM_IMAGE} AS final
RUN groupadd --system --gid 10001 tcdpool \
    && useradd --system --uid 10001 --gid tcdpool --home-dir /var/lib/tcdpool --shell /usr/sbin/nologin tcdpool \
    && mkdir -p /opt/tcdpool /etc/tcdpool /var/lib/tcdpool/run \
    && chown -R tcdpool:tcdpool /var/lib/tcdpool && chmod 0700 /var/lib/tcdpool
COPY --from=tcd /build/tcd/tcd /opt/tcdpool/tcd
COPY --from=gobuild /out/tcdpool /out/tcdpool-client /opt/tcdpool/
COPY deploy/tcdpool.example.json /etc/tcdpool/tcdpool.json
# Smoke check: tcd has all its shared libraries in this image.
RUN ! ldd /opt/tcdpool/tcd | grep -q 'not found' && /opt/tcdpool/tcdpool check-config -config /etc/tcdpool/tcdpool.json
ENV PATH=/opt/tcdpool:$PATH
USER tcdpool
WORKDIR /var/lib/tcdpool
EXPOSE 8443
ENTRYPOINT ["/opt/tcdpool/tcdpool"]
CMD ["serve", "-config", "/etc/tcdpool/tcdpool.json"]
