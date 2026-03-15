// shell-imagen is a standalone CLI for image generation, used as a skill script.
// It reuses the shell-imagen package and reads the API key from GEMINI_API_KEY env.
//
// Usage: shell-imagen <prompt>
//
// Generates an image and writes it to a temp file. Outputs the file path to stdout
// prefixed with [artifact:image] so the agent can report it back.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	shellimagen "github.com/rcliao/shell-imagen"
)

func main() {
	prompt := strings.Join(os.Args[1:], " ")
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "usage: shell-imagen <prompt>")
		os.Exit(1)
	}

	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "error: GEMINI_API_KEY not set")
		os.Exit(1)
	}

	gen, err := shellimagen.New(apiKey, "", 2*time.Minute)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	imageData, err := gen.Generate(ctx, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generation failed: %v\n", err)
		os.Exit(1)
	}

	// Write to temp file
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("shell-imagen-%d.png", time.Now().UnixNano()))
	if err := os.WriteFile(tmpFile, imageData, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write image: %v\n", err)
		os.Exit(1)
	}

	// Output artifact marker for the agent/bridge to parse
	fmt.Printf("[artifact type=\"image\" path=\"%s\" caption=\"%s\"]\n", tmpFile, prompt)
}
