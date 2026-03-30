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
	HTTPTimeout     = 60 * time.Minute // Timeout cho mỗi HTTP request
	DownloadTimeout = 60 * time.Minute // Timeout cho toàn bộ download
	MaxWorkers      = 10               // Số goroutine download song song
	MaxPrefetch     = 15               // Tối đa temp files trên disk cùng lúc
	SavedFilesDir   = "./saved_files"  // Thư mục lưu file tải về
	TaskTTL         = 1 * time.Hour    // Task hết hạn sau 1 giờ
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

// ============== SAVE TASK TYPES ==============

type SaveTaskStatus string

const (
	StatusProcessing SaveTaskStatus = "processing"
	StatusDone       SaveTaskStatus = "done"
	StatusFailed     SaveTaskStatus = "failed"
)

type SavedFile struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

type SaveTask struct {
	ID          string         `json:"id"`
	Status      SaveTaskStatus `json:"status"`
	Total       int            `json:"total"`
	Completed   int            `json:"completed"`
	Failed      int            `json:"failed"`
	Files       []SavedFile    `json:"files,omitempty"`
	Error       string         `json:"error,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	mu          sync.Mutex
}

// ============== GLOBAL STATE ==============

var (
	sessions = make(map[string]*Session)
	mu       sync.RWMutex

	saveTasks  = make(map[string]*SaveTask)
	tasksMu    sync.RWMutex

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
	// Tạo thư mục lưu file
	os.MkdirAll(SavedFilesDir, 0755)

	// Khởi động cleanup goroutines
	go cleanupExpiredSessions()
	go cleanupExpiredTasks()

	http.HandleFunc("/create", enableCORS(handleCreate))
	http.HandleFunc("/download/", enableCORS(handleDownload))

	// Async save endpoints
	http.HandleFunc("/save", enableCORS(handleSave))
	http.HandleFunc("/status/", enableCORS(handleStatus))

	// Static file server cho saved files
	fileServer := http.StripPrefix("/files/", http.FileServer(http.Dir(SavedFilesDir)))
	http.HandleFunc("/files/", enableCORS(func(w http.ResponseWriter, r *http.Request) {
		fileServer.ServeHTTP(w, r)
	}))

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

// ============== TASK CLEANUP ==============

func cleanupExpiredTasks() {
	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		expired := []string{}

		tasksMu.RLock()
		for id, task := range saveTasks {
			if now.Sub(task.CreatedAt) > TaskTTL {
				expired = append(expired, id)
			}
		}
		tasksMu.RUnlock()

		if len(expired) > 0 {
			tasksMu.Lock()
			for _, id := range expired {
				delete(saveTasks, id)
				// Xoá thư mục file của task
				os.RemoveAll(fmt.Sprintf("%s/%s", SavedFilesDir, id))
			}
			tasksMu.Unlock()
			log.Printf("Cleaned up %d expired save tasks", len(expired))
		}
	}
}

// ============== SAVE HANDLERS ==============

func handleSave(w http.ResponseWriter, r *http.Request) {
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

	taskID := uuid.New().String()
	task := &SaveTask{
		ID:        taskID,
		Status:    StatusProcessing,
		Total:     len(req.Files),
		CreatedAt: time.Now(),
	}

	// Tạo thư mục cho task
	taskDir := fmt.Sprintf("%s/%s", SavedFilesDir, taskID)
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		http.Error(w, "Failed to create directory", http.StatusInternalServerError)
		return
	}

	tasksMu.Lock()
	saveTasks[taskID] = task
	tasksMu.Unlock()

	// Bắt đầu download trong background
	go processDownloadTask(task, req.Files, taskDir, r.Host)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"task_id": taskID,
		"status":  StatusProcessing,
		"total":   len(req.Files),
	})

	log.Printf("Created save task %s with %d files", taskID, len(req.Files))
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	taskID := path.Base(r.URL.Path)

	tasksMu.RLock()
	task, exists := saveTasks[taskID]
	tasksMu.RUnlock()

	if !exists {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	task.mu.Lock()
	resp := map[string]interface{}{
		"task_id":    task.ID,
		"status":     task.Status,
		"total":      task.Total,
		"completed":  task.Completed,
		"failed":     task.Failed,
		"created_at": task.CreatedAt,
	}
	if task.Status == StatusDone {
		resp["files"] = task.Files
		resp["completed_at"] = task.CompletedAt
	}
	if task.Error != "" {
		resp["error"] = task.Error
	}
	task.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func processDownloadTask(task *SaveTask, fileURLs []string, taskDir string, host string) {
	ctx, cancel := context.WithTimeout(context.Background(), DownloadTimeout)
	defer cancel()

	usedNames := make(map[string]int)
	var namesMu sync.Mutex

	// Worker pool
	type saveFileTask struct {
		index int
		url   string
	}

	tasks := make(chan saveFileTask, len(fileURLs))
	for i, u := range fileURLs {
		tasks <- saveFileTask{index: i, url: u}
	}
	close(tasks)

	var wg sync.WaitGroup
	workers := MaxWorkers
	if len(fileURLs) < workers {
		workers = len(fileURLs)
	}

	type savedResult struct {
		index int
		file  *SavedFile
	}
	resultsCh := make(chan savedResult, len(fileURLs))

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ft := range tasks {
				fileName, resp, err := getOriginalFileName(ctx, ft.url)
				if err != nil {
					log.Printf("Save task %s: failed to download file %d: %v", task.ID, ft.index, err)
					task.mu.Lock()
					task.Failed++
					task.mu.Unlock()
					resultsCh <- savedResult{index: ft.index, file: nil}
					continue
				}

				// Xử lý trùng tên
				namesMu.Lock()
				originalName := fileName
				if count, exists := usedNames[originalName]; exists {
					ext := path.Ext(fileName)
					base := fileName[:len(fileName)-len(ext)]
					fileName = fmt.Sprintf("%s_%d%s", base, count+1, ext)
				}
				usedNames[originalName]++
				namesMu.Unlock()

				// Lưu file vào disk
				filePath := fmt.Sprintf("%s/%s", taskDir, fileName)
				f, err := os.Create(filePath)
				if err != nil {
					resp.Body.Close()
					log.Printf("Save task %s: failed to create file: %v", task.ID, err)
					task.mu.Lock()
					task.Failed++
					task.mu.Unlock()
					resultsCh <- savedResult{index: ft.index, file: nil}
					continue
				}

				written, err := io.Copy(f, resp.Body)
				f.Close()
				resp.Body.Close()

				if err != nil {
					os.Remove(filePath)
					log.Printf("Save task %s: failed to write file: %v", task.ID, err)
					task.mu.Lock()
					task.Failed++
					task.mu.Unlock()
					resultsCh <- savedResult{index: ft.index, file: nil}
					continue
				}

				scheme := "https"
				fileURL := fmt.Sprintf("%s://%s/files/%s/%s", scheme, host, task.ID, url.PathEscape(fileName))

				task.mu.Lock()
				task.Completed++
				log.Printf("Save task %s: downloaded %d/%d - %s", task.ID, task.Completed, task.Total, fileName)
				task.mu.Unlock()

				resultsCh <- savedResult{
					index: ft.index,
					file: &SavedFile{
						Name: fileName,
						URL:  fileURL,
						Size: written,
					},
				}
			}
		}()
	}

	wg.Wait()
	close(resultsCh)

	// Thu thập kết quả theo thứ tự
	allResults := make([]*SavedFile, len(fileURLs))
	for r := range resultsCh {
		if r.file != nil {
			allResults[r.index] = r.file
		}
	}

	var files []SavedFile
	for _, f := range allResults {
		if f != nil {
			files = append(files, *f)
		}
	}

	now := time.Now()
	task.mu.Lock()
	task.Files = files
	task.CompletedAt = &now
	if task.Failed == task.Total {
		task.Status = StatusFailed
		task.Error = "All files failed to download"
	} else {
		task.Status = StatusDone
	}
	task.mu.Unlock()

	log.Printf("Save task %s completed: %d/%d files saved", task.ID, len(files), task.Total)
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

	// Set headers
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", session.ZipName))

	// Flush headers ngay lập tức để browser hiện download bar sớm (không phải chờ file đầu tiên)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	zipWriter := zip.NewWriter(w)
	defer zipWriter.Close()

	// Context với timeout cho toàn bộ download
	ctx, cancel := context.WithTimeout(r.Context(), DownloadTimeout)
	defer cancel()

	fileCount := len(session.Files)
	usedNames := make(map[string]int)

	// File đầu tiên: stream trực tiếp vào ZIP (không qua temp file) để giảm TTFB
	if fileCount > 0 {
		log.Printf("Streaming first file directly to ZIP for faster TTFB")
		fileName, resp, err := getOriginalFileName(ctx, session.Files[0])
		if err != nil {
			log.Printf("Error downloading first file: %v", err)
		} else {
			usedNames[fileName]++
			log.Printf("Writing to ZIP: %s (file 1/%d) [direct stream]", fileName, fileCount)

			header := &zip.FileHeader{
				Name:   fileName,
				Method: zip.Store,
			}
			header.SetModTime(time.Now())

			fileWriter, zerr := zipWriter.CreateHeader(header)
			if zerr != nil {
				log.Printf("Error creating ZIP entry: %v", zerr)
				resp.Body.Close()
			} else {
				_, zerr = io.Copy(fileWriter, resp.Body)
				resp.Body.Close()
				if zerr != nil {
					log.Printf("Error writing first file to ZIP: %v", zerr)
				}
				// Flush sau file đầu tiên để browser nhận data sớm
				zipWriter.Flush()
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	}

	// Các file còn lại: dùng prefetch + temp files như cũ
	remainingFiles := session.Files[1:]
	remainingCount := len(remainingFiles)

	if remainingCount > 0 {
		// Tạo temp directory cho prefetch (chỉ khi có file còn lại)
		tempDir, err := os.MkdirTemp("", "zip-download-*")
		if err != nil {
			log.Printf("Failed to create temp dir: %v", err)
			return
		}
		defer os.RemoveAll(tempDir)

		results := make([]*FileResult, remainingCount)
		done := make([]chan struct{}, remainingCount)
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
			for i, fileURL := range remainingFiles {
				select {
				case sem <- struct{}{}: // Acquire semaphore
				case <-ctx.Done():
					for j := i; j < remainingCount; j++ {
						results[j] = &FileResult{Index: j, Err: ctx.Err()}
						close(done[j])
					}
					return
				}

				select {
				case tasks <- FileTask{Index: i, URL: fileURL}:
				case <-ctx.Done():
					<-sem
					for j := i; j < remainingCount; j++ {
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
		for i := 0; i < remainingCount; i++ {
			select {
			case <-done[i]:
			case <-ctx.Done():
				log.Printf("Download timeout for token: %s", token)
				return
			}

			result := results[i]
			if result.Err != nil {
				log.Printf("Error downloading file %d: %v", i+1, result.Err)
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

			log.Printf("Writing to ZIP: %s (file %d/%d)", fileName, i+2, fileCount)

			if err := streamTempFileToZip(zipWriter, result.TempPath, fileName); err != nil {
				log.Printf("Error writing to ZIP: %v", err)
			}

			// Xoá temp file và release semaphore
			os.Remove(result.TempPath)
			<-sem
		}
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

