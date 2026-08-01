package launcher

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// A port registry is an optional, external file that becomes the authority for
// which port each project runs on. It exists because a port is not really a
// property of rover's view of a project: the project itself, its start script,
// and any agent or human running it by hand all need the same answer, and the
// only way they can agree is to read one shared file.
//
// Without --registry rover behaves exactly as before, using the port stored in
// projects_registry.json. Nothing about the open-source default changes.
//
// The file's shape is deliberately the same one the projects themselves read:
//
//	{"registry": {"some-app": {"port": 8766, "path": "/abs/path", "status": "active"}}}
//
// Only "port", "path", "status" and "note" are consulted. In particular the
// allocator hint some registries carry for the *next* project to be assigned is
// never read here — treating a reservation as a live port is exactly how two
// services end up believing they own the same number.
type portRegistry struct {
	entries map[string]portEntry
}

type portEntry struct {
	Port   int    `json:"port"`
	Path   string `json:"path"`
	Status string `json:"status"`
	Note   string `json:"note"`
}

type portRegistryFile struct {
	Registry map[string]portEntry `json:"registry"`
}

// loadPortRegistry reads the registry file. A missing or malformed file is an
// error rather than a silent fallback: the operator asked for this file to be
// authoritative, and quietly reverting to a second source is how the two drift
// apart without anyone noticing.
func loadPortRegistry(path string) (*portRegistry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("port registry %s: %w", path, err)
	}
	var f portRegistryFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("port registry %s: %w", path, err)
	}
	if f.Registry == nil {
		return nil, fmt.Errorf("port registry %s: no \"registry\" object", path)
	}
	return &portRegistry{entries: f.Registry}, nil
}

// samePath compares two filesystem paths for identity, case-insensitively on
// platforms whose paths are case-insensitive.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// lookup finds the entry for a project. Path is the primary key because the two
// registries name projects differently — one uses display names, the other
// slugs — while both record an absolute path, which is unambiguous. The name
// match is a fallback for entries that carry no path.
func (r *portRegistry) lookup(name, path string) (portEntry, string, bool) {
	if r == nil {
		return portEntry{}, "", false
	}
	for key, e := range r.entries {
		if samePath(e.Path, path) {
			return e, key, true
		}
	}
	if e, ok := r.entries[name]; ok {
		return e, name, true
	}
	return portEntry{}, "", false
}

// resolvePort returns the port a project should use for this run.
//
// It reports an error when the registry deliberately says the project must not
// run — a retired entry, or one with no port — so that retiring a project in
// one file actually stops it everywhere, instead of rover cheerfully starting
// it on a stale port it remembers.
func (m *Manager) resolvePort(proj *ProjectInfo) (int, error) {
	if m.portAuthority == "" {
		return proj.Port, nil
	}
	reg, err := loadPortRegistry(m.portAuthority)
	if err != nil {
		return 0, err
	}
	entry, key, found := reg.lookup(proj.Name, proj.Path)
	if !found {
		return proj.Port, nil
	}
	if strings.EqualFold(entry.Status, "retired") {
		if entry.Note != "" {
			return 0, fmt.Errorf("%s is retired in the port registry: %s", key, entry.Note)
		}
		return 0, fmt.Errorf("%s is retired in the port registry", key)
	}
	if entry.Port <= 0 {
		if entry.Note != "" {
			return 0, fmt.Errorf("%s has no port in the port registry: %s", key, entry.Note)
		}
		return 0, fmt.Errorf("%s has no port in the port registry", key)
	}
	if entry.Port != proj.Port {
		m.logf("launcher: project %q: port %d from the port registry overrides the stored %d",
			proj.Name, entry.Port, proj.Port)
	}
	return entry.Port, nil
}

// SetPortAuthority makes an external ports.json-format file authoritative for
// project ports, matched by project path. Empty (the default) keeps rover's own
// registry as the only source.
//
// Rover never writes to this file. It remains the operator's to edit, and every
// other tool that reads it sees the same value rover does.
func (m *Manager) SetPortAuthority(path string) {
	m.portAuthority = path
}

// PortAuthority reports the external registry path, or "" when unset.
func (m *Manager) PortAuthority() string {
	return m.portAuthority
}
