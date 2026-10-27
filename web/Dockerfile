# The Zirnis console (8080, the internal network) and the payer edge (8081, the internet: hosted checkout
# and provider callbacks), served by unprivileged nginx, which proxies to the API container.
FROM node:24-alpine AS build
WORKDIR /web
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

# Runs as a non-root user and listens on 8080. Alpine packages are upgraded at build time so a
# base image that lags its distro (e.g. OpenSSL CVE-2026-31789) still ships the patched libraries.
# nginx itself comes from nginx.org's repository and only changes with the tag: track the stable line.
FROM nginxinc/nginx-unprivileged:1.30-alpine
USER root
RUN apk upgrade --no-cache
USER 101
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /web/dist /usr/share/nginx/html
EXPOSE 8080 8081
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/ >/dev/null || exit 1
