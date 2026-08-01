package launcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writePortRegistry(t *testing.T, entries map[string]portEntry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ports.json")
	b, err := json.Marshal(portRegistryFile{Registry: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func quietManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(t.TempDir())
	m.SetLogger(func(string, ...any) {})
	return m
}

func TestResolvePortWithoutAuthorityKeepsStoredPort(t *testing.T) {
	m := quietManager(t)
	proj := &ProjectInfo{Name: "demo", Path: `C:\p\demo`, Port: 8100}
	got, err := m.resolvePort(proj)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 8100 {
		t.Fatalf("got %d, want the stored 8100", got)
	}
}

func TestResolvePortMatchesByPathNotName(t *testing.T) {
	// The two registries name the same project differently on purpose: rover
	// uses a display name, the port registry a slug. Path is what they share.
	dir := t.TempDir()
	reg := writePortRegistry(t, map[string]portEntry{
		"dream-job-prep": {Port: 8767, Path: dir, Status: "active"},
	})
	m := quietManager(t)
	m.SetPortAuthority(reg)

	proj := &ProjectInfo{Name: "Dream Job Prep", Path: dir, Port: 8765}
	got, err := m.resolvePort(proj)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 8767 {
		t.Fatalf("got %d, want 8767 from the registry", got)
	}
}

func TestResolvePortFallsBackToNameWhenNoPathRecorded(t *testing.T) {
	reg := writePortRegistry(t, map[string]portEntry{
		"demo": {Port: 8200, Status: "active"},
	})
	m := quietManager(t)
	m.SetPortAuthority(reg)

	got, err := m.resolvePort(&ProjectInfo{Name: "demo", Path: `C:\p\demo`, Port: 8100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 8200 {
		t.Fatalf("got %d, want 8200", got)
	}
}

func TestResolvePortUnknownProjectKeepsStoredPort(t *testing.T) {
	reg := writePortRegistry(t, map[string]portEntry{
		"other": {Port: 8200, Path: `C:\p\other`, Status: "active"},
	})
	m := quietManager(t)
	m.SetPortAuthority(reg)

	got, err := m.resolvePort(&ProjectInfo{Name: "demo", Path: `C:\p\demo`, Port: 8100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 8100 {
		t.Fatalf("got %d, want the stored 8100 for an unlisted project", got)
	}
}

func TestRetiredProjectRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	reg := writePortRegistry(t, map[string]portEntry{
		"investments": {Path: dir, Status: "retired",
			Note: "merged into family-finance-app; 8772 belongs to it now"},
	})
	m := quietManager(t)
	m.SetPortAuthority(reg)

	_, err := m.resolvePort(&ProjectInfo{Name: "investments", Path: dir, Port: 8772})
	if err == nil {
		t.Fatal("a retired project must not start")
	}
	if !strings.Contains(err.Error(), "family-finance-app") {
		t.Fatalf("the note explains why; got %q", err)
	}
}

func TestEntryWithoutAPortRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	reg := writePortRegistry(t, map[string]portEntry{
		"demo": {Path: dir, Status: "active", Note: "port deliberately unassigned"},
	})
	m := quietManager(t)
	m.SetPortAuthority(reg)

	if _, err := m.resolvePort(&ProjectInfo{Name: "demo", Path: dir, Port: 8100}); err == nil {
		t.Fatal("an entry with no port must not silently fall back to the stored one")
	}
}

func TestMissingRegistryIsAnErrorNotASilentFallback(t *testing.T) {
	m := quietManager(t)
	m.SetPortAuthority(filepath.Join(t.TempDir(), "absent.json"))

	if _, err := m.resolvePort(&ProjectInfo{Name: "demo", Port: 8100}); err == nil {
		t.Fatal("falling back silently is how the two sources drift apart unnoticed")
	}
}

func TestMalformedRegistryIsAnError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ports.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := quietManager(t)
	m.SetPortAuthority(p)

	if _, err := m.resolvePort(&ProjectInfo{Name: "demo", Port: 8100}); err == nil {
		t.Fatal("a malformed registry must be reported, not ignored")
	}
}

func TestNextAvailableIsNeverTreatedAsAPort(t *testing.T) {
	// A registry's allocator hint reserves the NEXT project's port. Reading it
	// as a live port hands two services the same number.
	p := filepath.Join(t.TempDir(), "ports.json")
	raw := `{"next_available": 8777, "registry": {"demo": {"port": 8100, "status": "active"}}}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	m := quietManager(t)
	m.SetPortAuthority(p)

	got, err := m.resolvePort(&ProjectInfo{Name: "demo", Port: 9999})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 8100 {
		t.Fatalf("got %d, want 8100 - never the allocator hint", got)
	}
}

func TestRegistryPortIsUsedEvenWhenBusy(t *testing.T) {
	// Moving to a free port would make anything that attributes a service by
	// port label the wrong app. Resolution must not consider availability.
	dir := t.TempDir()
	reg := writePortRegistry(t, map[string]portEntry{
		"demo": {Port: 8100, Path: dir, Status: "active"},
	})
	m := quietManager(t)
	m.SetPortAuthority(reg)

	got, err := m.resolvePort(&ProjectInfo{Name: "demo", Path: dir, Port: 8100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 8100 {
		t.Fatalf("got %d, want 8100 regardless of whether it is free", got)
	}
}

func TestSamePathIgnoresTrailingSeparatorsAndCase(t *testing.T) {
	if !samePath(`C:\p\demo`, `C:\p\demo\`) {
		t.Error("a trailing separator is the same directory")
	}
	// Case-insensitivity applies on Windows only; on other platforms these
	// differ, which is also correct.
	got := samePath(`C:\P\Demo`, `C:\p\demo`)
	if want := runtime.GOOS == "windows"; got != want {
		t.Errorf("case-insensitive match = %v, want %v on this platform", got, want)
	}
	if samePath("", `C:\p\demo`) {
		t.Error("an empty path must never match")
	}
}
