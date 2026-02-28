package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ============== CONFIG ==============
const (
	SessionTTL      = 1 * time.Hour   // Session hết hạn sau 1 giờ
	CleanupInterval = 5 * time.Minute // Cleanup mỗi 5 phút

	// Transport-level timeouts (chỉ timeout connect/header, KHÔNG timeout body)
	ConnectTimeout        = 30 * time.Second // TCP connect timeout
	TLSHandshakeTimeout   = 15 * time.Second // TLS handshake timeout
	ResponseHeaderTimeout = 30 * time.Second // Chờ response header

	// Download timeout tính động: base + perFile * numFiles
	DownloadTimeoutBase    = 5 * time.Minute // Base timeout
	DownloadTimeoutPerFile = 3 * time.Minute // Thêm mỗi file

	// Retry config
	MaxRetries    = 3
	RetryBaseWait = 1 * time.Second // Backoff: 1s, 2s, 4s
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

// ============== GLOBAL STATE ==============

var (
	sessions = make(map[string]*Session)
	mu       sync.RWMutex

	// HTTP client: chỉ timeout connect/header, KHÔNG timeout đọc body
	// → video lớn stream chậm cũng không bị cắt
	httpClient = &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: ConnectTimeout}).DialContext,
			TLSHandshakeTimeout:   TLSHandshakeTimeout,
			ResponseHeaderTimeout: ResponseHeaderTimeout,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
		},
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
	log.Printf("Server running on %s (Session TTL: %v, Connect: %v, Header: %v)",
		port, SessionTTL, ConnectTimeout, ResponseHeaderTimeout)
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

	// Set headers
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", session.ZipName))

	zipWriter := zip.NewWriter(w)
	defer zipWriter.Close()

	usedNames := make(map[string]int)

	// Dynamic timeout: base + perFile * numFiles
	downloadTimeout := DownloadTimeoutBase + DownloadTimeoutPerFile*time.Duration(len(session.Files))
	ctx, cancel := context.WithTimeout(r.Context(), downloadTimeout)
	defer cancel()

	log.Printf("Starting download for token %s: %d files, timeout: %v", token, len(session.Files), downloadTimeout)

	successCount := 0
	failCount := 0

	for i, fileURL := range session.Files {
		// Check context trước mỗi file
		select {
		case <-ctx.Done():
			log.Printf("Download timeout for token %s after %d/%d files", token, i, len(session.Files))
			return
		default:
		}

		// Fetch với retry
		fileName, resp, err := fetchWithRetry(ctx, fileURL, MaxRetries)
		if err != nil {
			log.Printf("[%d/%d] FAILED %s: %v", i+1, len(session.Files), fileURL, err)
			failCount++
			continue
		}

		// Xử lý trùng tên - lưu tên gốc để đếm chính xác
		originalName := fileName
		if count, exists := usedNames[originalName]; exists {
			ext := path.Ext(fileName)
			base := fileName[:len(fileName)-len(ext)]
			fileName = fmt.Sprintf("%s_%d%s", base, count+1, ext)
		}
		usedNames[originalName]++

		log.Printf("[%d/%d] Streaming: %s -> %s", i+1, len(session.Files), fileURL, fileName)

		if err := streamToZip(zipWriter, resp, fileName); err != nil {
			log.Printf("[%d/%d] Error streaming %s: %v", i+1, len(session.Files), fileName, err)
			resp.Body.Close()
			failCount++
			continue
		}
		resp.Body.Close()
		successCount++
	}

	// Xóa session sau khi download xong
	mu.Lock()
	delete(sessions, token)
	mu.Unlock()

	log.Printf("Download completed for token %s: %d success, %d failed out of %d total",
		token, successCount, failCount, len(session.Files))
}

// ============== RETRY ==============

func fetchWithRetry(ctx context.Context, fileURL string, maxRetries int) (string, *http.Response, error) {
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Backoff trước retry (không backoff lần đầu)
		if attempt > 0 {
			backoff := RetryBaseWait * time.Duration(1<<(attempt-1)) // 1s, 2s, 4s
			log.Printf("Retry %d/%d for %s (waiting %v)", attempt, maxRetries, fileURL, backoff)

			select {
			case <-ctx.Done():
				return "", nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		fileName, resp, err := getOriginalFileName(ctx, fileURL)
		if err == nil {
			return fileName, resp, nil
		}

		lastErr = err

		// Không retry lỗi 4xx (client error) — chỉ retry lỗi recoverable
		if !isRetryableError(err) {
			return "", nil, err
		}
	}

	return "", nil, fmt.Errorf("after %d retries: %w", maxRetries, lastErr)
}

// isRetryableError kiểm tra lỗi có nên retry không
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	// Context cancelled/deadline → không retry
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	errStr := err.Error()

	// Lỗi 4xx → không retry (404, 403, etc.)
	if strings.Contains(errStr, "bad status 4") {
		return false
	}

	// Mọi lỗi khác (network, timeout, 5xx) → retry
	return true
}

// ============== HELPERS ==============

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

func streamToZip(zw *zip.Writer, resp *http.Response, fileName string) error {
	header := &zip.FileHeader{
		Name:   fileName,
		Method: zip.Store,
	}
	header.SetModTime(time.Now())

	fileWriter, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}

	_, err = io.Copy(fileWriter, resp.Body)
	return err
}
