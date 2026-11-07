package discord

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// A multi-photo message must download its photos at the same time, not one
// after another, and keep them in the order they were attached.
func TestDownloadAllFetchesConcurrentlyInOrder(t *testing.T) {
	const delay = 300 * time.Millisecond
	var inFlight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(delay)
		inFlight.Add(-1)
		w.Write([]byte("photo:" + r.URL.Path))
	}))
	defer srv.Close()

	h := newHarness(t, "ok")
	atts := []*discordgo.MessageAttachment{
		{URL: srv.URL + "/a", Filename: "a.jpg", ContentType: "image/jpeg", Size: 10},
		{URL: "http://127.0.0.1:1/x.zip", Filename: "x.zip", ContentType: "application/zip", Size: 10},
		{URL: srv.URL + "/b", Filename: "b.jpg", ContentType: "image/jpeg", Size: 10},
		{URL: srv.URL + "/c", Filename: "c.pdf", ContentType: "application/pdf", Size: 10},
	}
	start := time.Now()
	got, ok := h.bot.handler.downloadAll(context.Background(), atts)
	elapsed := time.Since(start)
	if !ok {
		t.Fatal("all downloads should succeed")
	}
	if peak.Load() < 3 {
		t.Fatalf("peak concurrent downloads = %d, want 3", peak.Load())
	}
	if elapsed > 2*delay {
		t.Fatalf("3 downloads of %s each took %s; they ran one after another", delay, elapsed)
	}
	for i, want := range []string{"/a", "", "/b", "/c"} {
		if want == "" {
			if got[i].path != "" {
				t.Fatalf("unsupported attachment %d must not be downloaded", i)
			}
			continue
		}
		data, err := os.ReadFile(got[i].path)
		if err != nil || string(data) != "photo:"+want {
			t.Fatalf("attachment %d: got %q (err %v), want photo:%s", i, data, err, want)
		}
		os.Remove(got[i].path)
	}
}

// One failed download fails the message and leaves no temp files behind.
func TestDownloadAllFailureRemovesTheOthers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/bad") {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	h := newHarness(t, "ok")
	atts := []*discordgo.MessageAttachment{
		{URL: srv.URL + "/good1", Filename: "a.jpg", ContentType: "image/jpeg", Size: 2},
		{URL: srv.URL + "/bad", Filename: "b.jpg", ContentType: "image/jpeg", Size: 2},
		{URL: srv.URL + "/good2", Filename: "c.jpg", ContentType: "image/jpeg", Size: 2},
	}
	if _, ok := h.bot.handler.downloadAll(context.Background(), atts); ok {
		t.Fatal("a failed download must fail the batch")
	}
	left, _ := os.ReadDir(tmp)
	for _, e := range left {
		if strings.HasPrefix(e.Name(), "dc-photo-") {
			t.Fatalf("temp file %s left behind after a failed batch", e.Name())
		}
	}
}
