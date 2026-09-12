# Builds the plugin .so, then ships it in a stage whose only job is to hand the
# artefact to a bifrost container over a shared volume.
#
# Unlike the other templates here this image has no runtime of its own — a .so
# is not a program. The point is reproducing the ABI constraints: the builder
# pins the SAME Go version as the host binary, because -buildmode=plugin bakes
# a hash of every shared package and a different compiler is enough to make
# plugin.Open refuse the result.
#
# ARG so the compose stand and CI can pin one version in one place.
ARG GO_VERSION=1.27.0
ARG PLUGIN=bifrost-plugin

FROM golang:${GO_VERSION}-bookworm AS build
ARG PLUGIN
WORKDIR /src

# gcc is not optional: -buildmode=plugin requires cgo.
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc libc6-dev \
 && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=1
RUN go build -buildmode=plugin -o /out/${PLUGIN}.so . \
 && go build -o /out/loader ./ci/loader

# Fail the image build rather than the gateway: dlopen the artefact here and
# resolve every symbol bifrost will look up.
RUN /out/loader /out/${PLUGIN}.so

# Debian rather than scratch so the glibc under the .so matches what a
# Debian-based bifrost image provides. A plugin linked against a different libc
# than the process loading it is undefined behaviour, not a version warning.
FROM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171 AS artifact
ARG PLUGIN
COPY --from=build /out/${PLUGIN}.so /plugins/${PLUGIN}.so

# Publishes the .so into the volume the gateway mounts, then exits.
CMD ["sh", "-c", "cp /plugins/*.so /shared/ && ls -l /shared"]
