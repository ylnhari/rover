package launcher

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHelperHTTPServer is not a real test: it is re-executed as a child
// process by tests that need a genuine HTTP server for the validation probe.
// It listens on 127.0.0.1:$PORT until killed.
func TestHelperHTTPServer(t *testing.T) {
	if os.Getenv("ROVER_TEST_HELPER") != "1" {
		t.Skip("helper process only")
	}
	port := os.Getenv("PORT")
	if port == "" {
		t.Fatal("PORT not set")
	}
	srv := &http.Server{
		Addr: "127.0.0.1:" + port,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "helper ok")
		}),
	}
	srv.ListenAndServe()
}

// helperServerCmd returns a start command that re-executes this test binary as
// a real HTTP server (see TestHelperHTTPServer). Tests using it must call
// t.Setenv("ROVER_TEST_HELPER", "1") so the child skips the guard. The {port}
// placeholder is absorbed by a harmless -test.skip regex so composeStartCmd
// doesn't append a --port flag the test binary would reject; the helper reads
// the PORT env var instead.
func helperServerCmd() string {
	return os.Args[0] + " -test.run=^TestHelperHTTPServer$ -test.skip=p{port}"
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func TestDetectStartCmd(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"server.py", "python server.py"},
		{"run.sh", "bash run.sh"},
		{"start.bat", "start.bat"},
		{"deploy.ps1", "pwsh -File deploy.ps1"},
		{"index.js", "node index.js"},
		{"app.ts", "npx tsx app.ts"},
		{"main.go", "go run main.go"},
		{"script.rb", "ruby script.rb"},
		{"index.php", "php index.php"},
		{"script.pl", "perl script.pl"},
		{"script.lua", "lua script.lua"},
		{"unknown.xyz", "unknown.xyz"},
	}
	for _, tc := range tests {
		got := detectStartCmd(tc.path)
		if got != tc.want {
			t.Errorf("detectStartCmd(%q) = %q; want %q", tc.path, got, tc.want)
		}
	}
}

func TestNewManager(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	if m == nil {
		t.Fatal("NewManager returned nil")
	}
	if m.ProjectsRoot() != dir {
		t.Errorf("ProjectsRoot() = %q; want %q", m.ProjectsRoot(), dir)
	}
}

func TestScanEmpty(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	projects := m.Scan()
	if len(projects) != 0 {
		t.Errorf("Scan() = %d projects; want 0", len(projects))
	}
}

func TestScanWithProjects(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	p1 := ProjectInfo{Name: "beta", Path: filepath.Join(dir, "beta"), Port: 9001, StartCmd: "python beta.py", URL: "http://127.0.0.1:9001"}
	p2 := ProjectInfo{Name: "alpha", Path: filepath.Join(dir, "alpha"), Port: 9002, StartCmd: "python alpha.py", URL: "http://127.0.0.1:9002"}

	reg := roverRegistry{Projects: map[string]ProjectInfo{p1.Name: p1, p2.Name: p2}}
	if err := saveRoverRegistry(m.registryPath, reg); err != nil {
		t.Fatal(err)
	}

	projects := m.Scan()
	if len(projects) != 2 {
		t.Fatalf("Scan() = %d projects; want 2", len(projects))
	}
	if projects[0].Name != "alpha" || projects[1].Name != "beta" {
		t.Errorf("expected sorted alpha,beta; got %q, %q", projects[0].Name, projects[1].Name)
	}
}

func TestListProjectDirs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "myapp"), 0755)
	os.MkdirAll(filepath.Join(dir, "myapi"), 0755)
	os.MkdirAll(filepath.Join(dir, ".hidden"), 0755)
	os.MkdirAll(filepath.Join(dir, ".venv"), 0755)
	os.MkdirAll(filepath.Join(dir, "archive"), 0755)
	os.MkdirAll(filepath.Join(dir, "rover"), 0755)

	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	reg := roverRegistry{Projects: map[string]ProjectInfo{"myapi": {Name: "myapi"}}}
	if err := saveRoverRegistry(m.registryPath, reg); err != nil {
		t.Fatal(err)
	}

	dirs := m.ListProjectDirs()
	if len(dirs) != 1 || dirs[0] != "myapp" {
		t.Errorf("ListProjectDirs() = %v; want [myapp]", dirs)
	}
}

func TestListEligibleFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "testproj"), 0755)
	os.WriteFile(filepath.Join(dir, "testproj", "server.py"), []byte("print('hi')"), 0644)
	os.WriteFile(filepath.Join(dir, "testproj", "start.sh"), []byte("echo hi"), 0644)
	os.WriteFile(filepath.Join(dir, "testproj", ".gitignore"), []byte("*.pyc"), 0644)
	os.MkdirAll(filepath.Join(dir, "testproj", "node_modules"), 0755)
	os.WriteFile(filepath.Join(dir, "testproj", "node_modules", "index.js"), []byte(""), 0644)
	os.MkdirAll(filepath.Join(dir, "testproj", "__pycache__"), 0755)
	os.WriteFile(filepath.Join(dir, "testproj", "__pycache__", "cache.py"), []byte(""), 0644)

	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	files := m.ListEligibleFiles("testproj")
	if len(files) != 2 {
		t.Fatalf("ListEligibleFiles() = %d files; want 2 (got: %v)", len(files), fileNames(files))
	}
	if files[0].Path != "server.py" || files[1].Path != "start.sh" {
		t.Errorf("expected server.py, start.sh; got %q, %q", files[0].Path, files[1].Path)
	}
}

func fileNames(files []FileInfo) []string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Path
	}
	return names
}

func TestListEligibleFilesNonexistent(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	files := m.ListEligibleFiles("doesnotexist")
	if files != nil {
		t.Errorf("expected nil, got %v", files)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")

	orig := roverRegistry{Projects: map[string]ProjectInfo{
		"app": {Name: "app", Path: dir, Port: 1234, StartCmd: "python app.py", URL: "http://127.0.0.1:1234", Description: "Active", ProxyPort: 45678},
	}}
	if err := saveRoverRegistry(path, orig); err != nil {
		t.Fatal(err)
	}

	loaded := loadRoverRegistry(path)
	if len(loaded.Projects) != 1 {
		t.Fatalf("loaded %d projects; want 1", len(loaded.Projects))
	}
	p := loaded.Projects["app"]
	if p.Port != 1234 || p.StartCmd != "python app.py" || p.ProxyPort != 45678 {
		t.Errorf("unexpected project data: %+v", p)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind after atomic save")
	}
}

func TestSaveRegistryEmptyPath(t *testing.T) {
	err := saveRoverRegistry("", roverRegistry{Projects: make(map[string]ProjectInfo)})
	if err == nil || !strings.Contains(err.Error(), "registry path not set") {
		t.Errorf("expected 'registry path not set' error, got %v", err)
	}
}

func TestLoadRegistryNonexistent(t *testing.T) {
	reg := loadRoverRegistry("/nonexistent/path/registry.json")
	if reg.Projects == nil {
		t.Error("expected non-nil Projects map")
	}
}

func TestAddProject(t *testing.T) {
	t.Setenv("ROVER_TEST_HELPER", "1")
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	proj, _, err := m.AddProject("nonexistent", "nonexistent_cmd", 9999)
	if err == nil {
		t.Fatal("expected error for nonexistent project directory")
	}
	if proj != nil {
		t.Error("expected nil project on error")
	}

	appDir := filepath.Join(dir, "testapp")
	os.MkdirAll(appDir, 0755)
	port := freeTCPPort(t)

	proj, report, err := m.AddProject("testapp", helperServerCmd(), port)
	if err != nil {
		t.Fatalf("AddProject failed: %v", err)
	}
	if proj == nil {
		t.Fatal("AddProject returned nil")
	}
	if proj.Port != port {
		t.Errorf("expected port %d, got %d", port, proj.Port)
	}
	want := fmt.Sprintf("127.0.0.1:%d", port)
	if !strings.Contains(proj.URL, want) {
		t.Errorf("expected URL containing %s, got %q", want, proj.URL)
	}
	if report == nil || !report.Probe.Listening {
		t.Errorf("expected a listening probe report, got %+v", report)
	}
	if !report.Probe.HTTP || report.Probe.Status == 0 {
		t.Errorf("expected HTTP-classified probe, got %+v", report.Probe)
	}
}

func TestAddProjectInvalidName(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	for _, name := range []string{"../evil", "a/b", `a\b`, ".hidden", ""} {
		if _, _, err := m.AddProject(name, "python app.py", 8123); err == nil {
			t.Errorf("expected name validation error for %q", name)
		}
	}
}

func TestValidateProjectExitsEarly(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.SetProbeTimeout(10 * time.Second)

	report, err := m.ValidateProject(dir, "exit 3", freeTCPPort(t))
	if err == nil {
		t.Fatal("expected error for command that exits before listening")
	}
	if report == nil || report.ExitCode == nil || *report.ExitCode != 3 {
		t.Errorf("expected exit code 3 in report, got %+v", report)
	}
	if !strings.Contains(err.Error(), "exited with code 3") {
		t.Errorf("error should carry the exit code: %v", err)
	}
}

func TestValidateProjectTimeout(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.SetProbeTimeout(2 * time.Second)

	sleepCmd := "sleep 30"
	if isWindows() {
		sleepCmd = "ping -n 30 127.0.0.1 >nul"
	}
	report, err := m.ValidateProject(dir, sleepCmd, freeTCPPort(t))
	if err == nil {
		t.Fatal("expected timeout error for command that never listens")
	}
	if report.Probe.Listening {
		t.Error("probe should not report listening")
	}
	if !strings.Contains(err.Error(), "nothing listening") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateProjectOccupiedPort(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	_, err = m.ValidateProject(dir, "echo hi", port)
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Errorf("expected occupied-port refusal, got %v", err)
	}
}

func TestRemoveProject(t *testing.T) {
	t.Setenv("ROVER_TEST_HELPER", "1")
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	appDir := filepath.Join(dir, "testapp")
	os.MkdirAll(appDir, 0755)

	if _, _, err := m.AddProject("testapp", helperServerCmd(), freeTCPPort(t)); err != nil {
		t.Fatalf("AddProject: %v", err)
	}

	err := m.RemoveProject("testapp")
	if err != nil {
		t.Fatalf("RemoveProject failed: %v", err)
	}

	projects := m.Scan()
	if len(projects) != 0 {
		t.Errorf("expected 0 projects after removal, got %d", len(projects))
	}
}

func TestRemoveNonexistent(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	err := m.RemoveProject("doesnotexist")
	if err == nil {
		t.Error("expected error removing nonexistent project")
	}
}

func TestStartStopProject(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")
	m.SetProbeTimeout(2 * time.Second)

	appDir := filepath.Join(dir, "echoserver")
	os.MkdirAll(appDir, 0755)

	var startCmd string
	if isWindows() {
		startCmd = `cmd /C echo server started {port} && ping -n 30 127.0.0.1 >nul`
	} else {
		startCmd = `sh -c 'echo server started {port} && sleep 30'`
	}

	freePort := freeTCPPort(t)

	reg := roverRegistry{Projects: map[string]ProjectInfo{
		"echoserver": {Name: "echoserver", Path: appDir, StartCmd: startCmd, Port: freePort},
	}}
	if err := saveRoverRegistry(m.registryPath, reg); err != nil {
		t.Fatal(err)
	}

	if err := m.Start("echoserver", StartOptions{}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	rp := m.GetRunning("echoserver")
	if rp == nil {
		t.Fatal("GetRunning returned nil")
	}
	if rp.Name != "echoserver" {
		t.Errorf("want echoserver, got %q", rp.Name)
	}
	if rp.State != StateStarting {
		t.Errorf("want state starting before listener confirmation, got %q", rp.State)
	}
	if rp.URL != "" {
		t.Errorf("URL must not be advertised before the listener is confirmed, got %q", rp.URL)
	}

	list := m.ListRunning()
	if len(list) != 1 {
		t.Errorf("ListRunning() = %d; want 1", len(list))
	}

	if err := m.Stop("echoserver"); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	rp = m.GetRunning("echoserver")
	if rp != nil {
		t.Error("GetRunning should return nil after stop")
	}
	ex := m.LastExit("echoserver")
	if ex == nil || !ex.Stopped {
		t.Errorf("expected stopped exit status, got %+v", ex)
	}
}

func TestStartConfirmsReadyAndProxies(t *testing.T) {
	t.Setenv("ROVER_TEST_HELPER", "1")
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")
	m.SetBindHost("127.0.0.1")
	m.SetProbeTimeout(15 * time.Second)

	appDir := filepath.Join(dir, "webapp")
	os.MkdirAll(appDir, 0755)
	port := freeTCPPort(t)

	reg := roverRegistry{Projects: map[string]ProjectInfo{
		"webapp": {Name: "webapp", Path: appDir, StartCmd: helperServerCmd(), Port: port, ProxyEnabled: true},
	}}
	if err := saveRoverRegistry(m.registryPath, reg); err != nil {
		t.Fatal(err)
	}

	if err := m.Start("webapp", StartOptions{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop("webapp")

	var rp *RunningProject
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		rp = m.GetRunning("webapp")
		if rp != nil && rp.State == StateRunning && rp.ProxyPort > 0 {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if rp == nil || rp.State != StateRunning {
		t.Fatalf("project never reached running state: %+v", rp)
	}
	wantURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	if rp.URL != wantURL {
		t.Errorf("URL = %q; want %q", rp.URL, wantURL)
	}
	if rp.ProxyPort <= 0 {
		t.Fatalf("proxy did not start: %+v", rp)
	}

	// The proxy must actually forward to the app.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", rp.ProxyPort))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("proxy status = %d; want 200", resp.StatusCode)
	}

	// The allocated proxy port is persisted for stable bookmarks.
	saved := loadRoverRegistry(m.registryPath).Projects["webapp"]
	if saved.ProxyPort != rp.ProxyPort {
		t.Errorf("proxy port not persisted: registry %d, running %d", saved.ProxyPort, rp.ProxyPort)
	}

	firstProxyPort := rp.ProxyPort
	if err := m.Stop("webapp"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := m.Start("webapp", StartOptions{}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		rp = m.GetRunning("webapp")
		if rp != nil && rp.ProxyPort > 0 {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if rp == nil || rp.ProxyPort != firstProxyPort {
		t.Errorf("proxy port not stable across restarts: first %d, second %+v", firstProxyPort, rp)
	}
}

func TestReapCrashedProject(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")
	m.SetProbeTimeout(2 * time.Second)

	appDir := filepath.Join(dir, "crasher")
	os.MkdirAll(appDir, 0755)

	reg := roverRegistry{Projects: map[string]ProjectInfo{
		"crasher": {Name: "crasher", Path: appDir, StartCmd: "exit 3"},
	}}
	saveRoverRegistry(m.registryPath, reg)

	if err := m.Start("crasher", StartOptions{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m.GetRunning("crasher") == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if m.GetRunning("crasher") != nil {
		t.Fatal("crashed project still reported as running")
	}
	ex := m.LastExit("crasher")
	if ex == nil {
		t.Fatal("no exit status recorded for crashed project")
	}
	if ex.Code != 3 {
		t.Errorf("exit code = %d; want 3", ex.Code)
	}
	if ex.Stopped {
		t.Error("crash must not be recorded as a deliberate stop")
	}
}

func TestAdoptAndDetach(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	port := freeTCPPort(t)
	srv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "external") }),
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	appDir := filepath.Join(dir, "external")
	os.MkdirAll(appDir, 0755)
	reg := roverRegistry{Projects: map[string]ProjectInfo{
		"external": {Name: "external", Path: appDir, StartCmd: "irrelevant", Port: port},
	}}
	saveRoverRegistry(m.registryPath, reg)

	info, err := m.Adopt("external")
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if info.State != StateAdopted {
		t.Errorf("state = %q; want %q", info.State, StateAdopted)
	}
	if m.GetRunning("external") == nil {
		t.Fatal("adopted project not tracked as running")
	}

	if err := m.Stop("external"); err != nil {
		t.Fatalf("Stop (detach): %v", err)
	}
	if m.GetRunning("external") != nil {
		t.Error("still tracked after detach")
	}
	// Detaching must NOT kill the process rover doesn't own.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("external server was killed by detach: %v", err)
	}
	resp.Body.Close()
}

func TestAdoptNothingListening(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	appDir := filepath.Join(dir, "ghost")
	os.MkdirAll(appDir, 0755)
	reg := roverRegistry{Projects: map[string]ProjectInfo{
		"ghost": {Name: "ghost", Path: appDir, StartCmd: "irrelevant", Port: freeTCPPort(t)},
	}}
	saveRoverRegistry(m.registryPath, reg)

	if _, err := m.Adopt("ghost"); err == nil {
		t.Error("expected error adopting a port with no listener")
	}
}

func TestStartAlreadyRunning(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	appDir := filepath.Join(dir, "echo")
	os.MkdirAll(appDir, 0755)

	var startCmd string
	if isWindows() {
		startCmd = `cmd /C ping -n 30 127.0.0.1 >nul`
	} else {
		startCmd = `sh -c 'sleep 30'`
	}

	reg := roverRegistry{Projects: map[string]ProjectInfo{
		"echo": {Name: "echo", Path: appDir, StartCmd: startCmd},
	}}
	saveRoverRegistry(m.registryPath, reg)

	if err := m.Start("echo", StartOptions{}); err != nil {
		t.Fatal(err)
	}
	defer m.Stop("echo")

	err := m.Start("echo", StartOptions{})
	if err == nil {
		t.Error("expected error starting already running project")
	}
}

func TestStartNonexistent(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	err := m.Start("doesnotexist", StartOptions{})
	if err == nil {
		t.Error("expected error for nonexistent project")
	}
}

func TestStopNotRunning(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	err := m.Stop("doesnotexist")
	if err == nil {
		t.Error("expected error stopping not-running project")
	}
}

func TestStopAll(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("proj%d", i)
		appDir := filepath.Join(dir, name)
		os.MkdirAll(appDir, 0755)

		var startCmd string
		if isWindows() {
			startCmd = `cmd /C ping -n 30 127.0.0.1 >nul`
		} else {
			startCmd = `sh -c 'sleep 30'`
		}

		reg := roverRegistry{Projects: map[string]ProjectInfo{
			name: {Name: name, Path: appDir, StartCmd: startCmd},
		}}
		saveRoverRegistry(m.registryPath, reg)

		if err := m.Start(name, StartOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	if len(m.ListRunning()) != 3 {
		t.Errorf("expected 3 running, got %d", len(m.ListRunning()))
	}

	m.StopAll()
	if len(m.ListRunning()) != 0 {
		t.Errorf("expected 0 running after StopAll, got %d", len(m.ListRunning()))
	}
}

func TestSubscribeUnsubscribe(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	appDir := filepath.Join(dir, "streamapp")
	os.MkdirAll(appDir, 0755)

	var startCmd string
	if isWindows() {
		startCmd = `cmd /C echo hello && ping -n 30 127.0.0.1 >nul`
	} else {
		startCmd = `sh -c 'echo hello && sleep 30'`
	}

	reg := roverRegistry{Projects: map[string]ProjectInfo{
		"streamapp": {Name: "streamapp", Path: appDir, StartCmd: startCmd},
	}}
	saveRoverRegistry(m.registryPath, reg)

	m.Start("streamapp", StartOptions{})
	defer m.Stop("streamapp")

	ch, err := m.Subscribe("streamapp")
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.Type != "stdout" && ev.Type != "stderr" && ev.Type != "ready" {
			t.Errorf("expected stdout/stderr/ready event, got %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stream event")
	}

	m.Unsubscribe("streamapp", ch)

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed after unsubscribe")
		}
	default:
	}
}

func TestSubscribeNotRunning(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	_, err := m.Subscribe("doesnotexist")
	if err == nil {
		t.Error("expected error subscribing to not-running project")
	}
}

func TestKillProcessNil(t *testing.T) {
	if err := killProcess(nil); err != nil {
		t.Errorf("killProcess(nil) = %v; want nil", err)
	}
}

func TestSetLogger(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	var logged string
	m.SetLogger(func(format string, args ...any) {
		logged = fmt.Sprintf(format, args...)
	})
	m.logf("test %s", "message")
	if logged != "test message" {
		t.Errorf("expected 'test message', got %q", logged)
	}
}

func wantURL(host string, port int) string {
	ip := localIP()
	if ip == "localhost" {
		return fmt.Sprintf("http://%s:%d", host, port)
	}
	return fmt.Sprintf("http://%s:%d", ip, port)
}

func TestCaptureOutputURLDetection(t *testing.T) {
	rp := &runningProcess{
		info: RunningProject{Name: "test"},
		subs: nil,
		done: make(chan struct{}),
	}

	stdout := strings.NewReader("some output\nServer running at http://127.0.0.1:8888\nmore output\n")
	stderr := strings.NewReader("")

	rp.captureOutput(stdout, stderr)

	expected := wantURL("127.0.0.1", 8888)
	if rp.info.URL != expected {
		t.Errorf("expected URL %s, got %q", expected, rp.info.URL)
	}
	if rp.info.Port != 8888 {
		t.Errorf("expected port 8888, got %d", rp.info.Port)
	}

	output := rp.output.String()
	if !strings.Contains(output, "Server running at http://127.0.0.1:8888") {
		t.Errorf("expected output to contain the server line, got %q", output)
	}
}

func TestCaptureOutputURLFromStderr(t *testing.T) {
	rp := &runningProcess{
		info: RunningProject{Name: "test"},
		subs: nil,
		done: make(chan struct{}),
	}

	stdout := strings.NewReader("booting up\n")
	stderr := strings.NewReader("ERROR: something\nURL: http://127.0.0.1:9000\n")

	rp.captureOutput(stdout, stderr)

	expected := wantURL("127.0.0.1", 9000)
	if rp.info.URL != expected {
		t.Errorf("expected URL %s, got %q", expected, rp.info.URL)
	}
	if rp.info.Port != 9000 {
		t.Errorf("expected port 9000, got %d", rp.info.Port)
	}
}

func TestCaptureOutputDoesNotOverwriteConfirmedURL(t *testing.T) {
	// The readiness probe's URL is authoritative; a URL that merely appears in
	// a log line must not replace it.
	rp := &runningProcess{
		info: RunningProject{Name: "test", URL: "http://127.0.0.1:7000", Port: 7000},
		done: make(chan struct{}),
	}

	stdout := strings.NewReader("output with http://127.0.0.1:9999\n")
	stderr := strings.NewReader("")

	rp.captureOutput(stdout, stderr)

	if rp.info.URL != "http://127.0.0.1:7000" {
		t.Errorf("URL was overwritten by a log line: %q", rp.info.URL)
	}
	if rp.info.Port != 7000 {
		t.Errorf("port was overwritten by a log line: %d", rp.info.Port)
	}
}

func TestConsoleBufferBounded(t *testing.T) {
	rp := &runningProcess{info: RunningProject{Name: "chatty"}, done: make(chan struct{})}
	line := strings.Repeat("x", 1024) + "\n"
	rp.outputMu.Lock()
	for i := 0; i < 2*maxConsoleBuffer/len(line); i++ {
		rp.appendOutput(line)
	}
	size := rp.output.Len()
	head := rp.output.String()[:64]
	rp.outputMu.Unlock()
	if size > maxConsoleBuffer {
		t.Errorf("console buffer grew to %d, cap is %d", size, maxConsoleBuffer)
	}
	if !strings.Contains(head, "truncated") {
		t.Errorf("expected truncation marker at buffer head, got %q", head)
	}
}

func TestSubscribeWithExistingOutput(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	appDir := filepath.Join(dir, "preload")
	os.MkdirAll(appDir, 0755)

	var startCmd string
	if isWindows() {
		startCmd = `cmd /C echo preloaded && ping -n 30 127.0.0.1 >nul`
	} else {
		startCmd = `sh -c 'echo preloaded && sleep 30'`
	}

	reg := roverRegistry{Projects: map[string]ProjectInfo{
		"preload": {Name: "preload", Path: appDir, StartCmd: startCmd},
	}}
	saveRoverRegistry(m.registryPath, reg)

	m.Start("preload", StartOptions{})
	defer m.Stop("preload")

	time.Sleep(200 * time.Millisecond)

	ch, err := m.Subscribe("preload")
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.Type != "stdout" {
			t.Errorf("expected stdout, got %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for existing output")
	}
}

func TestRegistryPath(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "custom_registry.json")

	if m.RegistryPath() != filepath.Join(dir, "custom_registry.json") {
		t.Errorf("unexpected registry path: %q", m.RegistryPath())
	}
}

func TestProjectsRoot(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	if m.ProjectsRoot() != dir {
		t.Errorf("unexpected projects root: %q", m.ProjectsRoot())
	}
}

func isWindows() bool {
	return os.PathSeparator == '\\'
}

func TestComposeStartCmd(t *testing.T) {
	cases := []struct {
		cmd  string
		port int
		want string
	}{
		{"python server.py", 8765, "python server.py --port 8765"},
		{"uv run python s.py --port {port}", 8080, "uv run python s.py --port 8080"},
		{"app --port 9000", 8080, "app --port 9000"}, // explicit --port left alone
		{"app --port=9000", 8080, "app --port=9000"}, // = form recognized too
		{"python s.py", 0, "python s.py"},            // no port -> unchanged
		// substring must not suppress injection (audit §1.4)
		{"app --portfolio-mode", 8080, "app --portfolio-mode --port 8080"},
	}
	for _, c := range cases {
		if got := composeStartCmd(c.cmd, c.port); got != c.want {
			t.Errorf("composeStartCmd(%q,%d)=%q; want %q", c.cmd, c.port, got, c.want)
		}
	}
}

func TestStartPortInUse(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")
	appDir := filepath.Join(dir, "busy")
	os.MkdirAll(appDir, 0755)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	startCmd := `sh -c 'echo {port}'`
	if isWindows() {
		startCmd = `cmd /C echo {port}`
	}
	reg := roverRegistry{Projects: map[string]ProjectInfo{
		"busy": {Name: "busy", Path: appDir, StartCmd: startCmd, Port: port},
	}}
	saveRoverRegistry(m.registryPath, reg)

	err = m.Start("busy", StartOptions{})
	if !errors.Is(err, ErrPortInUse) {
		t.Fatalf("expected ErrPortInUse, got %v", err)
	}
	var conflict *PortConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *PortConflictError, got %T", err)
	}
	if conflict.Port != port {
		t.Errorf("conflict port = %d; want %d", conflict.Port, port)
	}
	// The listener is this test process, so the occupant should be identified
	// as us. Tolerate nil on exotic environments without netstat/lsof.
	if conflict.Occupant != nil && conflict.Occupant.PID != os.Getpid() {
		t.Errorf("occupant pid = %d; want %d (this test)", conflict.Occupant.PID, os.Getpid())
	}
}

func TestStartKillOccupantRequiresMatchingPID(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")
	appDir := filepath.Join(dir, "busy2")
	os.MkdirAll(appDir, 0755)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	reg := roverRegistry{Projects: map[string]ProjectInfo{
		"busy2": {Name: "busy2", Path: appDir, StartCmd: "echo hi", Port: port},
	}}
	saveRoverRegistry(m.registryPath, reg)

	// A stale/wrong confirm_pid must NOT kill anything and must re-report the
	// conflict. (Also guards the rover-self case: the occupant here is this
	// process, and KillConfirmedListener refuses to kill self.)
	err = m.Start("busy2", StartOptions{KillOccupant: true, ConfirmPID: 1})
	if !errors.Is(err, ErrPortInUse) {
		t.Fatalf("expected conflict, got %v", err)
	}
	// Listener must still be alive.
	if portAvailable(port) {
		t.Fatal("occupant was killed despite mismatched confirm_pid")
	}
}

func TestFindListenerOnPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	occ := FindListenerOnPort(port)
	if occ == nil {
		t.Skip("listener lookup unavailable on this system")
	}
	if occ.PID != os.Getpid() {
		t.Errorf("occupant pid = %d; want %d", occ.PID, os.Getpid())
	}
}

func TestUpdateProjectPort(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")
	reg := roverRegistry{Projects: map[string]ProjectInfo{
		"app": {Name: "app", Path: dir, StartCmd: "python s.py", Port: 8000},
	}}
	saveRoverRegistry(m.registryPath, reg)

	p, err := m.UpdateProjectPort("app", 8123)
	if err != nil {
		t.Fatalf("UpdateProjectPort: %v", err)
	}
	if p.Port != 8123 {
		t.Errorf("returned port = %d; want 8123", p.Port)
	}
	if got := m.Scan(); len(got) != 1 || got[0].Port != 8123 {
		t.Errorf("port not persisted: %+v", got)
	}
	if _, err := m.UpdateProjectPort("nope", 9000); err == nil {
		t.Error("expected error updating unknown project")
	}
}

func TestProxyURLHost(t *testing.T) {
	// A specific routable bind host is advertised verbatim (what a remote
	// client should hit). Loopback / all-interfaces binds fall back to a
	// routable IP, which is environment-dependent, so only assert they are
	// never the unusable 0.0.0.0 / empty / loopback literal.
	if got := proxyURLHost("100.100.20.30"); got != "100.100.20.30" {
		t.Errorf("proxyURLHost(tailnet) = %q; want verbatim", got)
	}
	if got := proxyURLHost("192.168.1.10"); got != "192.168.1.10" {
		t.Errorf("proxyURLHost(lan) = %q; want verbatim", got)
	}
	for _, in := range []string{"", "0.0.0.0", "::", "127.0.0.1"} {
		got := proxyURLHost(in)
		if got == "" || got == "0.0.0.0" || got == "127.0.0.1" || got == "::" {
			t.Errorf("proxyURLHost(%q) = %q; want a routable fallback", in, got)
		}
	}
}
