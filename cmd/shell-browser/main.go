// shell-browser is a standalone CLI for headless Chrome automation, used as a skill script.
// It reuses the shell-browser package for all browser interaction.
//
// Usage: shell-browser <url> [actions...]
//
// Actions are passed as separate arguments, one per action line.
// Screenshots are saved to temp files and output as artifact markers.
// Text results (extract, js) are printed to stdout.
//
// Example:
//
//	shell-browser "https://example.com" screenshot
//	shell-browser "https://example.com" 'click "#btn"' screenshot 'extract ".content"'
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	shellbrowser "github.com/rcliao/shell-browser"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: shell-browser <url> [action...]")
		fmt.Fprintln(os.Stderr, "actions: screenshot, click \"sel\", type \"sel\" \"val\", wait \"sel\", extract \"sel\", js \"expr\", sleep \"dur\"")
		os.Exit(1)
	}

	url := os.Args[1]
	body := strings.Join(os.Args[2:], "\n")

	cfg := shellbrowser.Config{
		Enabled:        true,
		Headless:       true,
		TimeoutSeconds: 30,
	}
	if cp := os.Getenv("CHROME_PATH"); cp != "" {
		cfg.ChromePath = cp
	}
	if os.Getenv("BROWSER_HEADLESS") == "false" {
		cfg.Headless = false
	}

	directive := shellbrowser.ParseDirective(url, body)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	result := shellbrowser.Execute(ctx, cfg, directive)

	// Process results: save screenshots as artifacts, print text results
	for _, step := range result.Steps {
		if step.Err != nil {
			fmt.Fprintf(os.Stderr, "step %d (%s): ERROR: %s\n", step.Step, step.Description, step.Err)
			continue
		}
		if step.Screenshot != nil {
			tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("shell-browser-%d.png", time.Now().UnixNano()))
			if err := os.WriteFile(tmpFile, step.Screenshot, 0644); err != nil {
				fmt.Fprintf(os.Stderr, "failed to save screenshot: %v\n", err)
				continue
			}
			fmt.Printf("[artifact type=\"image\" path=\"%s\" caption=\"Screenshot of %s\"]\n", tmpFile, url)
		} else {
			fmt.Printf("Step %d: %s → %s\n", step.Step, step.Description, step.Output)
		}
	}
}
