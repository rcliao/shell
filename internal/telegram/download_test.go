package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func TestDownloadPhoto(t *testing.T) {
	// Tiny 1x1 red JPEG (smallest valid JPEG).
	jpegBytes := []byte{
		0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46,
		0x49, 0x46, 0x00, 0x01, 0x01, 0x00, 0x00, 0x01,
		0x00, 0x01, 0x00, 0x00, 0xFF, 0xD9,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/botTEST_TOKEN/getMe":
			json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": map[string]any{"id": 1, "is_bot": true, "first_name": "Test"},
			})
		case r.URL.Path == "/botTEST_TOKEN/getFile":
			json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": models.File{
					FileID:   "abc123",
					FilePath: "photos/test.jpg",
				},
			})
		case r.URL.Path == "/file/botTEST_TOKEN/photos/test.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(jpegBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b, err := bot.New("TEST_TOKEN", bot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}

	photos := []models.PhotoSize{
		{FileID: "small", Width: 90, Height: 90, FileSize: 100},
		{FileID: "abc123", Width: 800, Height: 600, FileSize: 50000},
		{FileID: "medium", Width: 320, Height: 240, FileSize: 5000},
	}

	img, err := DownloadPhoto(context.Background(), b, photos)
	if err != nil {
		t.Fatalf("DownloadPhoto: %v", err)
	}
	defer os.Remove(img.Path)

	data, err := os.ReadFile(img.Path)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}

	if len(data) != len(jpegBytes) {
		t.Errorf("file size = %d, want %d", len(data), len(jpegBytes))
	}

	if ext := img.Path[len(img.Path)-4:]; ext != ".jpg" {
		t.Errorf("extension = %q, want .jpg", ext)
	}

	// Verify metadata from the largest PhotoSize.
	if img.Width != 800 || img.Height != 600 {
		t.Errorf("dimensions = %dx%d, want 800x600", img.Width, img.Height)
	}
	if img.Size != 50000 {
		t.Errorf("size = %d, want 50000", img.Size)
	}
}

func TestDownloadPhoto_EmptySlice(t *testing.T) {
	_, err := DownloadPhoto(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error for empty photo slice")
	}
}

func TestIsImageDocument(t *testing.T) {
	tests := []struct {
		name string
		doc  *models.Document
		want bool
	}{
		{"nil document", nil, false},
		{"image/jpeg", &models.Document{MimeType: "image/jpeg"}, true},
		{"image/png", &models.Document{MimeType: "image/png"}, true},
		{"image/webp", &models.Document{MimeType: "image/webp"}, true},
		{"application/pdf", &models.Document{MimeType: "application/pdf"}, false},
		{"text/plain", &models.Document{MimeType: "text/plain"}, false},
		{"empty mime", &models.Document{MimeType: ""}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsImageDocument(tt.doc); got != tt.want {
				t.Errorf("IsImageDocument() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDownloadDocument(t *testing.T) {
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/botTEST_TOKEN/getMe":
			json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": map[string]any{"id": 1, "is_bot": true, "first_name": "Test"},
			})
		case r.URL.Path == "/botTEST_TOKEN/getFile":
			json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": models.File{
					FileID:   "doc123",
					FilePath: "documents/photo.png",
				},
			})
		case r.URL.Path == "/file/botTEST_TOKEN/documents/photo.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b, err := bot.New("TEST_TOKEN", bot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}

	doc := &models.Document{
		FileID:   "doc123",
		FileName: "photo.png",
		MimeType: "image/png",
		FileSize: int64(len(pngBytes)),
	}

	img, err := DownloadDocument(context.Background(), b, doc)
	if err != nil {
		t.Fatalf("DownloadDocument: %v", err)
	}
	defer os.Remove(img.Path)

	data, err := os.ReadFile(img.Path)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}

	if len(data) != len(pngBytes) {
		t.Errorf("file size = %d, want %d", len(data), len(pngBytes))
	}

	if ext := img.Path[len(img.Path)-4:]; ext != ".png" {
		t.Errorf("extension = %q, want .png", ext)
	}

	// Verify file size metadata from Document.
	if img.Size != int64(len(pngBytes)) {
		t.Errorf("size = %d, want %d", img.Size, len(pngBytes))
	}
}

func TestDownloadDocument_NilDoc(t *testing.T) {
	_, err := DownloadDocument(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error for nil document")
	}
}

func TestIsPDFDocument(t *testing.T) {
	tests := []struct {
		name string
		doc  *models.Document
		want bool
	}{
		{"nil document", nil, false},
		{"application/pdf", &models.Document{MimeType: "application/pdf"}, true},
		{"image/jpeg", &models.Document{MimeType: "image/jpeg"}, false},
		{"text/plain", &models.Document{MimeType: "text/plain"}, false},
		{"empty mime", &models.Document{MimeType: ""}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsPDFDocument(tt.doc); got != tt.want {
				t.Errorf("IsPDFDocument() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDownloadPDF(t *testing.T) {
	// Minimal PDF header bytes.
	pdfBytes := []byte("%PDF-1.4 minimal test content")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/botTEST_TOKEN/getMe":
			json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": map[string]any{"id": 1, "is_bot": true, "first_name": "Test"},
			})
		case r.URL.Path == "/botTEST_TOKEN/getFile":
			json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": models.File{
					FileID:   "pdf123",
					FilePath: "documents/report.pdf",
				},
			})
		case r.URL.Path == "/file/botTEST_TOKEN/documents/report.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(pdfBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b, err := bot.New("TEST_TOKEN", bot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}

	doc := &models.Document{
		FileID:   "pdf123",
		FileName: "report.pdf",
		MimeType: "application/pdf",
		FileSize: int64(len(pdfBytes)),
	}

	info, err := DownloadPDF(context.Background(), b, doc)
	if err != nil {
		t.Fatalf("DownloadPDF: %v", err)
	}
	defer os.Remove(info.Path)

	data, err := os.ReadFile(info.Path)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}

	if len(data) != len(pdfBytes) {
		t.Errorf("file size = %d, want %d", len(data), len(pdfBytes))
	}

	if ext := info.Path[len(info.Path)-4:]; ext != ".pdf" {
		t.Errorf("extension = %q, want .pdf", ext)
	}

	if info.Size != int64(len(pdfBytes)) {
		t.Errorf("size = %d, want %d", info.Size, len(pdfBytes))
	}
}

func TestDownloadPDF_NilDoc(t *testing.T) {
	_, err := DownloadPDF(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error for nil document")
	}
}

func TestDownloadPDF_WrongMIME(t *testing.T) {
	doc := &models.Document{
		FileID:   "img123",
		MimeType: "image/png",
	}
	_, err := DownloadPDF(context.Background(), nil, doc)
	if err == nil {
		t.Fatal("expected error for non-PDF document")
	}
}

func TestDownloadPDF_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/botTEST_TOKEN/getMe":
			json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": map[string]any{"id": 1, "is_bot": true, "first_name": "Test"},
			})
		case r.URL.Path == "/botTEST_TOKEN/getFile":
			json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": models.File{
					FileID:   "pdf123",
					FilePath: "documents/report.pdf",
				},
			})
		case r.URL.Path == "/file/botTEST_TOKEN/documents/report.pdf":
			http.Error(w, "internal server error", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b, err := bot.New("TEST_TOKEN", bot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}

	doc := &models.Document{
		FileID:   "pdf123",
		FileName: "report.pdf",
		MimeType: "application/pdf",
	}

	_, err = DownloadPDF(context.Background(), b, doc)
	if err == nil {
		t.Fatal("expected error for HTTP 500 response")
	}
	if got := err.Error(); !strings.Contains(got, "unexpected status") {
		t.Errorf("expected 'unexpected status' in error, got %q", got)
	}
}

// TestPDFHandlerRouting verifies the match function logic used in bot.go
// to route PDF documents to the PDF handler.
func TestPDFHandlerRouting(t *testing.T) {
	// The match function in bot.go is:
	//   update.Message != nil && IsPDFDocument(update.Message.Document)
	// We test all combinations.
	tests := []struct {
		name    string
		msg     *models.Message
		want    bool
	}{
		{
			name: "pdf document matches",
			msg:  &models.Message{Document: &models.Document{MimeType: "application/pdf"}},
			want: true,
		},
		{
			name: "image document does not match",
			msg:  &models.Message{Document: &models.Document{MimeType: "image/png"}},
			want: false,
		},
		{
			name: "nil document does not match",
			msg:  &models.Message{},
			want: false,
		},
		{
			name: "nil message does not match",
			msg:  nil,
			want: false,
		},
		{
			name: "text message without document does not match",
			msg:  &models.Message{Text: "hello"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Replicate the match logic from bot.go.
			got := tt.msg != nil && IsPDFDocument(tt.msg.Document)
			if got != tt.want {
				t.Errorf("PDF match = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDownloadDocument_MIMEFallback(t *testing.T) {
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/botTEST_TOKEN/getMe":
			json.NewEncoder(w).Encode(map[string]any{
				"ok":     true,
				"result": map[string]any{"id": 1, "is_bot": true, "first_name": "Test"},
			})
		case r.URL.Path == "/botTEST_TOKEN/getFile":
			json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": models.File{
					FileID:   "doc456",
					FilePath: "documents/noext",
				},
			})
		case r.URL.Path == "/file/botTEST_TOKEN/documents/noext":
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	b, err := bot.New("TEST_TOKEN", bot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}

	// Document with no file extension but has MIME type.
	doc := &models.Document{
		FileID:   "doc456",
		FileName: "noext",
		MimeType: "image/jpeg",
	}

	img, err := DownloadDocument(context.Background(), b, doc)
	if err != nil {
		t.Fatalf("DownloadDocument: %v", err)
	}
	defer os.Remove(img.Path)

	if ext := img.Path[len(img.Path)-5:]; ext != ".jpeg" {
		t.Errorf("extension = %q, want .jpeg", ext)
	}
}
