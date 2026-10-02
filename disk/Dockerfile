# The disk guard: one static binary in an empty image. Everything it runs on a
# node — crictl, journalctl, growpart, resize2fs — is the node's own, reached
# through the node's mount namespace, so the image carries no tools.
FROM --platform=$BUILDPLATFORM ghcr.io/hanzoai/mirror/golang:1.27.1-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /disk .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /disk /disk
ENTRYPOINT ["/disk"]
