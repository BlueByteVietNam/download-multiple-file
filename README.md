# download-multiple-file

Stream multiple files into a single ZIP download on-the-fly. No temp files, no disk storage.

## Features

- ✅ Stream multiple URLs into single ZIP
- ✅ No temp files - direct pipe from source to client  
- ✅ Original filenames preserved (from URL or Content-Disposition)
- ✅ Session TTL with auto cleanup
- ✅ Configurable timeouts

## Usage

### 1. Create download session

```bash
curl -X POST 'http://localhost:8080/create' \
  -H 'Content-Type: application/json' \
  -d '{
    "files": [
      "https://example.com/video1.mp4",
      "https://example.com/video2.mp4"
    ],
    "zipName": "my_videos.zip"
  }'
```

Response:
```json
{"download_url": "http://localhost:8080/download/{token}"}
```

### 2. Download ZIP

Open the `download_url` in browser or:
```bash
curl -o my_videos.zip "http://localhost:8080/download/{token}"
```

## Config

| Parameter | Default | Description |
|-----------|---------|-------------|
| SessionTTL | 1 hour | Session expiration time |
| HTTPTimeout | 5 min | Timeout per HTTP request |
| DownloadTimeout | 30 min | Total download timeout |

## Run

```bash
go build -o server
./server
# Server running on :8080
```

## How it works

```
Client POST URLs → Server creates session with token
                         ↓
Client GET /download/{token} → Server streams each URL
                         ↓
                    ZIP on-the-fly (io.Copy)
                         ↓
                  Client receives ZIP
```

**Memory usage**: ~32KB (io.Copy buffer), regardless of file sizes.
