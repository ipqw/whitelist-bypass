# Под обхода: headless-creator'ы апстрима + GUI-Creator для входа в аккаунты + агент.
# Версия апстрима меняется здесь в двух местах: тег образа и AppImage с его sha256.

FROM ghcr.io/kulikov0/whitelist-bypass-bot:v0.4.4 AS upstream

FROM golang:1.26-bookworm AS agent
WORKDIR /src
COPY agent/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wlb-agent .

FROM debian:bookworm-slim AS creator
ARG CREATOR_URL=https://github.com/kulikov0/whitelist-bypass/releases/download/v0.4.4/WhitelistBypass.Creator-0.4.4-x86_64.AppImage
ARG CREATOR_SHA256=ed2d4bf28211711513ecea9f27b59d553888bd7472685a4b88c7c7ec9a3131ca
ADD --checksum=sha256:${CREATOR_SHA256} ${CREATOR_URL} /creator.AppImage
RUN cd / && chmod +x creator.AppImage && ./creator.AppImage --appimage-extract >/dev/null \
 && mv squashfs-root /opt-creator && chmod -R a+rX /opt-creator

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates tini procps \
      xvfb x11vnc novnc python3-websockify \
      libgtk-3-0 libnss3 libxss1 libasound2 libgbm1 libxshmfence1 libdrm2 \
      libatk-bridge2.0-0 libcups2 libsecret-1-0 libxkbcommon0 libxcomposite1 \
      libxdamage1 libxrandr2 libxtst6 libnotify4 libx11-xcb1 fonts-dejavu-core \
 && rm -rf /var/lib/apt/lists/*

RUN useradd --uid 1000 --home-dir /data --no-create-home --shell /usr/sbin/nologin wlb \
 && mkdir -p /data && chown wlb:wlb /data

COPY --from=upstream /opt/wlb/bin/ /opt/wlb/bin/
COPY --from=creator /opt-creator /opt/creator
COPY --from=agent /out/wlb-agent /usr/local/bin/wlb-agent
COPY pod/wlb-login /usr/local/bin/wlb-login

USER wlb
ENV HOME=/data
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/wlb-agent"]
