FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY bot/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wlb-bot .

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/wlb-bot /usr/local/bin/wlb-bot
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/wlb-bot"]
