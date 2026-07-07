package launcher

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ylnhari/rover/internal/auth"
)

// ErrPortInUse is returned by Start when the requested port is already taken by
// another process, so callers can prompt for a one-off alternative port.
var ErrPortInUse = errors.New("port already in use")

// PortConflictError reports that the target port is occupied and, when it could
// be identified, by whom. Rover never kills the occupant on its own: callers
// must resolve the conflict explicitly (adopt, confirmed kill, or another port).
type PortConflictError struct {
	Port     int
	Occupant *Occupant
}

func (e *PortConflictError) Error() string {
	if e.Occupant != nil {
		return fmt.Sprintf("port %d is in use by %s", e.Port, e.Occupant)
	}
	return fmt.Sprintf("port %d is in use (could not identify the listener)", e.Port)
}

// Is makes errors.Is(err, ErrPortInUse) work for conflict errors.
func (e *PortConflictError) Is(target error) bool { return target == ErrPortInUse }

// portFlagRe matches a standalone --port flag (not e.g. --portfolio-mode).
var portFlagRe = regexp.MustCompile(`(^|\s)--port(=|\s|$)`)

// composeStartCmd injects the chosen port into a project's start command.
// A "{port}" placeholder is substituted; otherwise, if the command doesn't
// already pass --port, "--port <port>" is appended. With no port (<=0) the
// command is returned unchanged. The port is also always exported as the PORT
// environment variable at launch, which most frameworks honor.
func composeStartCmd(startCmd string, port int) string {
	if port <= 0 {
		return startCmd
	}
	if strings.Contains(startCmd, "{port}") {
		return strings.ReplaceAll(startCmd, "{port}", strconv.Itoa(port))
	}
	if portFlagRe.MatchString(startCmd) {
		return startCmd
	}
	return fmt.Sprintf("%s --port %d", startCmd, port)
}

// portAvailable reports whether a TCP port on localhost is free to bind.
func portAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// Project kinds, classified by the validation/readiness probe. The zero value
// (empty) means "registered before kinds existed" and is treated like web so
// existing registries keep today's behavior.
const (
	KindWeb  = "web"  // listens and speaks HTTP: proxyable
	KindTCP  = "tcp"  // listens but does not speak HTTP: rover cannot proxy it
	KindTask = "task" // no port: a worker/script, console output is the product
)

type ProjectInfo struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Port         int    `json:"port,omitempty"`
	StartCmd     string `json:"start_cmd"`
	URL          string `json:"url,omitempty"`
	Description  string `json:"description,omitempty"`
	Kind         string `json:"kind,omitempty"`
	ProxyEnabled bool   `json:"proxy_enabled"`
	// ProxyPort is the stable, persisted port the reverse proxy listens on, so
	// bookmarks survive restarts. 0 = not yet allocated (assigned on first
	// proxied start).
	ProxyPort int `json:"proxy_port,omitempty"`
}

type StreamEvent struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

// Project lifecycle states as reported in RunningProject.State.
const (
	StateStarting = "starting" // process launched, listener not yet confirmed
	StateRunning  = "running"  // confirmed listening on its port
	StateAdopted  = "adopted"  // pre-existing process adopted (not launched by rover)
)

type RunningProject struct {
	Name      string    `json:"name"`
	StartTime time.Time `json:"start_time"`
	Port      int       `json:"port,omitempty"`
	// URL is what the app itself reports (scraped banner or loopback fallback).
	// It is informational: nothing guarantees it works off the rover host.
	URL string `json:"url,omitempty"`
	// DirectURL is set only after rover verified, by dialing the socket, that
	// the app is reachable on rover's advertised interface. Empty means
	// loopback-only (or unverified): the UI must not offer a direct link then.
	DirectURL  string `json:"direct_url,omitempty"`
	ProxyPort  int    `json:"proxy_port,omitempty"`
	ProxyURL   string `json:"proxy_url,omitempty"`
	ProxyError string `json:"proxy_error,omitempty"`
	State      string `json:"state,omitempty"`
	PID        int    `json:"pid,omitempty"`
}

// ExitStatus records how a project's last run ended.
type ExitStatus struct {
	Code    int       `json:"code"`
	Time    time.Time `json:"time"`
	Stopped bool      `json:"stopped"` // true when rover stopped it deliberately
}

// maxConsoleBuffer bounds the in-memory console history kept per project so a
// chatty server can't grow rover's memory without limit.
const maxConsoleBuffer = 512 * 1024

type runningProcess struct {
	// outputMu guards both output and info (mutated by the scanner, the
	// readiness probe, and the proxy starter).
	info          RunningProject
	cmd           *exec.Cmd // nil for adopted processes
	output        bytes.Buffer
	outputMu      sync.Mutex
	subs          []chan StreamEvent
	subsMu        sync.Mutex
	done          chan struct{}
	reaped        chan struct{} // closed by reap() after the exit is recorded
	cancel        context.CancelFunc
	proxyServer   *http.Server
	proxyLn       net.Listener
	exited        atomic.Bool
	stopRequested atomic.Bool
}

func (rp *runningProcess) broadcast(ev StreamEvent) {
	rp.subsMu.Lock()
	for _, ch := range rp.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	rp.subsMu.Unlock()
}

// shutdownProxy stops the reverse proxy if one is running. Safe to call from
// Stop and the reaper concurrently with the async proxy starter (proxy fields
// are guarded by outputMu).
func (rp *runningProcess) shutdownProxy() {
	rp.outputMu.Lock()
	srv, ln := rp.proxyServer, rp.proxyLn
	rp.proxyServer, rp.proxyLn = nil, nil
	rp.outputMu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		srv.Shutdown(ctx)
		cancel()
	}
	if ln != nil {
		ln.Close()
	}
}

// appendOutput appends to the bounded console buffer, trimming the oldest
// lines once it exceeds maxConsoleBuffer. Caller must hold outputMu.
func (rp *runningProcess) appendOutput(s string) {
	rp.output.WriteString(s)
	if rp.output.Len() <= maxConsoleBuffer {
		return
	}
	b := rp.output.Bytes()
	keep := b[len(b)-maxConsoleBuffer*3/4:]
	if i := bytes.IndexByte(keep, '\n'); i >= 0 && i+1 < len(keep) {
		keep = keep[i+1:]
	}
	var nb bytes.Buffer
	nb.WriteString("[rover] older console output truncated\n")
	nb.Write(keep)
	rp.output = nb
}

type Manager struct {
	projectsRoot string
	registryPath string
	procs        map[string]*runningProcess
	lastExits    map[string]ExitStatus
	mu           sync.Mutex
	// regMu serializes registry read-modify-write cycles: HTTP handlers and
	// the async proxy starter (persistProxyPort) mutate the file concurrently.
	regMu sync.Mutex
	logf  func(string, ...any)
	// bindHost is the interface rover itself listens on. Project proxies bind
	// to the same host so a proxy is never reachable from a network rover is
	// not. Empty means all interfaces (0.0.0.0).
	bindHost string
	// probeTimeout bounds how long registration validation and start-time
	// readiness probes wait for the project to begin listening.
	probeTimeout time.Duration
	// Proxy auth gate: when proxyAuthOn, proxy requests must carry a valid
	// signed rover cookie; unauthenticated browsers are redirected to rover's
	// UI (roverScheme://host:roverPort) to log in.
	proxyAuthOn bool
	proxySecret string
	roverPort   string
	roverTLS    bool
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

// proxyURLHost picks the host to advertise in a project's proxy URL. When rover
// is bound to a specific routable interface (e.g. a Tailscale IP), that address
// is exactly what a remote client should hit, so use it verbatim. For an
// all-interfaces or loopback bind, fall back to a routable LAN/tailnet IP.
func proxyURLHost(bindHost string) string {
	switch bindHost {
	case "", "0.0.0.0", "::", "[::]":
		return localIP()
	}
	if ip := net.ParseIP(strings.Trim(bindHost, "[]")); ip != nil && ip.IsLoopback() {
		return localIP()
	}
	return bindHost
}

// verifyDirectURL checks socket-level reality instead of trusting log banners:
// it dials the project's port on rover's advertised interface and returns a
// direct URL only when something actually accepts connections there (i.e. the
// app bound 0.0.0.0 or that interface, not just loopback). Returns "" when the
// app is only reachable on the rover host, when rover has no non-loopback
// interface to advertise, or when proxy auth is on — a direct link would be a
// silent bypass of the auth gate the operator turned on, so it is never
// advertised alongside the authenticated proxy link.
func (m *Manager) verifyDirectURL(port int) string {
	if m.proxyAuthOn || port <= 0 {
		return ""
	}
	host := proxyURLHost(m.bindHost)
	if host == "localhost" {
		return ""
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), time.Second)
	if err != nil {
		return ""
	}
	conn.Close()
	return fmt.Sprintf("http://%s:%d", host, port)
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
		lastExits:    make(map[string]ExitStatus),
		logf:         log.Printf,
		probeTimeout: 30 * time.Second,
	}
}

func (m *Manager) SetLogger(logf func(string, ...any)) {
	m.logf = logf
}

// SetBindHost records the interface rover listens on so project proxies inherit
// the same exposure. Pass the host portion of rover's --addr ("" = all interfaces).
func (m *Manager) SetBindHost(host string) {
	m.bindHost = host
}

// SetProbeTimeout configures how long validation / readiness probes wait for a
// project to start listening.
func (m *Manager) SetProbeTimeout(d time.Duration) {
	if d > 0 {
		m.probeTimeout = d
	}
}

// SetProxyAuth configures the proxy authentication gate. When on, every proxy
// request must carry a valid signed rover cookie; browsers without one are
// redirected to rover's own UI (reached at roverPort on the same host) to log
// in, which sets the cookie for the whole host.
func (m *Manager) SetProxyAuth(on bool, secret, roverPort string, roverTLS bool) {
	m.proxyAuthOn = on && secret != ""
	m.proxySecret = secret
	m.roverPort = roverPort
	m.roverTLS = roverTLS
}

// startProxy binds a reverse proxy for rp on the requested port (0 = ephemeral)
// on rover's own interface. Requests are gated on the tracked process still
// being alive — the proxy never forwards to a port whose owner has exited —
// and, when proxy auth is on, on a valid signed rover cookie.
func (m *Manager) startProxy(rp *runningProcess, targetPort, proxyPort int) (net.Listener, *http.Server, int, error) {
	// Bind the proxy to the same interface rover listens on so it is never
	// reachable from a network rover itself is not exposed to.
	ln, err := net.Listen("tcp", net.JoinHostPort(m.bindHost, strconv.Itoa(proxyPort)))
	if err != nil {
		return nil, nil, 0, err
	}
	pport := ln.Addr().(*net.TCPAddr).Port

	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", targetPort))
	proxy := httputil.NewSingleHostReverseProxy(target)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rp.exited.Load() {
			http.Error(w, "project is not running", http.StatusServiceUnavailable)
			return
		}
		if m.proxyAuthOn {
			c, err := r.Cookie("rover_proxy")
			if err != nil || auth.VerifyToken(m.proxySecret, c.Value) != nil {
				host := r.Host
				if h, _, err := net.SplitHostPort(host); err == nil {
					host = h
				}
				scheme := "http"
				if m.roverTLS {
					scheme = "https"
				}
				next := "http://" + r.Host + r.URL.RequestURI()
				dest := fmt.Sprintf("%s://%s/?next=%s", scheme, net.JoinHostPort(host, m.roverPort), url.QueryEscape(next))
				http.Redirect(w, r, dest, http.StatusFound)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	})

	srv := &http.Server{Handler: handler}

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("proxy listener error: %v", err)
		}
	}()

	return ln, srv, pport, nil
}

// startProxyFor starts the reverse proxy for a tracked project, preferring the
// project's stable persisted proxy port. On failure it falls back to an
// ephemeral port (and persists the new one); if that fails too, the error is
// recorded on the running info so the UI can surface it.
func (m *Manager) startProxyFor(name string, rp *runningProcess, proj ProjectInfo) {
	rp.outputMu.Lock()
	targetPort := rp.info.Port
	rp.outputMu.Unlock()
	ln, srv, pport, err := m.startProxy(rp, targetPort, proj.ProxyPort)
	if err != nil && proj.ProxyPort > 0 {
		m.logf("launcher: proxy for %s: stable port %d unavailable (%v), falling back to an ephemeral port", name, proj.ProxyPort, err)
		ln, srv, pport, err = m.startProxy(rp, rp.info.Port, 0)
	}
	if err != nil {
		m.logf("launcher: proxy for %s: %v", name, err)
		rp.outputMu.Lock()
		rp.info.ProxyError = fmt.Sprintf("proxy failed to start: %v", err)
		rp.outputMu.Unlock()
		return
	}
	rp.outputMu.Lock()
	rp.proxyLn = ln
	rp.proxyServer = srv
	rp.info.ProxyPort = pport
	rp.info.ProxyURL = fmt.Sprintf("http://%s:%d", proxyURLHost(m.bindHost), pport)
	proxyURL := rp.info.ProxyURL
	rp.outputMu.Unlock()
	if pport != proj.ProxyPort {
		if err := m.persistProxyPort(name, pport); err != nil {
			m.logf("launcher: could not persist proxy port for %s: %v", name, err)
		}
	}
	rp.broadcast(StreamEvent{Type: "proxy", Data: proxyURL})
}

func (m *Manager) persistProxyPort(name string, port int) error {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	reg := loadRoverRegistry(m.registryPath)
	p, ok := reg.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	p.ProxyPort = port
	reg.Projects[name] = p
	return saveRoverRegistry(m.registryPath, reg)
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

// StartOptions controls a single project start.
type StartOptions struct {
	// PortOverride (>0) overrides the registered default port for this run only.
	PortOverride int
	// KillOccupant authorizes killing the process currently listening on the
	// target port — but only when its PID matches ConfirmPID, which the caller
	// must have obtained from a prior PortConflictError. This is the only path
	// by which rover ever kills a process it did not start.
	KillOccupant bool
	ConfirmPID   int
}

// Start launches a project. If the target port is occupied it never kills the
// occupant silently: it returns a *PortConflictError identifying the listener,
// unless the caller explicitly confirmed the kill via StartOptions.
func (m *Manager) Start(name string, opts StartOptions) error {
	m.mu.Lock()
	if _, ok := m.procs[name]; ok {
		m.mu.Unlock()
		return fmt.Errorf("already running")
	}
	m.mu.Unlock()

	proj := m.GetProject(name)
	if proj == nil {
		return fmt.Errorf("project %q not found", name)
	}

	port := proj.Port
	if opts.PortOverride > 0 {
		port = opts.PortOverride
	}
	if port > 0 && !portAvailable(port) {
		occ := FindListenerOnPort(port)
		if opts.KillOccupant && occ != nil && opts.ConfirmPID == occ.PID {
			if err := KillConfirmedListener(port, occ.PID); err != nil {
				m.logf("launcher: confirmed kill of pid %d on port %d failed: %v", occ.PID, port, err)
				return &PortConflictError{Port: port, Occupant: occ}
			}
			m.logf("launcher: killed pid %d (%s) on port %d — confirmed by user", occ.PID, occ.Name, port)
			if !waitPortFree(port, 2*time.Second) {
				return &PortConflictError{Port: port, Occupant: FindListenerOnPort(port)}
			}
		} else {
			return &PortConflictError{Port: port, Occupant: occ}
		}
	}

	startCmd := composeStartCmd(proj.StartCmd, port)

	ctx, cancel := context.WithCancel(context.Background())

	var shell, flag string
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	} else {
		shell, flag = "sh", "-c"
	}

	cmd := exec.CommandContext(ctx, shell, flag, startCmd)
	cmd.Dir = proj.Path

	setProcessGroup(cmd)

	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env,
		"PYTHONUNBUFFERED=1",
		"PYTHONIOENCODING=utf-8",
		"PYTHONLEGACYWINDOWSSTDIO=utf-8",
	)
	if port > 0 {
		cmd.Env = append(cmd.Env, "PORT="+strconv.Itoa(port))
	}

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
			Port:      port,
			State:     StateStarting,
			PID:       cmd.Process.Pid,
		},
		cmd:    cmd,
		done:   make(chan struct{}),
		reaped: make(chan struct{}),
		cancel: cancel,
	}

	m.mu.Lock()
	if _, ok := m.procs[name]; ok {
		// A concurrent Start won the race between the top-of-function check
		// and here; kill the process we just spawned instead of leaking it.
		m.mu.Unlock()
		killProcess(cmd)
		cancel()
		return fmt.Errorf("already running")
	}
	m.procs[name] = rp
	delete(m.lastExits, name)
	m.mu.Unlock()

	go rp.captureOutput(stdout, stderr)
	go m.reap(name, rp)
	go m.confirmStarted(name, rp, *proj, port)

	return nil
}

// confirmStarted probes the project's port until something is listening, then
// promotes the project to running, advertises its URL, and starts the reverse
// proxy. The URL is never advertised — and the proxy never forwards — before
// the listener is confirmed.
func (m *Manager) confirmStarted(name string, rp *runningProcess, proj ProjectInfo, port int) {
	if port <= 0 {
		// No port to probe; treat the project as running once launched.
		rp.outputMu.Lock()
		rp.info.State = StateRunning
		rp.outputMu.Unlock()
		rp.broadcast(StreamEvent{Type: "ready"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), m.probeTimeout)
	defer cancel()
	go func() {
		select {
		case <-rp.done: // process exited; stop probing
			cancel()
		case <-ctx.Done():
		}
	}()

	res := probeServer(ctx, port)
	if !res.Listening {
		if rp.exited.Load() {
			return // reaper reports the failure
		}
		msg := fmt.Sprintf("[rover] warning: nothing is listening on port %d after %s — the app may use a different port or still be booting\n", port, m.probeTimeout)
		m.logf("launcher: %s: nothing listening on port %d after %s", name, port, m.probeTimeout)
		rp.outputMu.Lock()
		rp.appendOutput(msg)
		rp.outputMu.Unlock()
		rp.broadcast(StreamEvent{Type: "stderr", Data: msg})
		return
	}

	// Dial the port on rover's advertised interface (outside the lock) so the
	// direct link reflects the actual socket, not a guess from log text.
	direct := m.verifyDirectURL(port)

	rp.outputMu.Lock()
	rp.info.State = StateRunning
	if rp.info.URL == "" {
		rp.info.URL = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	rp.info.DirectURL = direct
	url := rp.info.URL
	rp.outputMu.Unlock()
	rp.broadcast(StreamEvent{Type: "url", Data: url})
	rp.broadcast(StreamEvent{Type: "ready", Data: direct})

	if proj.ProxyEnabled && m.classifyListener(name, &proj, res.HTTP) {
		m.startProxyFor(name, rp, proj)
	}
}

// classifyListener reconciles the probe's HTTP classification with the
// project's persisted kind and reports whether the listener is proxyable.
// "web" is sticky: one failed GET at boot (an app still warming up) must not
// take down the proxy for a project that already proved it speaks HTTP. An
// upgrade to web is persisted so the registry converges on observed reality.
// Only a project both classified tcp and failing the HTTP check now is not
// proxied — the reverse proxy would just serve 502s for it. A legacy empty
// kind stays proxyable, exactly as before kinds existed.
func (m *Manager) classifyListener(name string, proj *ProjectInfo, isHTTP bool) bool {
	if isHTTP && proj.Kind != KindWeb {
		proj.Kind = KindWeb
		if err := m.persistKind(name, KindWeb); err != nil {
			m.logf("launcher: could not persist kind for %s: %v", name, err)
		}
	}
	return isHTTP || proj.Kind != KindTCP
}

func (m *Manager) persistKind(name, kind string) error {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	reg := loadRoverRegistry(m.registryPath)
	p, ok := reg.Projects[name]
	if !ok {
		return fmt.Errorf("project %q not found", name)
	}
	p.Kind = kind
	reg.Projects[name] = p
	return saveRoverRegistry(m.registryPath, reg)
}

// reap waits for the child to exit, collects its exit status (preventing
// zombies), removes it from the running set, tears down its proxy, and tells
// stream subscribers how the run ended.
func (m *Manager) reap(name string, rp *runningProcess) {
	defer close(rp.reaped)
	<-rp.done // output scanners finished (pipes closed = process exited)

	code := 0
	if err := rp.cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	rp.exited.Store(true)
	stopped := rp.stopRequested.Load()

	rp.shutdownProxy()

	m.mu.Lock()
	if cur, ok := m.procs[name]; ok && cur == rp {
		delete(m.procs, name)
	}
	m.lastExits[name] = ExitStatus{Code: code, Time: time.Now(), Stopped: stopped}
	m.mu.Unlock()

	if !stopped {
		m.logf("launcher: %s exited with code %d", name, code)
	}
	rp.broadcast(StreamEvent{Type: "exit", Data: strconv.Itoa(code)})
	rp.broadcast(StreamEvent{Type: "done"})
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

	rp.stopRequested.Store(true)

	// Shutdown proxy listener first
	rp.shutdownProxy()

	if rp.cmd == nil {
		// Adopted process: detach only — rover does not own it, so it is left
		// running. The user can kill it via the confirmed-kill start flow.
		rp.exited.Store(true)
		m.mu.Lock()
		m.lastExits[name] = ExitStatus{Time: time.Now(), Stopped: true}
		m.mu.Unlock()
		rp.broadcast(StreamEvent{Type: "done"})
		m.logf("launcher: detached from adopted process for %s (process left running)", name)
		return nil
	}

	err := killProcess(rp.cmd)
	rp.cancel()
	// Wait for the reaper, not just the pipes: when Stop returns, the exit is
	// recorded (LastExit) and the process table is consistent.
	select {
	case <-rp.reaped:
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

// Adopt attaches rover to a process that is already listening on the project's
// registered port (e.g. the same app started by hand, or an orphan from a
// previous rover run): it verifies something is listening, tracks it as
// running, and starts the reverse proxy for it. Stop on an adopted project
// detaches without killing.
func (m *Manager) Adopt(name string) (*RunningProject, error) {
	m.mu.Lock()
	if _, ok := m.procs[name]; ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("already running")
	}
	m.mu.Unlock()

	proj := m.GetProject(name)
	if proj == nil {
		return nil, fmt.Errorf("project %q not found", name)
	}
	if proj.Port <= 0 {
		return nil, fmt.Errorf("project %q has no registered port to adopt", name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res := probeServer(ctx, proj.Port)
	if !res.Listening {
		return nil, fmt.Errorf("nothing is listening on port %d", proj.Port)
	}

	occ := FindListenerOnPort(proj.Port)
	pid := 0
	desc := "unknown process"
	if occ != nil {
		pid = occ.PID
		desc = occ.String()
	}

	rp := &runningProcess{
		info: RunningProject{
			Name:      name,
			StartTime: time.Now(),
			Port:      proj.Port,
			URL:       fmt.Sprintf("http://127.0.0.1:%d", proj.Port),
			DirectURL: m.verifyDirectURL(proj.Port),
			State:     StateAdopted,
			PID:       pid,
		},
		done: make(chan struct{}),
	}
	rp.appendOutput(fmt.Sprintf("[rover] adopted %s listening on port %d (not started by rover; Stop detaches without killing)\n", desc, proj.Port))

	m.mu.Lock()
	if _, ok := m.procs[name]; ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("already running")
	}
	m.procs[name] = rp
	delete(m.lastExits, name)
	m.mu.Unlock()

	m.logf("launcher: adopted %s on port %d for project %s", desc, proj.Port, name)

	if proj.ProxyEnabled && m.classifyListener(name, proj, res.HTTP) {
		m.startProxyFor(name, rp, *proj)
	}

	cp := rp.info
	return &cp, nil
}

func (m *Manager) GetRunning(name string) *RunningProject {
	m.mu.Lock()
	rp, ok := m.procs[name]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	rp.outputMu.Lock()
	cp := rp.info
	rp.outputMu.Unlock()
	return &cp
}

func (m *Manager) ListRunning() []RunningProject {
	m.mu.Lock()
	procs := make([]*runningProcess, 0, len(m.procs))
	for _, rp := range m.procs {
		procs = append(procs, rp)
	}
	m.mu.Unlock()
	list := make([]RunningProject, 0, len(procs))
	for _, rp := range procs {
		rp.outputMu.Lock()
		list = append(list, rp.info)
		rp.outputMu.Unlock()
	}
	return list
}

// LastExit reports how the project's most recent run ended, or nil if it never
// ran (or is currently running).
func (m *Manager) LastExit(name string) *ExitStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, running := m.procs[name]; running {
		return nil
	}
	if ex, ok := m.lastExits[name]; ok {
		cp := ex
		return &cp
	}
	return nil
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
			rp.appendOutput(out)
			rp.outputMu.Unlock()

			rp.broadcast(StreamEvent{Type: kind, Data: out})

			if match := probeURLRe.FindString(line); match != "" {
				u := strings.TrimRight(match, ".,;:!?)}>]\"'`")
				rp.outputMu.Lock()
				// The readiness probe is authoritative for the URL; a scraped
				// URL only fills the gap when nothing is known yet. It is kept
				// exactly as the app printed it — it is a log line, not a
				// verified listener, so rewriting its host would fabricate a
				// reachability claim rover hasn't checked (DirectURL is the
				// only field with a verified host).
				set := false
				if rp.info.URL == "" {
					rp.info.URL = u
					set = true
					if parsed, err := url.Parse(u); err == nil && parsed.Port() != "" {
						if p, err := strconv.Atoi(parsed.Port()); err == nil && rp.info.Port == 0 {
							rp.info.Port = p
						}
					}
				}
				rp.outputMu.Unlock()
				if set {
					rp.broadcast(StreamEvent{Type: "url", Data: u})
				}
			}
		}
	}

	go scan(stdout, "stdout")
	go scan(stderr, "stderr")
	wg.Wait()
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

// projectNameRe constrains project names to safe path components: no
// separators, no leading dot, max 64 chars.
var projectNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// AddProject registers a project at a user-supplied port. The raw startCmd is
// stored without the port; rover supplies it at launch (via composeStartCmd and
// the PORT env var), so the port can be changed later without rewriting the
// command. The project is validated by launching it and probing the port until
// something actually listens (see ValidateProject), and classified web or tcp
// by whether the listener answered HTTP. Port 0 registers a port-less task
// (worker/script): validated only by a short grace run (see ValidateTask), no
// URL, no proxy. The returned ValidationReport is non-nil whenever validation
// ran, on success and failure alike.
func (m *Manager) AddProject(name, startCmd string, port int) (*ProjectInfo, *ValidationReport, error) {
	if !projectNameRe.MatchString(name) {
		return nil, nil, fmt.Errorf("invalid project name %q: use letters, digits, dot, dash or underscore (no path separators, no leading dot)", name)
	}
	if port != 0 && (port < 1024 || port > 65535) {
		return nil, nil, fmt.Errorf("port must be in 1024-65535, or 0 for a port-less task")
	}
	dir := filepath.Join(m.projectsRoot, name)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, nil, fmt.Errorf("project directory %q not found under projects root", name)
	}

	if err := m.checkRegistrable(name, port); err != nil {
		return nil, nil, err
	}

	var report *ValidationReport
	var err error
	var url, kind string
	if port == 0 {
		kind = KindTask
		report, err = m.ValidateTask(dir, startCmd)
	} else {
		report, err = m.ValidateProject(dir, composeStartCmd(startCmd, port), port)
		if report != nil && report.Probe.Listening {
			kind = KindTCP
			if report.Probe.HTTP {
				kind = KindWeb
			}
		}
	}
	if err != nil {
		return nil, report, err
	}
	if port > 0 {
		url = report.DetectedURL
		if url == "" {
			url = fmt.Sprintf("http://127.0.0.1:%d", port)
		}
	}

	p := ProjectInfo{
		Name:         name,
		Path:         dir,
		Port:         port,
		StartCmd:     startCmd,
		URL:          url,
		Kind:         kind,
		ProxyEnabled: port > 0,
	}

	// Validation ran unlocked (it can take up to the probe timeout), so
	// re-check uniqueness under the registry lock before committing.
	m.regMu.Lock()
	defer m.regMu.Unlock()
	if err := m.checkRegistrable(name, port); err != nil {
		return nil, report, err
	}
	reg := loadRoverRegistry(m.registryPath)
	reg.Projects[name] = p
	if err := saveRoverRegistry(m.registryPath, reg); err != nil {
		return nil, report, err
	}
	return &p, report, nil
}

// checkRegistrable reports whether name/port are free in the registry.
func (m *Manager) checkRegistrable(name string, port int) error {
	reg := loadRoverRegistry(m.registryPath)
	if _, ok := reg.Projects[name]; ok {
		return fmt.Errorf("project %q already exists in rover registry", name)
	}
	for _, p := range reg.Projects {
		// Port-less tasks (port 0) never collide with each other.
		if port > 0 && p.Port == port {
			return fmt.Errorf("port %d is already assigned to project %q", port, p.Name)
		}
	}
	return nil
}

// UpdateProjectPort changes a project's registered default port.
func (m *Manager) UpdateProjectPort(name string, port int) (*ProjectInfo, error) {
	if port <= 0 {
		return nil, fmt.Errorf("port must be greater than 0")
	}
	m.regMu.Lock()
	defer m.regMu.Unlock()
	reg := loadRoverRegistry(m.registryPath)
	p, ok := reg.Projects[name]
	if !ok {
		return nil, fmt.Errorf("project %q not found", name)
	}
	for _, other := range reg.Projects {
		if other.Name != name && other.Port == port {
			return nil, fmt.Errorf("port %d is already assigned to project %q", port, other.Name)
		}
	}
	p.Port = port
	p.URL = fmt.Sprintf("http://127.0.0.1:%d", port)
	reg.Projects[name] = p
	if err := saveRoverRegistry(m.registryPath, reg); err != nil {
		return nil, err
	}
	return &p, nil
}

// UpdateProjectCommand updates the start command for a project.
func (m *Manager) UpdateProjectCommand(name, startCmd string) (*ProjectInfo, error) {
	if startCmd == "" {
		return nil, fmt.Errorf("start command must not be empty")
	}
	m.regMu.Lock()
	defer m.regMu.Unlock()
	reg := loadRoverRegistry(m.registryPath)
	p, ok := reg.Projects[name]
	if !ok {
		return nil, fmt.Errorf("project %q not found", name)
	}
	p.StartCmd = startCmd
	reg.Projects[name] = p
	if err := saveRoverRegistry(m.registryPath, reg); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetProject returns the project info for a given name, or nil if not found.
func (m *Manager) GetProject(name string) *ProjectInfo {
	projects := m.Scan()
	for _, p := range projects {
		if p.Name == name {
			return &p
		}
	}
	return nil
}

// SetProxyEnabled updates the proxy_enabled flag for a project in the registry.
func (m *Manager) SetProxyEnabled(name string, enabled bool) (*ProjectInfo, error) {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	reg := loadRoverRegistry(m.registryPath)
	p, ok := reg.Projects[name]
	if !ok {
		return nil, fmt.Errorf("project %q not found", name)
	}
	p.ProxyEnabled = enabled
	reg.Projects[name] = p
	if err := saveRoverRegistry(m.registryPath, reg); err != nil {
		return nil, err
	}
	return &p, nil
}

func (m *Manager) RemoveProject(name string) error {
	m.regMu.Lock()
	defer m.regMu.Unlock()
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

	// Migration: existing projects without proxy_enabled field in JSON
	// default to false (Go zero value). Check raw JSON and set true where
	// the field is absent.
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawMap); err == nil {
		if rawProjects, ok := rawMap["projects"]; ok {
			var projMap map[string]json.RawMessage
			if err := json.Unmarshal(rawProjects, &projMap); err == nil {
				for name, raw := range projMap {
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(raw, &fields); err == nil {
						if _, exists := fields["proxy_enabled"]; !exists {
							p := reg.Projects[name]
							p.ProxyEnabled = true
							reg.Projects[name] = p
						}
					}
				}
			}
		}
	}

	return reg
}

// saveRoverRegistry writes the registry atomically (temp file + rename) with
// owner-only permissions, so a crash mid-write can't tear the file.
func saveRoverRegistry(path string, reg roverRegistry) error {
	if path == "" {
		return fmt.Errorf("registry path not set")
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	// On Windows the rename fails spuriously when a virus scanner or indexer
	// briefly holds the just-written temp file or the destination (likely when
	// two saves land back to back, e.g. kind then proxy port); retry briefly
	// rather than dropping the update.
	for i := 0; ; i++ {
		err = os.Rename(tmp, path)
		if err == nil || i >= 9 {
			return err
		}
		time.Sleep(time.Duration(i+1) * 10 * time.Millisecond)
	}
}

func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return killChildProcesses(cmd)
}

// waitPortFree polls until the port is bindable or the timeout elapses.
func waitPortFree(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if portAvailable(port) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return portAvailable(port)
}
