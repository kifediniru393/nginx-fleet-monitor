package nginxconf

import (
	"os"
	"path/filepath"
	"strings"
)

// LoadFromDisk reads an nginx config from disk, resolving include directives
// (with globs) recursively, and returns text in the same file-marker format
// as `nginx -T`. Fallback for when nginx -T is unavailable — nginx -T
// validates TLS key files, which an unprivileged exporter cannot read, while
// the config files themselves are typically world-readable.
//
// Tradeoff vs nginx -T: this is the on-disk config, which can be newer than
// the running one if a reload hasn't happened since the last edit.
func LoadFromDisk(root string) (string, error) {
	var b strings.Builder
	seen := map[string]bool{}
	if err := loadFile(root, filepath.Dir(root), &b, seen); err != nil {
		return "", err
	}
	return b.String(), nil
}

func loadFile(path, prefix string, b *strings.Builder, seen map[string]bool) error {
	abs, _ := filepath.Abs(path)
	if seen[abs] {
		return nil // include cycle
	}
	seen[abs] = true
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	b.WriteString("# configuration file " + path + ":\n")
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "include ") && strings.HasSuffix(trimmed, ";") {
			pattern := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "include"), ";"))
			if !filepath.IsAbs(pattern) {
				pattern = filepath.Join(prefix, pattern)
			}
			matches, _ := filepath.Glob(pattern)
			for _, m := range matches {
				// Best-effort per include: one unreadable file shouldn't
				// blank the whole topology.
				loadFile(m, prefix, b, seen)
			}
			continue
		}
		b.WriteString(line + "\n")
	}
	return nil
}
