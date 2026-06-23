package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ylnhari/rover/internal/server"
)

const testSecret = "test-secret-123"

func newHandler(t *testing.T) http.Handler {
	t.Helper()
	return server.New(server.Config{Secret: testSecret}).Handler()
}

// newTestServer starts a test server and returns it plus a valid auth token.
func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	s := httptest.NewServer(newHandler(t))
	tok := loginToken(t, s.URL, testSecret)
	t.Cleanup(s.Close)
	return s, tok
}

// loginToken exchanges the raw secret for a signed token via the test server.
func loginToken(t *testing.T, baseURL, secret string) string {
	t.Helper()
	if secret == "" {
		return ""
	}
	body := fmt.Sprintf(`{"secret":%q}`, secret)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/auth", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}
	var data struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&data)
	if data.Token == "" {
		t.Fatal("empty token from login")
	}
	return data.Token
}

// postJSON sends a POST with JSON body and the rover token header.
func postJSON(t *testing.T, url, body, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Rover-Secret", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

// getJSON sends a GET with the rover token header.
func getJSON(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("X-Rover-Secret", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return resp
}

func TestPing(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/ping")
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var pong struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	json.NewDecoder(resp.Body).Decode(&pong)
	if pong.Status != "ok" {
		t.Errorf("want status=ok, got %q", pong.Status)
	}
}

func TestWebUI(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("want text/html, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Rover") {
		t.Error("expected HTML to contain 'Rover'")
	}
}

func TestWebUIWithoutProjects(t *testing.T) {
	// Without ProjectsRoot, GET /api/projects hits the catch-all handler
	// (the webUI). The important thing is projects routes don't crash.
	h := server.New(server.Config{Secret: testSecret}).Handler()
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/projects")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200 (catch-all webUI), got %d", resp.StatusCode)
	}
}

func TestAuthStatus(t *testing.T) {
	// With secret
	ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api/auth")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var status struct {
		Required bool `json:"required"`
	}
	json.NewDecoder(resp.Body).Decode(&status)
	if !status.Required {
		t.Error("expected required=true when secret is set")
	}

	// Without secret
	h := server.New(server.Config{}).Handler()
	ts2 := httptest.NewServer(h)
	defer ts2.Close()

	resp2, err := http.Get(ts2.URL + "/api/auth")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp2.Body.Close()
	json.NewDecoder(resp2.Body).Decode(&status)
	if status.Required {
		t.Error("expected required=false when secret is empty")
	}
}

func TestLogin(t *testing.T) {
	ts, _ := newTestServer(t)

	// Correct secret — should return a token
	body := `{"secret":"test-secret-123"}`
	resp, err := http.Post(ts.URL+"/api/auth", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200, got %d", resp.StatusCode)
	}
	var loginResp struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	json.NewDecoder(resp.Body).Decode(&loginResp)
	if loginResp.Token == "" {
		t.Error("want non-empty token on successful login")
	}
	if loginResp.ExpiresAt == "" {
		t.Error("want non-empty expires_at on successful login")
	}

	// Wrong secret
	body = `{"secret":"wrong"}`
	resp, err = http.Post(ts.URL+"/api/auth", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}

	// Invalid JSON
	resp, err = http.Post(ts.URL+"/api/auth", "application/json", bytes.NewBufferString(`not-json`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", resp.StatusCode)
	}
}

func TestLoginSecretLessMode(t *testing.T) {
	h := server.New(server.Config{}).Handler()
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/auth", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("want 200 in secret-less mode, got %d", resp.StatusCode)
	}
}

func TestCreateSessionNoAuth(t *testing.T) {
	ts, _ := newTestServer(t)

	// No token — should be 401
	resp := postJSON(t, ts.URL+"/api/sessions", `{"command":"echo hi"}`, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}

	// Secret-less mode should allow without secret
	h := server.New(server.Config{}).Handler()
	ts2 := httptest.NewServer(h)
	defer ts2.Close()

	resp2 := postJSON(t, ts2.URL+"/api/sessions", `{"command":"echo hi"}`, "")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Errorf("want 202 in secret-less mode, got %d", resp2.StatusCode)
	}
}

func TestCreateSessionWrongSecret(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := postJSON(t, ts.URL+"/api/sessions", `{"command":"echo hi"}`, "not-a-valid-token")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", resp.StatusCode)
	}
}

func TestCreateSessionEmptyCommand(t *testing.T) {
	ts, token := newTestServer(t)

	resp := postJSON(t, ts.URL+"/api/sessions", `{"command":""}`, token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400, got %d", resp.StatusCode)
	}
}

func TestCreateAndGetSession(t *testing.T) {
	ts, token := newTestServer(t)

	resp := postJSON(t, ts.URL+"/api/sessions", `{"command":"echo hello world"}`, token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	var created struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	if created.ID == "" {
		t.Fatal("want non-empty session ID")
	}

	time.Sleep(500 * time.Millisecond)

	resp2 := getJSON(t, ts.URL+"/api/sessions/"+created.ID, token)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp2.StatusCode)
	}

	var detail struct {
		ID       string `json:"id"`
		Command  string `json:"command"`
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
	}
	json.NewDecoder(resp2.Body).Decode(&detail)
	if detail.ID != created.ID {
		t.Errorf("want id=%q, got %q", created.ID, detail.ID)
	}
	if detail.Command != "echo hello world" {
		t.Errorf("want command=%q, got %q", "echo hello world", detail.Command)
	}
	if detail.Status != "completed" {
		t.Errorf("want completed, got %q", detail.Status)
	}
	if detail.ExitCode != 0 {
		t.Errorf("want exit 0, got %d", detail.ExitCode)
	}
	if !strings.Contains(detail.Stdout, "hello world") {
		t.Errorf("want stdout to contain 'hello world', got %q", detail.Stdout)
	}
}

func TestListSessions(t *testing.T) {
	ts, token := newTestServer(t)

	r1 := postJSON(t, ts.URL+"/api/sessions", `{"command":"echo one"}`, token)
	io.Copy(io.Discard, r1.Body)
	r1.Body.Close()

	r2 := postJSON(t, ts.URL+"/api/sessions", `{"command":"echo two"}`, token)
	io.Copy(io.Discard, r2.Body)
	r2.Body.Close()

	time.Sleep(300 * time.Millisecond)

	resp := getJSON(t, ts.URL+"/api/sessions", token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	var sessions []struct {
		ID      string `json:"id"`
		Command string `json:"command"`
	}
	json.NewDecoder(resp.Body).Decode(&sessions)
	if len(sessions) < 2 {
		t.Fatalf("want at least 2 sessions, got %d", len(sessions))
	}
	if sessions[0].Command != "echo one" && sessions[1].Command != "echo one" {
		t.Errorf("expected to find 'echo one' in sessions")
	}
}

func TestSessionStream(t *testing.T) {
	ts, token := newTestServer(t)

	resp := postJSON(t, ts.URL+"/api/sessions", `{"command":"echo streamtest"}`, token)
	var created struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	sseResp := getJSON(t, ts.URL+"/api/sessions/"+created.ID+"/stream", token)
	defer sseResp.Body.Close()

	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", sseResp.StatusCode)
	}
	if ct := sseResp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("want text/event-stream, got %q", ct)
	}

	data, err := io.ReadAll(sseResp.Body)
	if err != nil {
		t.Fatalf("read sse: %v", err)
	}
	output := string(data)
	if !strings.Contains(output, "stre amtest") && !strings.Contains(output, "\"stdout\"") {
		t.Errorf("expected stdout events, got: %s", output)
	}
	if !strings.Contains(output, "done") {
		t.Errorf("expected 'done' event, got: %s", output)
	}
}

func TestSessionExitCode(t *testing.T) {
	ts, token := newTestServer(t)

	resp := postJSON(t, ts.URL+"/api/sessions", `{"command":"exit 7"}`, token)
	var created struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	time.Sleep(500 * time.Millisecond)

	resp = getJSON(t, ts.URL+"/api/sessions/"+created.ID, token)
	defer resp.Body.Close()

	var detail struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
	}
	json.NewDecoder(resp.Body).Decode(&detail)
	if detail.Status != "failed" {
		t.Errorf("want failed, got %q", detail.Status)
	}
	if detail.ExitCode != 7 {
		t.Errorf("want exit 7, got %d", detail.ExitCode)
	}
}

func TestSessionNotFound(t *testing.T) {
	ts, token := newTestServer(t)

	resp := getJSON(t, ts.URL+"/api/sessions/nonexistent", token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %d", resp.StatusCode)
	}
}

func TestConfigEndpoint(t *testing.T) {
	ts, token := newTestServer(t)

	// GET config
	resp := getJSON(t, ts.URL+"/api/config", token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var cfg struct {
		ExecTimeout float64 `json:"exec_timeout_seconds"`
		MaxOutput   int64   `json:"max_output_bytes"`
	}
	json.NewDecoder(resp.Body).Decode(&cfg)
	if cfg.ExecTimeout != 0 {
		t.Errorf("expected default 0 exec_timeout_seconds, got %f", cfg.ExecTimeout)
	}
}

// ── Project API tests ────────────────────────────────────────

func projectsHandler(t *testing.T) (http.Handler, func()) {
	t.Helper()
	dir := t.TempDir()

	// Create a project directory with a start file
	appDir := filepath.Join(dir, "testapp")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "server.py"), []byte("print('hello')"), 0644); err != nil {
		t.Fatal(err)
	}

	return server.New(server.Config{
		ProjectsRoot: dir,
		Secret:       testSecret,
	}).Handler(), func() {}
}

func TestProjectListEmpty(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(server.New(server.Config{ProjectsRoot: dir, Secret: testSecret}).Handler())
	defer ts.Close()
	token := loginToken(t, ts.URL, testSecret)

	resp := getJSON(t, ts.URL+"/api/projects", token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var projects []interface{}
	json.NewDecoder(resp.Body).Decode(&projects)
	if len(projects) != 0 {
		t.Errorf("expected empty projects, got %d", len(projects))
	}
}

func TestProjectListDirs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "myapp"), 0755)
	os.MkdirAll(filepath.Join(dir, ".hidden"), 0755)

	ts := httptest.NewServer(server.New(server.Config{ProjectsRoot: dir, Secret: testSecret}).Handler())
	defer ts.Close()
	token := loginToken(t, ts.URL, testSecret)

	resp := getJSON(t, ts.URL+"/api/projects/dirs", token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var dirs []string
	json.NewDecoder(resp.Body).Decode(&dirs)
	if len(dirs) != 1 || dirs[0] != "myapp" {
		t.Errorf("expected [myapp], got %v", dirs)
	}
}

func TestProjectFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "myapp"), 0755)
	os.WriteFile(filepath.Join(dir, "myapp", "start.py"), []byte("print('start')"), 0644)
	os.WriteFile(filepath.Join(dir, "myapp", ".gitignore"), []byte(""), 0644)
	os.MkdirAll(filepath.Join(dir, "myapp", "__pycache__"), 0755)
	os.WriteFile(filepath.Join(dir, "myapp", "__pycache__", "cache.py"), []byte(""), 0644)

	ts := httptest.NewServer(server.New(server.Config{ProjectsRoot: dir, Secret: testSecret}).Handler())
	defer ts.Close()
	token := loginToken(t, ts.URL, testSecret)

	resp := getJSON(t, ts.URL+"/api/projects/myapp/files", token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var files []struct {
		Path     string `json:"path"`
		StartCmd string `json:"start_cmd"`
	}
	json.NewDecoder(resp.Body).Decode(&files)
	if len(files) != 1 || files[0].Path != "start.py" {
		t.Errorf("expected [start.py], got %v", files)
	}
}

func TestAddAndRemoveProject(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "testapp"), 0755)

	ts := httptest.NewServer(server.New(server.Config{ProjectsRoot: dir, Secret: testSecret}).Handler())
	defer ts.Close()
	token := loginToken(t, ts.URL, testSecret)

	// Add project
	var shell, flag, startCmd string
	if isWindows() {
		shell, flag = "cmd", "/C"
		startCmd = `cmd /C echo http://127.0.0.1:9999`
	} else {
		shell, flag = "sh", "-c"
		startCmd = `sh -c 'echo http://127.0.0.1:9999'`
	}
	_ = shell
	_ = flag

	body := fmt.Sprintf(`{"name":"testapp","start_cmd":"%s","port":9999}`, startCmd)
	resp := postJSON(t, ts.URL+"/api/projects", body, token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 200/201, got %d", resp.StatusCode)
	}

	var proj struct {
		Name string `json:"name"`
		Port int    `json:"port"`
		URL  string `json:"url"`
	}
	json.NewDecoder(resp.Body).Decode(&proj)
	if proj.Name != "testapp" {
		t.Errorf("want testapp, got %q", proj.Name)
	}
	if proj.Port != 9999 {
		t.Errorf("want port 9999, got %d", proj.Port)
	}

	// List and confirm
	resp2 := getJSON(t, ts.URL+"/api/projects", token)
	defer resp2.Body.Close()
	var projects []struct {
		Name string `json:"name"`
	}
	json.NewDecoder(resp2.Body).Decode(&projects)
	if len(projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(projects))
	}

	// Remove project
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/projects/testapp", nil)
	req.Header.Set("X-Rover-Secret", token)
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp3.StatusCode)
	}

	// List and confirm empty
	resp4 := getJSON(t, ts.URL+"/api/projects", token)
	defer resp4.Body.Close()
	var empty []interface{}
	json.NewDecoder(resp4.Body).Decode(&empty)
	if len(empty) != 0 {
		t.Errorf("expected empty after remove, got %d", len(empty))
	}
}

func TestAddProjectInvalidInput(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(server.New(server.Config{ProjectsRoot: dir, Secret: testSecret}).Handler())
	defer ts.Close()
	token := loginToken(t, ts.URL, testSecret)

	resp := postJSON(t, ts.URL+"/api/projects", `{"name":"","start_cmd":"echo hi"}`, token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400 for empty name, got %d", resp.StatusCode)
	}

	resp = postJSON(t, ts.URL+"/api/projects", `not-json`, token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400 for invalid JSON, got %d", resp.StatusCode)
	}
}

func TestRemoveNonexistentProject(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(server.New(server.Config{ProjectsRoot: dir, Secret: testSecret}).Handler())
	defer ts.Close()
	token := loginToken(t, ts.URL, testSecret)

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/projects/nonexistent", nil)
	req.Header.Set("X-Rover-Secret", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400 for nonexistent, got %d", resp.StatusCode)
	}
}

func TestStartStopProjectViaAPI(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "runtime")
	os.MkdirAll(appDir, 0755)

	ts := httptest.NewServer(server.New(server.Config{ProjectsRoot: dir, Secret: testSecret}).Handler())
	defer ts.Close()
	token := loginToken(t, ts.URL, testSecret)

	// First, register the project via the registry file directly
	var sleepCmd string
	if isWindows() {
		sleepCmd = `cmd /C ping -n 30 127.0.0.1 >nul`
	} else {
		sleepCmd = `sh -c 'sleep 30'`
	}

	// Since this won't validate (no URL output), we'll add it via file
	regContent := fmt.Sprintf(`{"projects":{"runtime":{"name":"runtime","path":"%s","start_cmd":"%s","port":0,"url":"","description":"Active"}}}`,
		escapeJSONPath(appDir), escapeJSON(sleepCmd))
	regPath := filepath.Join(filepath.Dir(os.Args[0]), "projects_registry.json")
	os.WriteFile(regPath, []byte(regContent), 0644)
	// schedule cleanup
	defer os.Remove(regPath)

	// Start project
	resp := postJSON(t, ts.URL+"/api/projects/runtime/start", "", token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}

	var startResp struct {
		Status string `json:"status"`
		Name   string `json:"name"`
	}
	json.NewDecoder(resp.Body).Decode(&startResp)
	if startResp.Status != "starting" {
		t.Errorf("want 'starting', got %q", startResp.Status)
	}

	// Stop project
	resp2 := postJSON(t, ts.URL+"/api/projects/runtime/stop", "", token)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp2.StatusCode)
	}
}

func TestStartNonexistentProject(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(server.New(server.Config{ProjectsRoot: dir, Secret: testSecret}).Handler())
	defer ts.Close()
	token := loginToken(t, ts.URL, testSecret)

	resp := postJSON(t, ts.URL+"/api/projects/nonexistent/start", "", token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400 for nonexistent, got %d", resp.StatusCode)
	}
}

func TestStopNotRunningProject(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(server.New(server.Config{ProjectsRoot: dir, Secret: testSecret}).Handler())
	defer ts.Close()
	token := loginToken(t, ts.URL, testSecret)

	resp := postJSON(t, ts.URL+"/api/projects/nonexistent/stop", "", token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want 400 for not-running, got %d", resp.StatusCode)
	}
}

func TestProjectStreamNotFound(t *testing.T) {
	dir := t.TempDir()
	ts := httptest.NewServer(server.New(server.Config{ProjectsRoot: dir, Secret: testSecret}).Handler())
	defer ts.Close()
	token := loginToken(t, ts.URL, testSecret)

	resp := getJSON(t, ts.URL+"/api/projects/nonexistent/stream", token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %d", resp.StatusCode)
	}
}

func isWindows() bool {
	return os.PathSeparator == '\\'
}

func escapeJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

func escapeJSONPath(s string) string {
	return strings.ReplaceAll(s, `\`, `\\`)
}
