# Save & ZIP API

Client gửi từng file → Server tải background → Tạo ZIP → Trả link download.

## Flow

```
POST /save/init                     → {"task_id": "xxx"}
POST /save/add   {task_id, url}     → server tải background (gọi nhiều lần)
POST /save/zip   {task_id, zip_name}→ yêu cầu tạo ZIP
GET  /save/status/{task_id}         → poll tiến độ
GET  /files/{task_id}/xxx.zip       → download ZIP
```

## Example

```bash
# 1. Init
TASK_ID=$(curl -s -X POST https://muti-download.ytconvert.org/save/init | jq -r '.task_id')

# 2. Add files
curl -s -X POST https://muti-download.ytconvert.org/save/add \
  -d '{"task_id":"'$TASK_ID'","url":"https://example.com/video1.mp4"}'

curl -s -X POST https://muti-download.ytconvert.org/save/add \
  -d '{"task_id":"'$TASK_ID'","url":"https://example.com/video2.mp4"}'

# 3. Request ZIP
curl -s -X POST https://muti-download.ytconvert.org/save/zip \
  -d '{"task_id":"'$TASK_ID'","zip_name":"my_videos.zip"}'

# 4. Poll
curl -s https://muti-download.ytconvert.org/save/status/$TASK_ID
# → {"status":"done", "zip_url":"...", "zip_size":123456}
```

## Status

| Status | Meaning |
|--------|---------|
| `pending` | Đang nhận file |
| `zipping` | Chờ download xong + tạo ZIP |
| `done` | ZIP sẵn sàng (`zip_url`) |
| `failed` | Lỗi |

## Notes

- File + ZIP tự xoá sau 1 giờ (`TaskTTL`)
- Server tải concurrent (max 10 goroutines)
- ZIP dùng `Store` (không nén) — nhanh, phù hợp video/audio
