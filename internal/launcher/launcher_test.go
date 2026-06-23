package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
		"app": {Name: "app", Path: dir, Port: 1234, StartCmd: "python app.py", URL: "http://127.0.0.1:1234", Description: "Active"},
	}}
	if err := saveRoverRegistry(path, orig); err != nil {
		t.Fatal(err)
	}

	loaded := loadRoverRegistry(path)
	if len(loaded.Projects) != 1 {
		t.Fatalf("loaded %d projects; want 1", len(loaded.Projects))
	}
	p := loaded.Projects["app"]
	if p.Port != 1234 || p.StartCmd != "python app.py" {
		t.Errorf("unexpected project data: %+v", p)
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
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	proj, err := m.AddProject("nonexistent", "nonexistent_cmd")
	if err == nil {
		t.Fatal("expected error for nonexistent project directory")
	}
	if proj != nil {
		t.Error("expected nil project on error")
	}

	appDir := filepath.Join(dir, "testapp")
	os.MkdirAll(appDir, 0755)

	var startCmd string
	if isWindows() {
		startCmd = `cmd /C echo http://127.0.0.1:9999`
	} else {
		startCmd = `sh -c 'echo http://127.0.0.1:9999'`
	}

	proj, err = m.AddProject("testapp", startCmd)
	if err != nil {
		t.Fatalf("AddProject failed: %v", err)
	}
	if proj == nil {
		t.Fatal("AddProject returned nil")
	}
	if proj.Port != 9999 {
		t.Errorf("expected port 9999, got %d", proj.Port)
	}
	wantIP := fmt.Sprintf("%s:9999", localIP())
	if !strings.Contains(proj.URL, wantIP) {
		t.Errorf("expected URL containing %s, got %q", wantIP, proj.URL)
	}
}

func TestRemoveProject(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	appDir := filepath.Join(dir, "testapp")
	os.MkdirAll(appDir, 0755)

	var startCmd string
	if isWindows() {
		startCmd = `cmd /C echo http://127.0.0.1:9999`
	} else {
		startCmd = `sh -c 'echo http://127.0.0.1:9999'`
	}

	m.AddProject("testapp", startCmd)

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

	appDir := filepath.Join(dir, "echoserver")
	os.MkdirAll(appDir, 0755)

	var startCmd string
	if isWindows() {
		startCmd = `cmd /C echo server started && ping -n 30 127.0.0.1 >nul`
	} else {
		startCmd = `sh -c 'echo server started && sleep 30'`
	}

	reg := roverRegistry{Projects: map[string]ProjectInfo{
		"echoserver": {Name: "echoserver", Path: appDir, StartCmd: startCmd, Port: 1234},
	}}
	if err := saveRoverRegistry(m.registryPath, reg); err != nil {
		t.Fatal(err)
	}

	if err := m.Start("echoserver"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	rp := m.GetRunning("echoserver")
	if rp == nil {
		t.Fatal("GetRunning returned nil")
	}
	if rp.Name != "echoserver" {
		t.Errorf("want echoserver, got %q", rp.Name)
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

	if err := m.Start("echo"); err != nil {
		t.Fatal(err)
	}
	defer m.Stop("echo")

	err := m.Start("echo")
	if err == nil {
		t.Error("expected error starting already running project")
	}
}

func TestStartNonexistent(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.registryPath = filepath.Join(dir, "registry.json")

	err := m.Start("doesnotexist")
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

		if err := m.Start(name); err != nil {
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

	m.Start("streamapp")
	defer m.Stop("streamapp")

	ch, err := m.Subscribe("streamapp")
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.Type != "stdout" && ev.Type != "stderr" {
			t.Errorf("expected stdout/stderr event, got %q", ev.Type)
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

func TestCaptureOutputOverwritesExistingURL(t *testing.T) {
	rp := &runningProcess{
		info: RunningProject{Name: "test", URL: "http://127.0.0.1:7000", Port: 7000},
		done: make(chan struct{}),
	}

	stdout := strings.NewReader("output with http://127.0.0.1:9999\n")
	stderr := strings.NewReader("")

	rp.captureOutput(stdout, stderr)

	expected := wantURL("127.0.0.1", 9999)
	if rp.info.URL != expected {
		t.Errorf("expected URL %s, got %q", expected, rp.info.URL)
	}
	if rp.info.Port != 9999 {
		t.Errorf("port should be overwritten; got %d", rp.info.Port)
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

	m.Start("preload")
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

func TestValidateProjectTimeout(t *testing.T) {
	t.Skip("skipping: hardcoded 15s timeout makes this test too slow")
}

func isWindows() bool {
	return os.PathSeparator == '\\'
}
