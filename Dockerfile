# kiln-cache, the REAPI cache server, as a small non-root image.
#
#   docker build -t kiln-cache .
#   docker run -p 8980:8980 -p 8981:8981 -v kiln-cache:/data kiln-cache
#
# The binary is cross-compiled on the build platform (no emulation needed) and
# built reproducibly: -trimpath and an empty build ID, so the same source gives
# the same bytes.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -buildid=" -o /out/kiln-cache ./cmd/kiln-cache
RUN mkdir /out/data

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/kiln-cache /usr/local/bin/kiln-cache
# Owned by the image's nonroot user, so a fresh named volume is writable.
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME /data
EXPOSE 8980 8981
ENTRYPOINT ["/usr/local/bin/kiln-cache", "-dir", "/data", "-listen", "0.0.0.0:8980", "-metrics", "0.0.0.0:8981"]
