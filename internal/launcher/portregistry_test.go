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

// The registry decides the DEFAULT port. It must never become a lock that
// leaves an operator with no way to start a project at all — so Start consults
// an explicit override before it reads the registry. These cover the three
// cases where resolvePort refuses outright.

// registerDemo puts a real project in rover's own registry, so Start gets past
// its "project not found" guard and actually reaches the port logic. Without
// this these tests would pass on the wrong error and prove nothing.
func registerDemo(t *testing.T, m *Manager, path string) {
	t.Helper()
	m.registryPath = filepath.Join(t.TempDir(), "registry.json")
	proj := ProjectInfo{
		Name: "demo", Path: path, Port: 8100,
		// A command that exits immediately: these tests care about which port
		// Start selects, not about running anything.
		StartCmd: "cmd /c exit 0",
	}
	reg := roverRegistry{Projects: map[string]ProjectInfo{"demo": proj}}
	if err := saveRoverRegistry(m.registryPath, reg); err != nil {
		t.Fatal(err)
	}
	if m.GetProject("demo") == nil {
		t.Fatal("setup failed: demo not registered")
	}
	t.Cleanup(func() { m.StopAll() })
}

// registryRefused reports whether the error came from the port registry as
// opposed to anything else Start might legitimately complain about.
func registryRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "port registry") ||
		strings.Contains(msg, "is retired") ||
		strings.Contains(msg, "has no port")
}

func TestExplicitPortStartsARetiredProject(t *testing.T) {
	dir := t.TempDir()
	reg := writePortRegistry(t, map[string]portEntry{
		"demo": {Path: dir, Status: "retired", Note: "moved elsewhere"},
	})
	m := quietManager(t)
	m.SetPortAuthority(reg)
	registerDemo(t, m, dir)

	// Without an override the registry refuses, which is the point of retiring.
	if err := m.Start("demo", StartOptions{}); !registryRefused(err) {
		t.Fatalf("a retired entry should refuse by default; got %v", err)
	}
	// With one, Start must never consult the registry at all.
	if err := m.Start("demo", StartOptions{PortOverride: 8123}); registryRefused(err) {
		t.Fatalf("an explicit port must bypass the registry; got %v", err)
	}
}

func TestExplicitPortSurvivesAMalformedRegistry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ports.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := quietManager(t)
	m.SetPortAuthority(p)
	registerDemo(t, m, t.TempDir())

	if err := m.Start("demo", StartOptions{}); !registryRefused(err) {
		t.Fatalf("a malformed registry should refuse by default; got %v", err)
	}
	if err := m.Start("demo", StartOptions{PortOverride: 8124}); registryRefused(err) {
		t.Fatalf("an explicit port must not require a readable registry; got %v", err)
	}
}

func TestExplicitPortWorksWhenTheEntryHasNoPort(t *testing.T) {
	dir := t.TempDir()
	reg := writePortRegistry(t, map[string]portEntry{
		"demo": {Path: dir, Status: "active", Note: "port deliberately unassigned"},
	})
	m := quietManager(t)
	m.SetPortAuthority(reg)
	registerDemo(t, m, dir)

	if err := m.Start("demo", StartOptions{}); !registryRefused(err) {
		t.Fatalf("an entry with no port should refuse by default; got %v", err)
	}
	if err := m.Start("demo", StartOptions{PortOverride: 8125}); registryRefused(err) {
		t.Fatalf("an explicit port supplies what the entry omits; got %v", err)
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
