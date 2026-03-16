package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LoadManifest reads and parses a single agent manifest from a YAML file.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}

	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}

	if m.Name == "" {
		return nil, fmt.Errorf("manifest %s: name is required", path)
	}

	// Apply defaults
	if m.Provider.Kind == "" {
		m.Provider.Kind = "claude-cli"
	}

	return &m, nil
}

// LoadManifests reads all *.yaml and *.yml files from a directory.
// Returns an empty slice (not error) if the directory doesn't exist.
func LoadManifests(dir string) ([]*Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read agents dir %s: %w", dir, err)
	}

	var manifests []*Manifest
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		m, err := LoadManifest(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, m)
	}
	return manifests, nil
}
