package browserhandoff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrServeDisabled means the tailnet has not enabled Serve (or HTTPS
// certificates) for this machine — an owner action in the admin console.
var ErrServeDisabled = errors.New("tailscale serve is not enabled on this tailnet (admin console: DNS → MagicDNS + HTTPS Certificates, then approve Serve)")

// Tailscale publishes paths with `tailscale serve`, reachable only from the
// tailnet (never funnel). Each handoff gets its own path; other paths in the
// machine's serve config — including the other agent's — are never touched.
type Tailscale struct {
	Bin string // default "tailscale"

	mu   sync.Mutex
	host string
}

func (t *Tailscale) bin() string {
	if t.Bin != "" {
		return t.Bin
	}
	return "tailscale"
}

// Publish maps path → http://127.0.0.1:port and returns the https link. The
// link ends in "/" because the viewer uses relative URLs and tailscale serve
// strips the path prefix when proxying.
func (t *Tailscale) Publish(ctx context.Context, path string, port int) (string, error) {
	host, err := t.Host(ctx)
	if err != nil {
		return "", err
	}
	// `serve` blocks waiting for an admin when Serve is disabled; bound it.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, t.bin(), "serve", "--bg", "--yes", "--set-path", path,
		fmt.Sprintf("http://127.0.0.1:%d", port)).CombinedOutput()
	if strings.Contains(string(out), "not enabled") {
		return "", ErrServeDisabled
	}
	if err != nil {
		return "", fmt.Errorf("tailscale serve: %v: %s", err, lastLine(out))
	}
	return "https://" + host + path + "/", nil
}

// Unpublish removes path from the serve config.
func (t *Tailscale) Unpublish(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, t.bin(), "serve", "--https=443", "--set-path", path, "off").CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "not found") || strings.Contains(string(out), "does not exist") {
			return nil // already gone
		}
		return fmt.Errorf("tailscale serve off: %v: %s", err, lastLine(out))
	}
	return nil
}

// Host is this machine's MagicDNS name, e.g. mini.tailnet.ts.net.
func (t *Tailscale) Host(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.host != "" {
		return t.host, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, t.bin(), "status", "--json").Output()
	if err != nil {
		return "", fmt.Errorf("tailscale status: %w", err)
	}
	var st struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
		CertDomains []string `json:"CertDomains"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return "", fmt.Errorf("tailscale status: %w", err)
	}
	if len(st.CertDomains) == 0 {
		return "", ErrServeDisabled
	}
	host := strings.TrimSuffix(st.Self.DNSName, ".")
	if host == "" {
		host = st.CertDomains[0]
	}
	t.host = host
	return host, nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
