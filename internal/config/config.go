// Package config loads and validates ghaas manifests from disk or YAML.
package config

import (
	"fmt"
	"io"
	"os"

	"ghaas/pkg/manifest"
)

// Config is an alias for the public manifest representation.
// It is provided so callers can refer to configuration without importing the
// implementation package used by the loader.
type Config = manifest.Manifest

// Decode parses a manifest without applying semantic validation. YAML decoding
// remains strict: unknown fields and multiple documents are rejected.
func Decode(r io.Reader) (manifest.Manifest, error) {
	return manifest.Decode(r)
}

// Parse parses and semantically validates one YAML manifest.
func Parse(data []byte) (manifest.Manifest, error) {
	m, err := manifest.Parse(data)
	if err != nil {
		return manifest.Manifest{}, err
	}
	if err := Validate(m); err != nil {
		return manifest.Manifest{}, err
	}
	return m, nil
}

// ParseFile reads, parses, and validates a manifest file.
func ParseFile(path string) (manifest.Manifest, error) { return Load(path) }

// LoadReader parses and validates a manifest from an already-open reader.
func LoadReader(r io.Reader) (manifest.Manifest, error) {
	m, err := Decode(r)
	if err != nil {
		return manifest.Manifest{}, err
	}
	if err := Validate(m); err != nil {
		return manifest.Manifest{}, err
	}
	return m, nil
}

// Load reads, parses, and validates a manifest file.
func Load(path string) (manifest.Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("open manifest %q: %w", path, err)
	}
	defer f.Close()
	m, err := Decode(f)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("read manifest %q: %w", path, err)
	}
	if err := Validate(m); err != nil {
		return manifest.Manifest{}, fmt.Errorf("validate manifest %q: %w", path, err)
	}
	return m, nil
}

// LoadFile is an explicit alias for Load.
func LoadFile(path string) (manifest.Manifest, error) { return Load(path) }
