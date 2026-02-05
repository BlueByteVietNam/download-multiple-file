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
	"path"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ============== CONFIG ==============
const (
	SessionTTL      = 1 * time.Hour    // Session hết hạn sau 1 giờ
	CleanupInterval = 5 * time.Minute  // Cleanup mỗi 5 phút
	HTTPTimeout     = 5 * time.Minute  // Timeout cho mỗi HTTP request
	DownloadTimeout = 30 * time.Minute // Timeout cho toàn bộ download
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

	// HTTP client với timeout
	httpClient = &http.Client{
		Timeout: HTTPTimeout,
	}
)

// ============== MAIN ==============

func main() {
	// Khởi động cleanup goroutine
	go cleanupExpiredSessions()

	http.HandleFunc("/create", handleCreate)
	http.HandleFunc("/download/", handleDownload)

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
		DownloadURL: fmt.Sprintf("http://%s/download/%s", r.Host, token),
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

	// Context với timeout cho toàn bộ download
	ctx, cancel := context.WithTimeout(r.Context(), DownloadTimeout)
	defer cancel()

	for _, fileURL := range session.Files {
		// Check context trước mỗi file
		select {
		case <-ctx.Done():
			log.Printf("Download timeout for token: %s", token)
			return
		default:
		}

		fileName, resp, err := getOriginalFileName(ctx, fileURL)
		if err != nil {
			log.Printf("Error fetching %s: %v", fileURL, err)
			continue
		}

		// Xử lý trùng tên
		if count, exists := usedNames[fileName]; exists {
			ext := path.Ext(fileName)
			base := fileName[:len(fileName)-len(ext)]
			fileName = fmt.Sprintf("%s_%d%s", base, count+1, ext)
		}
		usedNames[fileName]++

		log.Printf("Streaming: %s -> %s", fileURL, fileName)

		if err := streamToZip(zipWriter, resp, fileName); err != nil {
			log.Printf("Error streaming: %v", err)
			resp.Body.Close()
			continue
		}
		resp.Body.Close()
	}

	// Xóa session sau khi download xong
	mu.Lock()
	delete(sessions, token)
	mu.Unlock()

	log.Printf("Download completed for token: %s", token)
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
