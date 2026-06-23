package launcher

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ProjectInfo struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Port        int    `json:"port,omitempty"`
	StartCmd    string `json:"start_cmd"`
	URL         string `json:"url,omitempty"`
	Description string `json:"description,omitempty"`
}

type StreamEvent struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

type RunningProject struct {
	Name      string    `json:"name"`
	StartTime time.Time `json:"start_time"`
	Port      int       `json:"port,omitempty"`
	URL       string    `json:"url,omitempty"`
}

type runningProcess struct {
	info     RunningProject
	cmd      *exec.Cmd
	output   bytes.Buffer
	outputMu sync.Mutex
	subs     []chan StreamEvent
	subsMu   sync.Mutex
	done     chan struct{}
	cancel   context.CancelFunc
}

type Manager struct {
	projectsRoot string
	registryPath string
	procs        map[string]*runningProcess
	mu           sync.Mutex
	logf         func(string, ...any)
}

type FileInfo struct {
	Path     string `json:"path"`
	StartCmd string `json:"start_cmd"`
}

var eligibleExts = map[string]bool{
	".py": true, ".sh": true, ".bat": true, ".ps1": true,
	".js": true, ".ts": true, ".go": true, ".rb": true,
	".php": true, ".pl": true, ".lua": true,
}

var loopbackURLRe = regexp.MustCompile(`(https?://)(?:localhost|127\.0\.0\.1|0\.0\.0\.0)`)

func localIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "localhost"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "localhost"
}

func replaceLoopback(url string) string {
	ip := localIP()
	if ip == "localhost" {
		return url
	}
	return loopbackURLRe.ReplaceAllString(url, "${1}"+ip)
}

func defaultRegistryPath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "projects_registry.json")
	}
	return ""
}

func NewManager(projectsRoot string) *Manager {
	return &Manager{
		projectsRoot: projectsRoot,
		registryPath: defaultRegistryPath(),
		procs:        make(map[string]*runningProcess),
		logf:         log.Printf,
	}
}

func (m *Manager) SetLogger(logf func(string, ...any)) {
	m.logf = logf
}

func (m *Manager) ProjectsRoot() string {
	return m.projectsRoot
}

func (m *Manager) RegistryPath() string {
	return m.registryPath
}

func (m *Manager) Scan() []ProjectInfo {
	reg := loadRoverRegistry(m.registryPath)
	result := make([]ProjectInfo, 0, len(reg.Projects))
	for _, p := range reg.Projects {
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

func (m *Manager) Start(name string) error {
	m.mu.Lock()
	if _, ok := m.procs[name]; ok {
		m.mu.Unlock()
		return fmt.Errorf("already running")
	}
	m.mu.Unlock()

	projects := m.Scan()
	var proj ProjectInfo
	found := false
	for _, p := range projects {
		if p.Name == name {
			proj = p
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("project %q not found", name)
	}

	ctx, cancel := context.WithCancel(context.Background())

	var shell, flag string
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	} else {
		shell, flag = "sh", "-c"
	}

	cmd := exec.CommandContext(ctx, shell, flag, proj.StartCmd)
	cmd.Dir = proj.Path

	setProcessGroup(cmd)

	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "PYTHONUNBUFFERED=1")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start: %w", err)
	}

	rp := &runningProcess{
		info: RunningProject{
			Name:      name,
			StartTime: time.Now(),
			Port:      proj.Port,
			URL:       proj.URL,
		},
		cmd:    cmd,
		done:   make(chan struct{}),
		cancel: cancel,
	}

	m.mu.Lock()
	m.procs[name] = rp
	m.mu.Unlock()

	go rp.captureOutput(stdout, stderr)
	return nil
}

func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	rp, ok := m.procs[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("not running")
	}
	delete(m.procs, name)
	m.mu.Unlock()

	err := killProcess(rp.cmd)
	rp.cancel()
	select {
	case <-rp.done:
	case <-time.After(5 * time.Second):
	}
	return err
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	names := make([]string, 0, len(m.procs))
	for name := range m.procs {
		names = append(names, name)
	}
	m.mu.Unlock()

	for _, name := range names {
		m.logf("launcher: stopping %s", name)
		if err := m.Stop(name); err != nil {
			m.logf("launcher: stop %s: %v", name, err)
		}
	}
}

func (m *Manager) GetRunning(name string) *RunningProject {
	m.mu.Lock()
	defer m.mu.Unlock()
	rp, ok := m.procs[name]
	if !ok {
		return nil
	}
	cp := rp.info
	return &cp
}

func (m *Manager) ListRunning() []RunningProject {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]RunningProject, 0, len(m.procs))
	for _, rp := range m.procs {
		list = append(list, rp.info)
	}
	return list
}

func (m *Manager) Subscribe(name string) (chan StreamEvent, error) {
	m.mu.Lock()
	rp, ok := m.procs[name]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("not running")
	}

	ch := make(chan StreamEvent, 256)
	rp.subsMu.Lock()
	rp.subs = append(rp.subs, ch)
	rp.subsMu.Unlock()

	rp.outputMu.Lock()
	existing := rp.output.String()
	url := rp.info.URL
	rp.outputMu.Unlock()

	if existing != "" {
		ch <- StreamEvent{Type: "stdout", Data: existing}
	}
	if url != "" {
		ch <- StreamEvent{Type: "url", Data: url}
	}

	return ch, nil
}

func (m *Manager) Unsubscribe(name string, ch chan StreamEvent) {
	m.mu.Lock()
	rp, ok := m.procs[name]
	m.mu.Unlock()
	if !ok {
		return
	}

	rp.subsMu.Lock()
	defer rp.subsMu.Unlock()
	for i, sub := range rp.subs {
		if sub == ch {
			rp.subs = append(rp.subs[:i], rp.subs[i+1:]...)
			close(ch)
			return
		}
	}
}

func (rp *runningProcess) captureOutput(stdout, stderr io.Reader) {
	defer close(rp.done)

	urlRe := regexp.MustCompile(`https?://\S+`)
	var wg sync.WaitGroup
	wg.Add(2)

	scan := func(r io.Reader, kind string) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			out := line + "\n"

			rp.outputMu.Lock()
			rp.output.WriteString(out)
			rp.outputMu.Unlock()

			ev := StreamEvent{Type: kind, Data: out}
			rp.subsMu.Lock()
			for _, ch := range rp.subs {
				select {
				case ch <- ev:
				default:
				}
			}
			rp.subsMu.Unlock()

			if m := urlRe.FindString(line); m != "" {
				url := strings.TrimRight(m, ".,;:!?)}>]\"'`")
				url = replaceLoopback(url)
				rp.outputMu.Lock()
				rp.info.URL = url
				if idx := strings.LastIndex(url, ":"); idx > 0 {
					if p, err := strconv.Atoi(url[idx+1:]); err == nil {
						rp.info.Port = p
					}
				}
				rp.outputMu.Unlock()
				ev := StreamEvent{Type: "url", Data: url}
				rp.subsMu.Lock()
				for _, ch := range rp.subs {
					select {
					case ch <- ev:
					default:
					}
				}
				rp.subsMu.Unlock()
			}
		}
	}

	go scan(stdout, "stdout")
	go scan(stderr, "stderr")
	wg.Wait()

	ev := StreamEvent{Type: "done"}
	rp.subsMu.Lock()
	for _, ch := range rp.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	rp.subsMu.Unlock()
}

func detectStartCmd(relPath string) string {
	ext := strings.ToLower(filepath.Ext(relPath))
	switch ext {
	case ".py":
		return "python " + relPath
	case ".sh":
		return "bash " + relPath
	case ".bat":
		return relPath
	case ".ps1":
		return "pwsh -File " + relPath
	case ".js":
		return "node " + relPath
	case ".ts":
		return "npx tsx " + relPath
	case ".go":
		return "go run " + relPath
	case ".rb":
		return "ruby " + relPath
	case ".php":
		return "php " + relPath
	case ".pl":
		return "perl " + relPath
	case ".lua":
		return "lua " + relPath
	default:
		return relPath
	}
}

func (m *Manager) ListProjectDirs() []string {
	entries, err := os.ReadDir(m.projectsRoot)
	if err != nil {
		return nil
	}

	reg := loadRoverRegistry(m.registryPath)

	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || name == ".venv" || name == "archive" || name == "rover" {
			continue
		}
		if _, ok := reg.Projects[name]; ok {
			continue
		}
		dirs = append(dirs, name)
	}
	sort.Strings(dirs)
	return dirs
}

func (m *Manager) ListEligibleFiles(dir string) []FileInfo {
	root := filepath.Join(m.projectsRoot, filepath.Clean(dir))
	base := filepath.Clean(m.projectsRoot)
	if root != base && !strings.HasPrefix(root, base+string(filepath.Separator)) {
		return nil
	}

	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}

	var files []FileInfo
	filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			name := fi.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "__pycache__" || name == ".venv" || name == "venv" || name == ".mypy_cache" || name == ".pytest_cache" || name == ".ruff_cache" || name == ".tox" || name == ".eggs" {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if !eligibleExts[ext] {
			return nil
		}

		base := fi.Name()
		if base == ".gitignore" || base == ".pre-commit-config.yaml" || strings.HasSuffix(base, ".egg-info") {
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		startCmd := detectStartCmd(rel)
		files = append(files, FileInfo{Path: rel, StartCmd: startCmd})
		return nil
	})

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files
}

func (m *Manager) ValidateProject(dir, startCmd string) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var shell, flag string
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	} else {
		shell, flag = "sh", "-c"
	}

	cmd := exec.CommandContext(ctx, shell, flag, startCmd)
	cmd.Dir = dir
	setProcessGroup(cmd)
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "PYTHONUNBUFFERED=1")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, "", fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, "", fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return 0, "", fmt.Errorf("start: %w", err)
	}
	defer killChildProcesses(cmd)

	urlRe := regexp.MustCompile(`https?://\S+`)
	urlCh := make(chan string, 1)

	scan := func(r io.Reader) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if m := urlRe.FindString(line); m != "" {
				url := strings.TrimRight(m, ".,;:!?)}>]\"'`")
				url = replaceLoopback(url)
				select {
				case urlCh <- url:
				default:
				}
			}
		}
	}

	go scan(stdout)
	go scan(stderr)

	select {
	case url := <-urlCh:
		cancel()
		port := 0
		if idx := strings.LastIndex(url, ":"); idx > 0 {
			if p, err := strconv.Atoi(url[idx+1:]); err == nil {
				port = p
			}
		}
		return port, url, nil
	case <-ctx.Done():
		return 0, "", fmt.Errorf("validation timed out: no URL detected in 15 seconds")
	}
}

func (m *Manager) AddProject(name, startCmd string) (*ProjectInfo, error) {
	dir := filepath.Join(m.projectsRoot, name)
	port, url, err := m.ValidateProject(dir, startCmd)
	if err != nil {
		return nil, err
	}

	reg := loadRoverRegistry(m.registryPath)
	if reg.Projects == nil {
		reg.Projects = make(map[string]ProjectInfo)
	}
	if _, ok := reg.Projects[name]; ok {
		return nil, fmt.Errorf("project %q already exists in rover registry", name)
	}

	p := ProjectInfo{
		Name:        name,
		Path:        dir,
		Port:        port,
		StartCmd:    startCmd,
		URL:         url,
		Description: "Active",
	}
	reg.Projects[name] = p

	if err := saveRoverRegistry(m.registryPath, reg); err != nil {
		return nil, err
	}
	return &p, nil
}

func (m *Manager) RemoveProject(name string) error {
	reg := loadRoverRegistry(m.registryPath)
	if _, ok := reg.Projects[name]; !ok {
		return fmt.Errorf("project %q not found", name)
	}
	delete(reg.Projects, name)
	return saveRoverRegistry(m.registryPath, reg)
}

type roverRegistry struct {
	Projects map[string]ProjectInfo `json:"projects"`
}

func loadRoverRegistry(path string) roverRegistry {
	if path == "" {
		return roverRegistry{Projects: make(map[string]ProjectInfo)}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return roverRegistry{Projects: make(map[string]ProjectInfo)}
	}
	var reg roverRegistry
	if err := json.Unmarshal(data, &reg); err != nil {
		return roverRegistry{Projects: make(map[string]ProjectInfo)}
	}
	if reg.Projects == nil {
		reg.Projects = make(map[string]ProjectInfo)
	}
	return reg
}

func saveRoverRegistry(path string, reg roverRegistry) error {
	if path == "" {
		return fmt.Errorf("registry path not set")
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return killChildProcesses(cmd)
}
