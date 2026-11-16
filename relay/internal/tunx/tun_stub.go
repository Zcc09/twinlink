//go:build !linux

package tunx

// Open is unavailable off Linux: this file exists so that the relay's logic
// packages and their tests compile on Windows/macOS instead of being excluded.
func Open(cfg Config) (Device, error) { return nil, ErrUnsupported }
