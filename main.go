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
	StatusPending    SaveTaskStatus = "pending"    // Đang nhận file từ client
	StatusZipping    SaveTaskStatus = "zipping"    // Đang tạo ZIP
	StatusDone       SaveTaskStatus = "done"       // Xong, có link ZIP
	StatusFailed     SaveTaskStatus = "failed"
)

type SavedFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Status string `json:"status"` // "downloading", "done", "failed"
}

type SaveTask struct {
	ID          string         `json:"id"`
	Status      SaveTaskStatus `json:"status"`
	Total       int            `json:"total"`
	Downloaded  int            `json:"downloaded"`
	Failed      int            `json:"failed"`
	Files       []SavedFile    `json:"files,omitempty"`
	ZipName     string         `json:"zip_name,omitempty"`
	ZipURL      string         `json:"zip_url,omitempty"`
	ZipSize     int64          `json:"zip_size,omitempty"`
	Error       string         `json:"error,omitempty"`
	Host        string         `json:"-"`
	CreatedAt   time.Time      `json:"created_at"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	mu          sync.Mutex
	usedNames   map[string]int
}

// ============== GLOBAL STATE ==============

var (
	sessions = make(map[string]*Session)
	mu       sync.RWMutex

	saveTasks = make(map[string]*SaveTask)
	tasksMu   sync.RWMutex

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

	// ZIP streaming endpoints (existing)
	http.HandleFunc("/create", enableCORS(handleCreate))
	http.HandleFunc("/download/", enableCORS(handleDownload))

	// Save-to-server endpoints (new)
	http.HandleFunc("/save/init", enableCORS(handleSaveInit))
	http.HandleFunc("/save/add", enableCORS(handleSaveAdd))
	http.HandleFunc("/save/zip", enableCORS(handleSaveZip))
	http.HandleFunc("/save/status/", enableCORS(handleSaveStatus))

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

// POST /save/init — Tạo task mới
func handleSaveInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	taskID := uuid.New().String()
	task := &SaveTask{
		ID:        taskID,
		Status:    StatusPending,
		Host:      r.Host,
		CreatedAt: time.Now(),
		usedNames: make(map[string]int),
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"task_id": taskID,
		"status":  StatusPending,
	})

	log.Printf("Created save task %s", taskID)
}

// POST /save/add — Client gửi từng URL, server tải background
func handleSaveAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TaskID string `json:"task_id"`
		URL    string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.TaskID == "" || req.URL == "" {
		http.Error(w, "task_id and url are required", http.StatusBadRequest)
		return
	}

	tasksMu.RLock()
	task, exists := saveTasks[req.TaskID]
	tasksMu.RUnlock()

	if !exists {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	task.mu.Lock()
	if task.Status != StatusPending {
		task.mu.Unlock()
		http.Error(w, "Task is no longer accepting files", http.StatusConflict)
		return
	}
	task.Total++
	fileIndex := len(task.Files)
	task.Files = append(task.Files, SavedFile{Name: "", Size: 0, Status: "downloading"})
	task.mu.Unlock()

	// Download trong background
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), DownloadTimeout)
		defer cancel()

		taskDir := fmt.Sprintf("%s/%s", SavedFilesDir, task.ID)

		fileName, resp, err := getOriginalFileName(ctx, req.URL)
		if err != nil {
			log.Printf("Save task %s: failed to download: %v", task.ID, err)
			task.mu.Lock()
			task.Failed++
			task.Files[fileIndex] = SavedFile{Name: "", Status: "failed"}
			task.mu.Unlock()
			return
		}

		// Xử lý trùng tên
		task.mu.Lock()
		originalName := fileName
		if count, exists := task.usedNames[originalName]; exists {
			ext := path.Ext(fileName)
			base := fileName[:len(fileName)-len(ext)]
			fileName = fmt.Sprintf("%s_%d%s", base, count+1, ext)
		}
		task.usedNames[originalName]++
		task.mu.Unlock()

		// Lưu file vào disk
		filePath := fmt.Sprintf("%s/%s", taskDir, fileName)
		f, err := os.Create(filePath)
		if err != nil {
			resp.Body.Close()
			log.Printf("Save task %s: failed to create file: %v", task.ID, err)
			task.mu.Lock()
			task.Failed++
			task.Files[fileIndex] = SavedFile{Name: fileName, Status: "failed"}
			task.mu.Unlock()
			return
		}

		written, err := io.Copy(f, resp.Body)
		f.Close()
		resp.Body.Close()

		if err != nil {
			os.Remove(filePath)
			log.Printf("Save task %s: failed to write file: %v", task.ID, err)
			task.mu.Lock()
			task.Failed++
			task.Files[fileIndex] = SavedFile{Name: fileName, Status: "failed"}
			task.mu.Unlock()
			return
		}

		task.mu.Lock()
		task.Downloaded++
		task.Files[fileIndex] = SavedFile{Name: fileName, Size: written, Status: "done"}
		task.mu.Unlock()

		log.Printf("Save task %s: downloaded %s (%.2f MB)", task.ID, fileName, float64(written)/1024/1024)
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "accepted",
		"task_id": req.TaskID,
	})
}

// POST /save/zip — Client yêu cầu tạo ZIP từ các file đã tải
func handleSaveZip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TaskID  string `json:"task_id"`
		ZipName string `json:"zip_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.TaskID == "" {
		http.Error(w, "task_id is required", http.StatusBadRequest)
		return
	}

	tasksMu.RLock()
	task, exists := saveTasks[req.TaskID]
	tasksMu.RUnlock()

	if !exists {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	zipName := req.ZipName
	if zipName == "" {
		zipName = "files.zip"
	}

	task.mu.Lock()
	if task.Status != StatusPending {
		task.mu.Unlock()
		http.Error(w, "ZIP already requested", http.StatusConflict)
		return
	}
	task.ZipName = zipName
	task.Status = StatusZipping
	task.mu.Unlock()

	// Tạo ZIP trong background (chờ tất cả file tải xong trước)
	go func() {
		// Chờ tất cả file download xong
		for {
			task.mu.Lock()
			done := task.Downloaded + task.Failed >= task.Total
			task.mu.Unlock()
			if done {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		taskDir := fmt.Sprintf("%s/%s", SavedFilesDir, task.ID)

		// Thu thập file thành công
		task.mu.Lock()
		var filesToZip []SavedFile
		for _, f := range task.Files {
			if f.Status == "done" {
				filesToZip = append(filesToZip, f)
			}
		}
		task.mu.Unlock()

		if len(filesToZip) == 0 {
			now := time.Now()
			task.mu.Lock()
			task.Status = StatusFailed
			task.Error = "No files to zip"
			task.CompletedAt = &now
			task.mu.Unlock()
			return
		}

		// Tạo ZIP
		log.Printf("Save task %s: creating ZIP with %d files...", task.ID, len(filesToZip))
		zipPath := fmt.Sprintf("%s/%s", taskDir, task.ZipName)
		zipFile, err := os.Create(zipPath)
		if err != nil {
			now := time.Now()
			task.mu.Lock()
			task.Status = StatusFailed
			task.Error = fmt.Sprintf("Failed to create ZIP: %v", err)
			task.CompletedAt = &now
			task.mu.Unlock()
			return
		}

		zw := zip.NewWriter(zipFile)
		for _, f := range filesToZip {
			filePath := fmt.Sprintf("%s/%s", taskDir, f.Name)
			if err := addFileToZip(zw, filePath, f.Name); err != nil {
				log.Printf("Save task %s: error adding %s to ZIP: %v", task.ID, f.Name, err)
			}
		}
		zw.Close()
		zipFile.Close()

		// Lấy size ZIP
		zipInfo, _ := os.Stat(zipPath)
		var zipSize int64
		if zipInfo != nil {
			zipSize = zipInfo.Size()
		}

		// Xoá file rời, chỉ giữ ZIP
		for _, f := range filesToZip {
			os.Remove(fmt.Sprintf("%s/%s", taskDir, f.Name))
		}

		zipURL := fmt.Sprintf("https://%s/files/%s/%s", task.Host, task.ID, url.PathEscape(task.ZipName))

		now := time.Now()
		task.mu.Lock()
		task.ZipURL = zipURL
		task.ZipSize = zipSize
		task.Status = StatusDone
		task.CompletedAt = &now
		task.mu.Unlock()

		log.Printf("Save task %s: ZIP created - %s (%.2f MB)", task.ID, task.ZipName, float64(zipSize)/1024/1024)
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "zipping",
		"task_id": req.TaskID,
	})
}

// GET /save/status/{task_id}
func handleSaveStatus(w http.ResponseWriter, r *http.Request) {
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
		"downloaded": task.Downloaded,
		"failed":     task.Failed,
		"created_at": task.CreatedAt,
	}
	if task.Status == StatusDone {
		resp["zip_url"] = task.ZipURL
		resp["zip_name"] = task.ZipName
		resp["zip_size"] = task.ZipSize
		resp["completed_at"] = task.CompletedAt
	}
	if task.Error != "" {
		resp["error"] = task.Error
	}
	task.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func addFileToZip(zw *zip.Writer, filePath string, fileName string) error {
	fi, err := os.Stat(filePath)
	if err != nil {
		return err
	}

	header := &zip.FileHeader{
		Name:               fileName,
		Method:             zip.Store,
		UncompressedSize64: uint64(fi.Size()),
	}
	header.SetModTime(fi.ModTime())

	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(w, f)
	return err
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

