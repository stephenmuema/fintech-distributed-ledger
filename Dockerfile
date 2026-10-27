# Zirnis API (no provider code: providers are modules, built with modules/Dockerfile) — multi-stage build producing a static binary on a distroless, non-root image.
FROM golang:1.27-alpine AS build
WORKDIR /src

# Download modules first for better layer caching.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
COPY pkg/ pkg/
# Every database driver is pure Go (SQLite, PostgreSQL, MySQL, SQL Server, Oracle, MongoDB),
# so the binary is fully static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/zirnis ./cmd/zirnis \
 && mkdir -p /out/data

# distroless/static: no shell, no package manager, runs as UID 65532.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/zirnis /zirnis
# The image runs on PostgreSQL by default: set ZIRNIS_DATABASE_URL (docker compose points it at
# the bundled PostgreSQL). /data holds the settlement and statement inboxes, and SQLite's file if
# ZIRNIS_DATASOURCE=sqlite is chosen explicitly (sandbox only).
COPY --from=build --chown=65532:65532 /out/data /data
ENV ZIRNIS_ADDR=:8080 \
    ZIRNIS_DATASOURCE=postgres \
    ZIRNIS_DB=/data/zirnis.db
VOLUME /data
EXPOSE 8080 8081
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=3s --start-period=90s CMD ["/zirnis", "healthcheck"]
ENTRYPOINT ["/zirnis"]
