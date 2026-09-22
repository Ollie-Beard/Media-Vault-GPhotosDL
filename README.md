# Media Vault GPhotosDL

GPhotosDL is the Google Photos download service used by the Media Vault ecosystem.

It provides a small HTTP API that allows Media Vault and its Jellyfin plugin to request media from Google Photos and download the original media directly to local storage. The service uses Google Chrome with a persistent browser profile so Google Photos authentication survives application and container restarts.

This repository contains the customised GPhotosDL source used by Media Vault.

---

## What it does

The Media Vault setup uses GPhotosDL as the bridge between Google Photos and Jellyfin:

```text
Google Photos
      │
      ▼
   GPhotosDL
      │
      ▼
 Local media file
      │
      ▼
   Jellyfin
```

For a normal Media Vault playback request:

```text
Jellyfin
   │
   ▼
Media Vault Jellyfin plugin
   │
   ▼
GPhotosDL HTTP API
   │
   ▼
Google Photos
   │
   ▼
Download to local storage
   │
   ▼
Jellyfin library scan
   │
   ▼
Normal Jellyfin playback
```

GPhotosDL does not provide fake Jellyfin media sources. It downloads the actual media file locally and allows Jellyfin to discover and play that file normally.

---

## Features

- Google Photos authentication through Google Chrome
- Persistent Chrome profile
- Direct media downloads to a configured local directory
- HTTP API for Media Vault/Jellyfin integration
- Asynchronous downloads
- Download progress tracking
- Download speed reporting
- ETA reporting
- Download cancellation
- Google Photos shared-link support
- Filename sanitisation
- Configurable browser, configuration and download directories
- Windows executable build
- Linux/Docker deployment
- Chrome download-history cleanup
- Container-specific Chrome launch configuration
- Persistent authentication across GPhotosDL restarts

---

## Why Google Chrome is used

The Docker version deliberately uses **Google Chrome on Debian**, rather than the Chromium package from Alpine Linux.

During development, Alpine Chromium did not reliably preserve the Google Photos authenticated session when Chromium was restarted in a fresh process/container.

The final Docker image therefore uses the official Google Chrome `.deb` package.

The Chrome package is a build dependency for the Docker image and is **not committed to this repository** because of its large size.

The Docker image expects:

```text
/usr/bin/google-chrome
```

The browser profile is stored separately from the application binary so that authentication can persist across container restarts.

---

# Windows

## Requirements

- Windows 10/11
- Google Chrome installed
- Go 1.22+ if building from source

The application looks for Chrome at:

```text
C:\Program Files\Google\Chrome\Application\chrome.exe
```

## Running

From the directory containing `gphotosdl.exe`:

The first run can be used to log in to a Google account.

```powershell
.\gphotosdl.exe -show -debug -login
```

Then run without -login for every subsequent use of the application:

```powershell
.\gphotosdl.exe -show -debug
```

Once authenticated, the Chrome profile is reused by subsequent GPhotosDL launches.

---

# Docker

## Docker image

The repository contains a multi-stage Dockerfile.

The build stage compiles the Go application.

The runtime stage is based on Debian Bookworm Slim and installs the Chrome runtime dependencies and Google Chrome.

The image exposes:

```text
8282
```

The application should bind to:

```text
0.0.0.0:8282
```

(visit localhost:8282)

when it needs to be accessed by Jellyfin or another device/container.

---

## Building the Docker image

The Dockerfile expects `google-chrome.deb` to be present in the build context.

Build the image with:

```powershell
docker build -t gphotosdl:latest .
```

The Chrome `.deb` is intentionally ignored by Git because of its size.

---

## Exporting the image

For deployment to a Synology NAS or another Docker host without rebuilding the image there:

```powershell
docker save -o gphotosdl.tar gphotosdl:latest
```

Copy the resulting `gphotosdl.tar` to the target Docker host.

---

# Synology NAS deployment

The current Media Vault deployment uses:

```text
/volume1/docker/gphotosdl/
```

with:

```text
/volume1/docker/gphotosdl/config
/volume1/docker/gphotosdl/downloads
```

The media destination is:

```text
/volume1/video
```

## Load the Docker image

After copying the exported image to the NAS:

```bash
sudo docker load -i /volume1/docker/gphotosdl/gphotosdl.tar
```

Check that Google Chrome is installed inside the image:

```bash
sudo docker run --rm --entrypoint google-chrome gphotosdl:latest --version
```

The current development image was built with Google Chrome:

```text
153.0.8010.47
```

---

## Run the container

The current Media Vault container can be started with:

```bash
sudo docker run -d \
  --name gphotosdl \
  -p 8282:8282 \
  -v /volume1/docker/gphotosdl/config:/app/config \
  -v /volume1/docker/gphotosdl/downloads:/app/downloads \
  -v /volume1/video:/media \
  -e GPHOTOSDL_BROWSER_PATH=/usr/bin/google-chrome \
  -e GPHOTOSDL_CONFIG_DIR=/app/config \
  -e GPHOTOSDL_DOWNLOAD_DIR=/app/downloads \
  -e GPHOTOSDL_CONTAINER=1 \
  --shm-size=512m \
  gphotosdl:latest \
  -addr 0.0.0.0:8282
```

The Dockerfile already contains the application's `ENTRYPOINT`, so do not append `/app/gphotosdl` after the image name.

---

# Docker environment variables

The current container configuration uses:

| Variable | Value | Purpose |
|---|---|---|
| `GPHOTOSDL_BROWSER_PATH` | `/usr/bin/google-chrome` | Chrome executable |
| `GPHOTOSDL_CONFIG_DIR` | `/app/config` | Persistent application/browser configuration |
| `GPHOTOSDL_DOWNLOAD_DIR` | `/app/downloads` | Default download directory |
| `GPHOTOSDL_CONTAINER` | `1` | Enables container-specific Chrome flags |

The application can also use its command-line options for configuration such as the HTTP listen address.

---

# Persistent Google Photos authentication

The browser profile is stored inside the configured GPhotosDL configuration directory.

For the current Synology deployment:

```text
/volume1/docker/gphotosdl/config/browser
```

This profile contains the authenticated Google Chrome session.

It must be treated as private data.

Do not:

- commit it to Git
- upload it to GitHub
- include it in Docker build contexts unnecessarily
- publish it as a release asset
- share the profile with other people

The profile is deliberately kept outside the Git repository.

---

# HTTP API

GPhotosDL exposes an HTTP API used by the Media Vault Jellyfin plugin.

The main download operation is:

```text
POST /download
```

A download request is handled asynchronously and receives a download ID that can be used to monitor or cancel the operation.

The API is used internally by Media Vault rather than being intended as a public internet-facing service.

A typical Media Vault flow is:

```text
POST /download
      │
      ▼
download ID
      │
      ├── monitor progress
      │
      ├── read speed/ETA
      │
      └── cancel if required
```

The service should normally be kept on a trusted LAN or private network.

---

# Media Vault integration

GPhotosDL is designed to work with the custom Media Vault Jellyfin plugin.

The Jellyfin plugin knows the Media Vault catalogue entry and requests a download from GPhotosDL.

GPhotosDL then:

1. Receives the Google Photos URL.
2. Creates an asynchronous download job.
3. Uses the authenticated Chrome/Google Photos session.
4. Downloads the media directly to local storage.
5. Reports progress to the caller.
6. Allows the caller to cancel the download.
7. Leaves the resulting media file available for Jellyfin to scan.

The plugin can then allow Jellyfin to discover the real local file.

This means Jellyfin ultimately handles the media as a normal local Jellyfin item.

---

# Container-specific Chrome configuration

When:

```text
GPHOTOSDL_CONTAINER=1
```

is set on Linux/Docker, GPhotosDL launches Chrome with container-specific options including:

```text
--no-sandbox
--disable-setuid-sandbox
--disable-dev-shm-usage
--password-store=basic
--disable-cookie-encryption
```

These options are intended for the Docker runtime and should not be copied into the normal Windows configuration unnecessarily.

---

# Chrome profile lock cleanup

GPhotosDL performs best-effort cleanup of stale Chromium/Chrome profile lock files when it is safe to do so.

The profile can contain files such as:

```text
SingletonLock
SingletonCookie
SingletonSocket
Default/LOCK
```

A stale lock can prevent Chrome from starting correctly after an unclean container or application shutdown.

GPhotosDL checks for an active Chromium/Chrome process before removing stale locks.

---

# Chrome download history

GPhotosDL downloads media directly to its configured destination rather than relying on Chrome's normal download workflow.

It also performs best-effort cleanup of Chrome's download history.

This is intended to prevent the Chrome `chrome://downloads/` page from filling up with Media Vault downloads.

---

# Authentication URL handling

Google Photos can redirect through several authentication states.

The customised authentication check accepts the authenticated Google Photos root URL:

```text
https://photos.google.com/
```

rather than relying on a single exact URL string.

This makes authentication detection more tolerant of normal Google Photos URL variations.

---

# Configuration and storage

A typical deployment separates application state from media storage:

```text
GPhotosDL
│
├── application
│
├── /app/config
│     └── browser profile and persistent configuration
│
├── /app/downloads
│     └── temporary/default downloads
│
└── /media
      └── Media Vault/Jellyfin media
```

For the current NAS deployment:

```text
/volume1/docker/gphotosdl/config
        ↓
/app/config

/volume1/docker/gphotosdl/downloads
        ↓
/app/downloads

/volume1/video
        ↓
/media
```

---

# Building from source

## Go executable

Install Go 1.22 or newer.

Then:

```powershell
go mod download
go build -o gphotosdl.exe .
```

Run:

```powershell
.\gphotosdl.exe -show -debug
```

---

## Linux build

A Linux binary can be built with:

```bash
CGO_ENABLED=0 GOOS=linux go build -o gphotosdl .
```

The Dockerfile performs this build automatically.

---

# Repository structure

The important source files are:

```text
.
├── .github/
│   └── workflows/
│       ├── build.yml
│       └── goreleaser.yaml
├── .gitattributes
├── .gitignore
├── .golangci.yml
├── .goreleaser.yaml
├── Dockerfile
├── go.mod
├── go.sum
├── main.go
├── signals_other.go
└── signals_unix.go
```

Generated/local files such as the Windows executable, Docker image export and Chrome `.deb` are intentionally excluded from the source repository.

---

---

# Releases

Built binaries and Docker image exports can be found in the releases section of this repository.

---

# Troubleshooting

## Google Photos says the browser is not logged in

Run GPhotosDL with the visible browser:

```powershell
.\gphotosdl.exe -show -debug
```

Authenticate with the Google account used for the Media Vault library.

For Docker, make sure the persistent `/app/config` volume is being used. Removing the configuration volume removes the persistent Chrome profile and therefore the saved Google Photos session.

---

## Chrome will not start in Docker

Check the installed Chrome version:

```bash
sudo docker run --rm --entrypoint google-chrome gphotosdl:latest --version
```

Also check the container logs:

```bash
sudo docker logs gphotosdl
```

The container should have:

```text
GPHOTOSDL_BROWSER_PATH=/usr/bin/google-chrome
```

and:

```text
GPHOTOSDL_CONTAINER=1
```

---

## Jellyfin cannot reach GPhotosDL

Make sure the container is listening on all interfaces:

```text
0.0.0.0:8282
```

and that the port is published:

```text
-p 8282:8282
```

The Media Vault Jellyfin plugin should use the NAS address, for example:

```text
http://192.168.1.8:8282
```

Do not use:

```text
http://localhost:8282
```

from a Jellyfin container unless GPhotosDL is actually running in the same network namespace/container environment where `localhost` refers to the GPhotosDL service.

---

## Docker container keeps losing authentication

Make sure the browser profile is stored on the persistent configuration volume:

```text
/volume1/docker/gphotosdl/config
```

Do not recreate the container with an empty `/app/config` volume unless you intentionally want a fresh Google Photos login.

---

# Security and privacy

GPhotosDL has access to the Google Photos account/session used for the Media Vault library.

Treat the following as sensitive:

- Google account authentication
- Chrome browser profile
- cookies
- application configuration
- downloaded media
- Google Photos URLs

The GPhotosDL HTTP API should not be exposed directly to the public internet without appropriate authentication and network controls.

The intended deployment is a private/local service used by the Media Vault Jellyfin installation.

---

# Relationship with the Media Vault projects

Media Vault is split into several components:

```text
Media Vault
│
├── Media Vault Jellyfin plugin
│
├── Media Vault GPhotosDL
│
├── Media Vault Apps2Samsung
│
└── Media Vault JavaScript Injector customisations
```

GPhotosDL is the download/backend component.

---

# Upstream

This project is based on the GPhotosDL project originally published by rclone.

The Media Vault version contains additional changes required for the Media Vault architecture, including:

- persistent Google Chrome authentication
- Windows Chrome profile handling
- stale Chrome profile lock cleanup
- tolerant Google Photos authentication detection
- Docker/Google Chrome support
- container-specific Chrome launch configuration
- direct Media Vault download integration
- download-history cleanup
- Media Vault-specific deployment configuration

The repository should be treated as the Media Vault custom version rather than a drop-in replacement for the upstream project.
