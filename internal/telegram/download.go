package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/rcliao/teeny-relay/internal/bridge"
)

// DownloadPhoto fetches the largest resolution of a Telegram photo to a
// temporary file and returns the image info including metadata. The caller is
// responsible for removing the file when it is no longer needed.
func DownloadPhoto(ctx context.Context, b *bot.Bot, photos []models.PhotoSize) (bridge.ImageInfo, error) {
	if len(photos) == 0 {
		return bridge.ImageInfo{}, fmt.Errorf("no photo sizes provided")
	}

	// Telegram sends multiple resolutions; pick the largest file.
	best := photos[0]
	for _, p := range photos[1:] {
		if p.FileSize > best.FileSize {
			best = p
		}
	}

	// Ask the Telegram API for the file path on their servers.
	file, err := b.GetFile(ctx, &bot.GetFileParams{FileID: best.FileID})
	if err != nil {
		return bridge.ImageInfo{}, fmt.Errorf("get file metadata: %w", err)
	}

	downloadURL := b.FileDownloadLink(file)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return bridge.ImageInfo{}, fmt.Errorf("create download request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return bridge.ImageInfo{}, fmt.Errorf("download photo: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return bridge.ImageInfo{}, fmt.Errorf("download photo: unexpected status %d", resp.StatusCode)
	}

	ext := filepath.Ext(file.FilePath)
	if ext == "" {
		ext = ".jpg"
	}

	tmp, err := os.CreateTemp("", "tg-photo-*"+ext)
	if err != nil {
		return bridge.ImageInfo{}, fmt.Errorf("create temp file: %w", err)
	}

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return bridge.ImageInfo{}, fmt.Errorf("write photo to disk: %w", err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return bridge.ImageInfo{}, fmt.Errorf("close temp file: %w", err)
	}

	return bridge.ImageInfo{
		Path:   tmp.Name(),
		Width:  best.Width,
		Height: best.Height,
		Size:   int64(best.FileSize),
	}, nil
}

// IsImageDocument reports whether a Telegram Document has an image MIME type
// (e.g. image/jpeg, image/png). This is how Telegram represents uncompressed
// photos sent via the attachment/document picker.
func IsImageDocument(doc *models.Document) bool {
	return doc != nil && strings.HasPrefix(doc.MimeType, "image/")
}

// DownloadDocument fetches a Telegram document to a temporary file and returns
// image info including metadata. The extension is derived from the original
// file name, falling back to the MIME subtype. The caller is responsible for
// removing the file.
func DownloadDocument(ctx context.Context, b *bot.Bot, doc *models.Document) (bridge.ImageInfo, error) {
	if doc == nil {
		return bridge.ImageInfo{}, fmt.Errorf("no document provided")
	}

	file, err := b.GetFile(ctx, &bot.GetFileParams{FileID: doc.FileID})
	if err != nil {
		return bridge.ImageInfo{}, fmt.Errorf("get file metadata: %w", err)
	}

	downloadURL := b.FileDownloadLink(file)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return bridge.ImageInfo{}, fmt.Errorf("create download request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return bridge.ImageInfo{}, fmt.Errorf("download document: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return bridge.ImageInfo{}, fmt.Errorf("download document: unexpected status %d", resp.StatusCode)
	}

	ext := filepath.Ext(doc.FileName)
	if ext == "" {
		// Derive from MIME type: "image/png" → ".png"
		if _, sub, ok := strings.Cut(doc.MimeType, "/"); ok && sub != "" {
			ext = "." + sub
		} else {
			ext = ".bin"
		}
	}

	tmp, err := os.CreateTemp("", "tg-doc-*"+ext)
	if err != nil {
		return bridge.ImageInfo{}, fmt.Errorf("create temp file: %w", err)
	}

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return bridge.ImageInfo{}, fmt.Errorf("write document to disk: %w", err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return bridge.ImageInfo{}, fmt.Errorf("close temp file: %w", err)
	}

	return bridge.ImageInfo{
		Path: tmp.Name(),
		Size: doc.FileSize,
	}, nil
}
