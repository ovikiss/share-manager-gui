# Share Manager GUI

Lightweight Go/Docker management UI for native Samba and NFS on a Debian host.
Samba and NFS remain host services; this container provides the management API and UI.

## Features

- Create, edit and delete Samba shares.
- Toggle Samba Recycle Bin per share.
- Browse host folders under the configured Browse root.
- Select Samba users for access, write and read-only lists.
- Add/change/delete Samba passwords without deleting Linux accounts.
- Create and delete NFS exports with separate, explained options.
- Validate changes with `testparm` and `exportfs`.
- Automatic backups before configuration and account changes.
- Restore files from each share's configured recycle directory (`#recycle` by default).
- Local unauthenticated access and external authentication through Nginx Proxy
  Manager + Tinyauth.

## Local development

```sh
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o share-manager main.go
docker build -t share-manager:dev .
```

GitHub Actions runs formatting, tests, a static build and a container build.
Dependabot keeps Go, Docker and Actions dependencies current.

## Deployment

The Compose file is intended to run from this directory on the Debian VM. It
requires host paths for `/etc/samba`, `/etc/exports`, `/etc/exports.d`,
`/var/lib/samba` and `/mnt`.

```sh
docker compose -f compose.yaml build
docker compose -f compose.yaml up -d
```

The UI listens on local port `9090` and the container listens internally on
`8080`. Direct local access is intentionally unauthenticated:

```text
http://DEBIAN_HOST:9090
```

Do not expose port 9090 to the Internet. For external access, create an NPM
proxy host on the `tinyauth_default` network, forwarding to `share-manager:8080`,
and apply the existing Tinyauth `auth_request` policy.

## Configuration

Deployment-specific values are declared in `compose.yaml`, not in Go:

| Variable | Purpose |
| --- | --- |
| `PORT` | Internal HTTP port |
| `SMB_CONF_PATH` | Samba configuration file |
| `NFS_EXPORTS_PATH` / `NFS_EXPORTS_DIR` | NFS export files |
| `SAMBA_DB_PATH` | Samba password database backup source |
| `BROWSE_ROOT` | Allowed root for folder browsing |
| `RECYCLE_DIR` | Per-share recycle directory name |
| `NFS_MANAGER_FILENAME` | File used for new NFS exports |
| `UI_FILE` / `STATIC_DIR` | Application page and generated shared UI assets |
| `UI_SETTINGS_PATH` | Persistent theme, style, text size and language preferences |
| `FILE_UID` / `FILE_GID` | Ownership for persistent backup files |
| `BACKUP_DIR` / `BACKUP_DISPLAY_DIR` | Internal and displayed backup paths |
| `MAX_RECYCLE_ENTRIES` | Maximum recycle entries shown |
| `MIN_USER_UID` | Fallback threshold for local users |
| `NSENTER_PID` | Host PID namespace target |
| `SMB_SERVICE` | Host Samba service name |

The backend is privileged and uses the host PID namespace so it can validate
and reload host services. It does not mount the Docker socket. Keep the service
behind the LAN boundary and Tinyauth externally.

## Backups and rollback

Backups are stored under the persistent share-manager data directory. Each
backup contains the Samba config, NFS exports and Samba password database; the
`exports.d` directory is included when present. Restore desired files to their
host paths, then validate with:

```sh
testparm -s
exportfs -ra
```

## Repository housekeeping

- `.github/workflows/ci.yml` verifies Go formatting, tests, build and Docker build.
- Version tags publish `amd64`, `arm64` and `arm/v7` images to GHCR.
- Every CI/release build synchronizes the header, CSS, icons and translations from [`mikrotik-ui-shared`](https://github.com/ovikiss/mikrotik-ui-shared); these generated files are not committed here.
- The shared header uses the Share Manager application icon from `/share-manager-icon.svg`.
- `.github/dependabot.yml` checks Go modules, Docker and GitHub Actions monthly.
- `.gitignore` excludes local binaries, environment files and runtime backups.
- No host credentials, Samba databases or generated runtime data belong in Git.
