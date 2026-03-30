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

## Save & ZIP API (new)

Client gửi từng file URL → Server tải background → Tạo ZIP → Trả link static.

### Flow

```
POST /save/init              → {"task_id": "xxx"}
POST /save/add  {task_id, url}  → server tải background (gọi nhiều lần)
POST /save/zip  {task_id, zip_name} → yêu cầu tạo ZIP
GET  /save/status/{task_id}  → poll tiến độ
GET  /files/{task_id}/xxx.zip → download ZIP
```

### Example

```bash
# 1. Init
TASK_ID=$(curl -s -X POST http://localhost:6001/save/init | jq -r '.task_id')

# 2. Add files (gọi song song hoặc lần lượt)
curl -s -X POST http://localhost:6001/save/add \
  -H 'Content-Type: application/json' \
  -d '{"task_id":"'$TASK_ID'","url":"https://example.com/video1.mp4"}'

curl -s -X POST http://localhost:6001/save/add \
  -H 'Content-Type: application/json' \
  -d '{"task_id":"'$TASK_ID'","url":"https://example.com/video2.mp4"}'

# 3. Request ZIP
curl -s -X POST http://localhost:6001/save/zip \
  -H 'Content-Type: application/json' \
  -d '{"task_id":"'$TASK_ID'","zip_name":"my_videos.zip"}'

# 4. Poll status
curl -s http://localhost:6001/save/status/$TASK_ID
# → {"status":"done","zip_url":"https://host/files/.../my_videos.zip","zip_size":123456}
```

### Status values

| Status | Description |
|--------|-------------|
| `pending` | Đang nhận file, chưa yêu cầu ZIP |
| `zipping` | Đang chờ download xong + tạo ZIP |
| `done` | ZIP sẵn sàng, có `zip_url` |
| `failed` | Lỗi |

## Config

| Parameter | Default | Description |
|-----------|---------|-------------|
| SessionTTL | 1 hour | Session expiration |
| TaskTTL | 1 hour | Save task expiration (auto cleanup files) |
| HTTPTimeout | 60 min | Timeout per HTTP request |
| DownloadTimeout | 60 min | Total download timeout |
| MaxWorkers | 10 | Concurrent download goroutines |

## Run

```bash
go build -o server && ./server
# Server running on :6001
```
