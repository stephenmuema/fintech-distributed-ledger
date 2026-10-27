# A provider module — one image per module, built from the repository root:
#
#   docker build -f modules/Dockerfile --build-arg MODULE=mpesa -t zirnis-mpesa-module .
#
# MODULE is a directory under modules/ (mpesa, kcb, equity, verto). Modules build against the
# core's public contract (pkg/) only; none of Zirnis's internals go into the image.
FROM golang:1.27-alpine AS build
ARG MODULE
WORKDIR /src
COPY go.mod go.sum ./
COPY pkg/ pkg/
COPY modules/ modules/
WORKDIR /src/modules/${MODULE}
RUN go mod download \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/module ./cmd/${MODULE}-module

# distroless/static: no shell, no package manager, runs as UID 65532.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/module /module
ENV MODULE_ADDR=:8090
EXPOSE 8090
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s CMD ["/module", "healthcheck"]
ENTRYPOINT ["/module"]
