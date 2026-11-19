# SeedLink Auditor

**SeedLink Auditor** is a lightweight, zero-dependency Go web application that runs on any server OS (Linux, Windows, macOS, BSD) or as a minimal Docker container to audit hardlinks, duplicate copies, and cross-seeded torrents across multiple folders and qBittorrent instances.

## Why It Exists

When using cross-seeding tools, matching releases are hardlinked into a dedicated cross-seed folder and seeded on additional trackers. If you later remove the original torrent or file from your primary folder, the cross-seed hardlink becomes the **sole copy on disk** (`nlink = 1`) and continues consuming full hard drive space. Conversely, misconfigured mounts or copy operations can leave **full duplicate copies** instead of hardlinks, silently wasting 2x storage.

SeedLink Auditor inspects physical inode / file index metadata (`device`, `inode`, `nlink`) across two or more folders (including all subfolders) and cross-references every file against one or more qBittorrent WebUI instances.

## Key Features

- **Zero External Dependencies & Single Binary**: Compiled purely with the Go standard library with embedded HTML/CSS/JS assets (`go:embed`).
- **Multi-Folder Comparison (Including Subfolders)**: Compare Folder A (Primary / Originals) against Folder B+ (Cross-Seed Links or Media Libraries).
- **Hardlink vs. Copy Detection**:
  - `HARDLINKED`: Identical `(device, inode)` across folders — 0 extra disk bytes used.
  - `DUPLICATE_COPY`: Same filename and byte size across folders on separate inodes — convertible to hardlinks with SHA-256 head/mid/tail content verification.
  - `ORPHANED_IN_B`: Present only in Folder B+ after the original in Folder A was removed, showing whether it is the sole copy on disk (`nlink == 1`) or still referenced externally.
  - `ONLY_IN_A` and `SAME_NAME_DIFF_SIZE`.
- **Multi-Instance qBittorrent Correlation**: Connects concurrently to one or more qBittorrent WebUI v2 instances with container path-mapping support (`remote_prefix -> local_prefix`), reporting instance, torrent name, tracker, ratio, seeding state, and unregistered/tracker errors.
- **Radarr & Sonarr v3 Integration**: Checks whether files are imported and referenced in Radarr/Sonarr, detects wanted releases stuck in download folders or blocked in the queue, flags unlinked files inside library folders, and lets you trigger `DownloadedMoviesScan`/`DownloadedEpisodesScan`, `ManualImport`, or `RefreshMovie`/`RescanSeries` directly after a dry-run preview.
- **Safe Read-Only Mode & Dry-Run Previews**: Starts in Read-Only Mode (`HTTP 403` protection against accidental modifications). Every action supports dry-run previews before execution.
- **CSV & JSON Exports**: Download full audit reports anytime via `/api/export?format=csv` or `format=json`.

---

## Deploying with Docker

### Important Note for Hardlinks in Docker

For hardlink detection and creation to work across folders inside a container, **both Folder A and Folder B must reside inside a single shared volume mount**:

- **Recommended**: Mount `/path/to/your/data:/data`, then compare `/data/torrents` against `/data/cross-seed`.
- **Avoid**: Mounting two separate volumes (`/downloads` and `/cross-seed`), which exposes different container device IDs and prevents hardlinking (`os.Link`). SeedLink Auditor automatically detects mismatched devices and displays a warning banner.

### Option 1: Docker Compose (Recommended)

1. Update `/path/to/your/data:/data` in `docker-compose.yml` to point to the parent directory containing your folders.
2. Start the container using the pre-built image from GitHub Container Registry:
   ```bash
   docker compose up -d
   ```
   *(Or uncomment `build: .` in `docker-compose.yml` and run `docker compose up -d --build` to build locally.)*
3. Open `http://<SERVER-IP>:8090` in your browser.

### Option 2: Docker CLI

```bash
docker run -d \
  --name seedlink-auditor \
  --restart unless-stopped \
  -p 8090:8090 \
  -v $(pwd)/config:/config \
  -v /path/to/your/data:/data \
  ghcr.io/kakmoster/seedlink-auditor:latest
```

### Configuring Path Mappings (qBittorrent, Radarr & Sonarr)

If your qBittorrent, Radarr, or Sonarr container reports paths like `/downloads/Movie/movie.mkv` or `/movies/...` while SeedLink Auditor accesses those directories under `/data/...`, add **Path Mappings** in Settings for each instance:
- **Remote prefix**: `/downloads` (or `/movies`)
- **Local prefix**: `/data/torrents` (or `/data/media/movies`)

---

## Running Standalone (Without Docker)

Build and run directly on Linux, Windows, macOS, or BSD:

```bash
go build -ldflags="-s -w" -o seedlink-auditor .
./seedlink-auditor
```

Environment variables:
- `PORT`: HTTP listen port (default: `8090`)
- `SEEDLINK_CONFIG_PATH`: Settings file path (default: `./data/settings.json`, or `/config/settings.json` in Docker)

Run the automated test suite:

```bash
go test -v ./...
```
