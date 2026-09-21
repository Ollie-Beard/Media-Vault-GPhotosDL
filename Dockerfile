# Build Stage
FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o gphotosdl main.go


# Runtime Stage
FROM debian:bookworm-slim

# Chrome runtime dependencies
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        fonts-liberation \
        libasound2 \
        libatk-bridge2.0-0 \
        libatk1.0-0 \
        libc6 \
        libcairo2 \
        libcups2 \
        libdbus-1-3 \
        libdrm2 \
        libgbm1 \
        libglib2.0-0 \
        libgtk-3-0 \
        libnspr4 \
        libnss3 \
        libu2f-udev \
        libvulkan1 \
        libx11-6 \
        libx11-xcb1 \
        libxcb1 \
        libxcomposite1 \
        libxdamage1 \
        libxext6 \
        libxfixes3 \
        libxkbcommon0 \
        libxrandr2 \
    && rm -rf /var/lib/apt/lists/*

# Install Google Chrome
COPY google-chrome.deb /tmp/google-chrome.deb

RUN apt-get update \
    && apt-get install -y /tmp/google-chrome.deb \
    && rm -f /tmp/google-chrome.deb \
    && rm -rf /var/lib/apt/lists/*

# GPhotosDL runtime configuration
ENV GPHOTOSDL_BROWSER_PATH=/usr/bin/google-chrome \
    GPHOTOSDL_CONFIG_DIR=/app/config \
    GPHOTOSDL_DOWNLOAD_DIR=/app/downloads \
    GPHOTOSDL_CONTAINER=1 \
    PATH="/usr/bin:${PATH}"

WORKDIR /app

COPY --from=builder /app/gphotosdl .

RUN mkdir -p /app/config /app/downloads

EXPOSE 8282

ENTRYPOINT ["/app/gphotosdl"]