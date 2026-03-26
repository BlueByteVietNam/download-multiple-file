package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ============== CONFIG ==============
const (
	SessionTTL      = 1 * time.Hour    // Session hết hạn sau 1 giờ
	CleanupInterval = 5 * time.Minute  // Cleanup mỗi 5 phút
	HTTPTimeout     = 60 * time.Minute  // Timeout cho mỗi HTTP request
	DownloadTimeout = 60 * time.Minute // Timeout cho toàn bộ download
	MaxWorkers      = 10               // Số goroutine download song song
	MaxPrefetch     = 15               // Tối đa temp files trên disk cùng lúc
)

// ============== TYPES ==============

type DownloadRequest struct {
	Files   []string `json:"files"`
	ZipName string   `json:"zipName"`
}

type DownloadResponse struct {
	DownloadURL string `json:"download_url"`
}

type Session struct {
	Files     []string
	ZipName   string
	CreatedAt time.Time
}

type FileTask struct {
	Index int
	URL   string
}

type FileResult struct {
	Index    int
	FileName string
	TempPath string
	Err      error
}

// ============== GLOBAL STATE ==============

var (
	sessions = make(map[string]*Session)
	mu       sync.RWMutex

	// HTTP client với timeout
	httpClient = &http.Client{
		Timeout: HTTPTimeout,
	}
)

// ============== CORS MIDDLEWARE ==============

func enableCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
		w.Header().Set("Access-Control-Max-Age", "86400")

		// Handle preflight OPTIONS request
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next(w, r)
	}
}

// ============== MAIN ==============

func main() {
	// Khởi động cleanup goroutine
	go cleanupExpiredSessions()

	http.HandleFunc("/create", enableCORS(handleCreate))
	http.HandleFunc("/download/", enableCORS(handleDownload))

	port := ":6001"
	log.Printf("Server running on %s (Session TTL: %v, HTTP Timeout: %v)", port, SessionTTL, HTTPTimeout)
	log.Fatal(http.ListenAndServe(port, nil))
}

// ============== CLEANUP GOROUTINE ==============

func cleanupExpiredSessions() {
	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		expired := []string{}

		mu.RLock()
		for token, session := range sessions {
			if now.Sub(session.CreatedAt) > SessionTTL {
				expired = append(expired, token)
			}
		}
		mu.RUnlock()

		if len(expired) > 0 {
			mu.Lock()
			for _, token := range expired {
				delete(sessions, token)
			}
			mu.Unlock()
			log.Printf("Cleaned up %d expired sessions", len(expired))
		}
	}
}

// ============== HANDLERS ==============

func handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req DownloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if len(req.Files) == 0 {
		http.Error(w, "No files provided", http.StatusBadRequest)
		return
	}

	zipName := req.ZipName
	if zipName == "" {
		zipName = "files.zip"
	}

	token := uuid.New().String()

	mu.Lock()
	sessions[token] = &Session{
		Files:     req.Files,
		ZipName:   zipName,
		CreatedAt: time.Now(),
	}
	mu.Unlock()

	resp := DownloadResponse{
		DownloadURL: fmt.Sprintf("https://%s/download/%s", r.Host, token),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)

	log.Printf("Created session %s with %d files (expires: %v)", token, len(req.Files), time.Now().Add(SessionTTL).Format("15:04:05"))
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	token := path.Base(r.URL.Path)

	mu.RLock()
	session, exists := sessions[token]
	mu.RUnlock()

	if !exists {
		http.Error(w, "Invalid or expired token", http.StatusNotFound)
		return
	}

	// Check nếu session đã expired
	if time.Since(session.CreatedAt) > SessionTTL {
		http.Error(w, "Session expired", http.StatusGone)
		mu.Lock()
		delete(sessions, token)
		mu.Unlock()
		return
	}

	// Tạo temp directory cho session
	tempDir, err := os.MkdirTemp("", "zip-download-*")
	if err != nil {
		log.Printf("Failed to create temp dir: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tempDir)

	// Set headers
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", session.ZipName))

	zipWriter := zip.NewWriter(w)
	defer zipWriter.Close()

	// Context với timeout cho toàn bộ download
	ctx, cancel := context.WithTimeout(r.Context(), DownloadTimeout)
	defer cancel()

	fileCount := len(session.Files)
	results := make([]*FileResult, fileCount)
	done := make([]chan struct{}, fileCount)
	for i := range done {
		done[i] = make(chan struct{})
	}

	// Semaphore để giới hạn số temp files trên disk
	sem := make(chan struct{}, MaxPrefetch)

	// Work channel cho dispatcher → workers
	tasks := make(chan FileTask)

	// Dispatcher goroutine: gửi tasks vào work channel, acquire semaphore trước
	go func() {
		defer close(tasks)
		for i, fileURL := range session.Files {
			select {
			case sem <- struct{}{}: // Acquire semaphore
			case <-ctx.Done():
				// Signal done cho các tasks còn lại
				for j := i; j < fileCount; j++ {
					results[j] = &FileResult{Index: j, Err: ctx.Err()}
					close(done[j])
				}
				return
			}

			select {
			case tasks <- FileTask{Index: i, URL: fileURL}:
			case <-ctx.Done():
				<-sem // Release semaphore vừa acquire
				for j := i; j < fileCount; j++ {
					results[j] = &FileResult{Index: j, Err: ctx.Err()}
					close(done[j])
				}
				return
			}
		}
	}()

	// Worker pool
	var wg sync.WaitGroup
	for w := 0; w < MaxWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range tasks {
				result := downloadToTemp(ctx, task, tempDir)
				results[task.Index] = result
				close(done[task.Index])
			}
		}()
	}

	// Đảm bảo workers kết thúc trong background
	go func() {
		wg.Wait()
	}()

	// ZIP writer loop: chờ file theo thứ tự, ghi vào ZIP
	usedNames := make(map[string]int)
	for i := 0; i < fileCount; i++ {
		// Chờ file i hoàn thành
		select {
		case <-done[i]:
		case <-ctx.Done():
			log.Printf("Download timeout for token: %s", token)
			return
		}

		result := results[i]
		if result.Err != nil {
			log.Printf("Error downloading file %d: %v", i, result.Err)
			// Release semaphore nếu không có temp file (error trước khi tạo file)
			if result.TempPath == "" {
				select {
				case <-sem:
				default:
				}
			}
			continue
		}

		// Xử lý trùng tên
		fileName := result.FileName
		originalName := fileName
		if count, exists := usedNames[originalName]; exists {
			ext := path.Ext(fileName)
			base := fileName[:len(fileName)-len(ext)]
			fileName = fmt.Sprintf("%s_%d%s", base, count+1, ext)
		}
		usedNames[originalName]++

		log.Printf("Writing to ZIP: %s (file %d/%d)", fileName, i+1, fileCount)

		if err := streamTempFileToZip(zipWriter, result.TempPath, fileName); err != nil {
			log.Printf("Error writing to ZIP: %v", err)
		}

		// Xoá temp file và release semaphore
		os.Remove(result.TempPath)
		<-sem
	}

	// Xóa session sau khi download xong
	mu.Lock()
	delete(sessions, token)
	mu.Unlock()

	log.Printf("Download completed for token: %s (%d files)", token, fileCount)
}

// ============== CONCURRENT DOWNLOAD HELPERS ==============

func downloadToTemp(ctx context.Context, task FileTask, tempDir string) *FileResult {
	result := &FileResult{Index: task.Index}

	fileName, resp, err := getOriginalFileName(ctx, task.URL)
	if err != nil {
		result.Err = err
		return result
	}
	defer resp.Body.Close()

	result.FileName = fileName

	// Tạo temp file
	tmpFile, err := os.CreateTemp(tempDir, fmt.Sprintf("file-%d-*", task.Index))
	if err != nil {
		result.Err = fmt.Errorf("create temp file: %w", err)
		return result
	}

	result.TempPath = tmpFile.Name()

	// Stream response body vào temp file
	_, err = io.Copy(tmpFile, resp.Body)
	tmpFile.Close()
	if err != nil {
		os.Remove(result.TempPath)
		result.TempPath = ""
		result.Err = fmt.Errorf("download to temp: %w", err)
		return result
	}

	log.Printf("Downloaded: %s -> %s (file %d)", task.URL, fileName, task.Index)
	return result
}

func streamTempFileToZip(zw *zip.Writer, tempPath string, fileName string) error {
	fi, err := os.Stat(tempPath)
	if err != nil {
		return err
	}

	header := &zip.FileHeader{
		Name:               fileName,
		Method:             zip.Store,
		UncompressedSize64: uint64(fi.Size()),
	}
	header.SetModTime(time.Now())

	fileWriter, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}

	f, err := os.Open(tempPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(fileWriter, f)
	return err
}

// ============== HELPERS ==============

func extractFilenameFromHeader(header string) string {
	// Thử pattern: filename*=UTF-8''encoded_filename (RFC 5987)
	if idx := indexOf(header, "filename*=UTF-8''"); idx >= 0 {
		start := idx + len("filename*=UTF-8''")
		end := indexOfAny(header[start:], []string{";", "\n", "\r"})
		if end < 0 {
			end = len(header) - start
		}
		encoded := header[start : start+end]
		if decoded, err := url.QueryUnescape(encoded); err == nil {
			return decoded
		}
	}

	// Thử pattern: filename="..." (có thể có dấu " bên trong)
	if idx := indexOf(header, `filename="`); idx >= 0 {
		start := idx + len(`filename="`)
		// Tìm dấu " cuối cùng trước ; hoặc end of string
		remaining := header[start:]
		var end int

		// Tìm dấu " cuối (backwards)
		lastQuote := lastIndexOf(remaining, `"`)
		if lastQuote > 0 {
			end = lastQuote
		} else {
			// Không tìm thấy dấu " đóng, lấy đến ; hoặc end
			end = indexOfAny(remaining, []string{";", "\n", "\r"})
			if end < 0 {
				end = len(remaining)
			}
		}

		if end > 0 {
			return remaining[:end]
		}
	}

	return ""
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func lastIndexOf(s, substr string) int {
	for i := len(s) - len(substr); i >= 0; i-- {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func indexOfAny(s string, substrs []string) int {
	minIdx := -1
	for _, substr := range substrs {
		idx := indexOf(s, substr)
		if idx >= 0 && (minIdx < 0 || idx < minIdx) {
			minIdx = idx
		}
	}
	return minIdx
}

func getOriginalFileName(ctx context.Context, fileURL string) (string, *http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", fileURL, nil)
	if err != nil {
		return "", nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", nil, err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return "", nil, fmt.Errorf("bad status %d", resp.StatusCode)
	}

	// Thử lấy từ Content-Disposition header
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		_, params, err := mime.ParseMediaType(cd)
		if err == nil {
			if filename, ok := params["filename"]; ok && filename != "" {
				return filename, resp, nil
			}
		}

		// Fallback: parse manually nếu mime.ParseMediaType fail
		// Extract filename từ pattern: filename="..." hoặc filename*=UTF-8''...
		if filename := extractFilenameFromHeader(cd); filename != "" {
			return filename, resp, nil
		}
	}

	// Fallback: lấy từ URL path
	parsed, err := url.Parse(fileURL)
	if err == nil {
		fileName := path.Base(parsed.Path)
		if fileName != "" && fileName != "/" && fileName != "." {
			return fileName, resp, nil
		}
	}

	return "file", resp, nil
}

