package server

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ylnhari/rover/internal/auth"
	"github.com/ylnhari/rover/internal/launcher"
	"github.com/ylnhari/rover/internal/version"
)

const (
	maxCommandBytes   = 10 * 1024
	maxAddProjectBody = 4 * 1024
	scanBufSize       = 256 * 1024
	maxSessions       = 500
	loginMaxPerMin    = 10
)

type rateLimiter struct {
	mu      sync.Mutex
	entries map[string]*rlEntry
}

type rlEntry struct {
	count   int
	resetAt time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{entries: make(map[string]*rlEntry)}
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	e, ok := rl.entries[ip]
	if !ok || now.After(e.resetAt) {
		rl.entries[ip] = &rlEntry{count: 1, resetAt: now.Add(time.Minute)}
		return true
	}
	if e.count >= loginMaxPerMin {
		return false
	}
	e.count++
	return true
}

type SessionStatus string

const (
	StatusRunning   SessionStatus = "running"
	StatusCompleted SessionStatus = "completed"
	StatusFailed    SessionStatus = "failed"
)

type streamEvent struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	ExitCode int    `json:"exit_code"`
}

type Session struct {
	mu        sync.RWMutex
	ID        string
	Command   string
	StartTime time.Time
	EndTime   *time.Time
	ExitCode  int
	Status    SessionStatus
	Stdout    string
	Stderr    string
	subs      []chan streamEvent
	subsMu    sync.Mutex
}

func (s *Session) subscribe() chan streamEvent {
	ch := make(chan streamEvent, 64)
	s.subsMu.Lock()
	s.subs = append(s.subs, ch)
	s.subsMu.Unlock()
	return ch
}

func (s *Session) unsubscribe(ch chan streamEvent) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for i, sub := range s.subs {
		if sub == ch {
			s.subs = append(s.subs[:i], s.subs[i+1:]...)
			close(ch)
			return
		}
	}
}

func (s *Session) broadcast(ev streamEvent) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

type SessionManager struct {
	mu          sync.RWMutex
	sessions    map[string]*Session
	execTimeout time.Duration
	maxOutput   int64
	persistPath string
}

func (sm *SessionManager) loadFromDisk() {
	if sm.persistPath == "" {
		return
	}
	data, err := os.ReadFile(sm.persistPath)
	if err != nil {
		return
	}
	var saved []json.RawMessage
	if err := json.Unmarshal(data, &saved); err != nil {
		return
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for _, raw := range saved {
		var s Session
		if err := json.Unmarshal(raw, &s); err == nil && s.ID != "" {
			sm.sessions[s.ID] = &s
		}
	}
}

func (sm *SessionManager) persistAllToDisk() {
	if sm.persistPath == "" {
		return
	}
	sm.mu.RLock()
	list := make([]*Session, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		list = append(list, s)
	}
	sm.mu.RUnlock()

	// Only persist completed sessions, newest-first capped at maxSessions.
	out := make([]json.RawMessage, 0, len(list))
	for _, s := range list {
		s.mu.RLock()
		done := s.Status != StatusRunning
		s.mu.RUnlock()
		if !done {
			continue
		}
		b, err := json.Marshal(s)
		if err == nil {
			out = append(out, b)
		}
		if len(out) >= maxSessions {
			break
		}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(sm.persistPath, data, 0600)
}

func NewSessionManager() *SessionManager {
	return &SessionManager{sessions: make(map[string]*Session)}
}

func (sm *SessionManager) GetExecTimeout() time.Duration {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.execTimeout
}

func (sm *SessionManager) SetExecTimeout(d time.Duration) {
	sm.mu.Lock()
	sm.execTimeout = d
	sm.mu.Unlock()
}

func (sm *SessionManager) GetMaxOutput() int64 {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.maxOutput
}

func (sm *SessionManager) SetMaxOutput(n int64) {
	sm.mu.Lock()
	sm.maxOutput = n
	sm.mu.Unlock()
}

func (sm *SessionManager) Create(command string) *Session {
	id := newID()
	s := &Session{
		ID:        id,
		Command:   command,
		StartTime: time.Now(),
		Status:    StatusRunning,
	}
	sm.mu.Lock()
	if len(sm.sessions) >= maxSessions {
		// Evict the oldest completed session.
		var oldestID string
		var oldestTime time.Time
		for sid, sess := range sm.sessions {
			sess.mu.RLock()
			done := sess.Status != StatusRunning
			t := sess.StartTime
			sess.mu.RUnlock()
			if done && (oldestID == "" || t.Before(oldestTime)) {
				oldestID = sid
				oldestTime = t
			}
		}
		if oldestID != "" {
			delete(sm.sessions, oldestID)
		}
	}
	sm.sessions[id] = s
	sm.mu.Unlock()
	go sm.execute(s)
	return s
}

func (sm *SessionManager) Get(id string) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.sessions[id]
}

func (sm *SessionManager) List() []*Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	result := make([]*Session, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		result = append(result, s)
	}
	return result
}

func (sm *SessionManager) execute(s *Session) {
	sm.mu.RLock()
	execTimeout := sm.execTimeout
	maxOutput := sm.maxOutput
	sm.mu.RUnlock()

	ctx, cancel := context.WithCancel(context.Background())
	if execTimeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), execTimeout)
	}
	defer cancel()

	shell, flag := platformShell()
	cmd := exec.CommandContext(ctx, shell, flag, s.Command)

	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		s.mu.Lock()
		s.Status = StatusFailed
		s.Stderr = err.Error()
		now := time.Now()
		s.EndTime = &now
		s.mu.Unlock()
		s.broadcast(streamEvent{Type: "stderr", Data: err.Error() + "\n"})
		s.broadcast(streamEvent{Type: "done"})
		return
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		s.mu.Lock()
		s.Status = StatusFailed
		s.Stderr = err.Error()
		now := time.Now()
		s.EndTime = &now
		s.mu.Unlock()
		s.broadcast(streamEvent{Type: "stderr", Data: err.Error() + "\n"})
		s.broadcast(streamEvent{Type: "done"})
		return
	}

	if err := cmd.Start(); err != nil {
		s.mu.Lock()
		s.Status = StatusFailed
		s.Stderr = err.Error()
		now := time.Now()
		s.EndTime = &now
		s.mu.Unlock()
		s.broadcast(streamEvent{Type: "stderr", Data: err.Error() + "\n"})
		s.broadcast(streamEvent{Type: "done"})
		return
	}

	done := make(chan struct{}, 2)
	var outputBytes int64
	var outputLimitHit atomic.Bool

	scanPipe := func(r io.Reader, kind string, store *string) {
		defer func() { done <- struct{}{} }()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, scanBufSize), scanBufSize)
		for sc.Scan() {
			line := sc.Text() + "\n"

			s.mu.Lock()
			*store += line
			total := atomic.AddInt64(&outputBytes, int64(len(line)))
			s.mu.Unlock()

			if maxOutput > 0 && total > maxOutput && !outputLimitHit.Load() {
				outputLimitHit.Store(true)
				cancel()
			}

			s.broadcast(streamEvent{Type: kind, Data: line})

			if outputLimitHit.Load() {
				return
			}
		}
	}

	go scanPipe(outPipe, "stdout", &s.Stdout)
	go scanPipe(errPipe, "stderr", &s.Stderr)
	<-done
	<-done

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if outputLimitHit.Load() {
			msg := fmt.Sprintf("\n[ROVER] Output limit exceeded (max %d bytes)\n", maxOutput)
			s.mu.Lock()
			s.Status = StatusFailed
			s.Stderr += msg
			now := time.Now()
			s.EndTime = &now
			s.ExitCode = -1
			s.mu.Unlock()
			s.broadcast(streamEvent{Type: "stderr", Data: msg})
			s.broadcast(streamEvent{Type: "done", ExitCode: -1})
			return
		}
		if ctx.Err() == context.DeadlineExceeded {
			msg := fmt.Sprintf("\n[ROVER] Command timed out after %v\n", execTimeout)
			s.mu.Lock()
			s.Status = StatusFailed
			s.Stderr += msg
			now := time.Now()
			s.EndTime = &now
			s.ExitCode = -1
			s.mu.Unlock()
			s.broadcast(streamEvent{Type: "stderr", Data: msg})
			s.broadcast(streamEvent{Type: "done", ExitCode: -1})
			return
		}
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			s.mu.Lock()
			s.Status = StatusFailed
			s.Stderr += err.Error() + "\n"
			now := time.Now()
			s.EndTime = &now
			s.mu.Unlock()
			s.broadcast(streamEvent{Type: "stderr", Data: err.Error() + "\n"})
			s.broadcast(streamEvent{Type: "done"})
			return
		}
	}

	now := time.Now()
	s.mu.Lock()
	s.ExitCode = exitCode
	s.EndTime = &now
	if exitCode == 0 {
		s.Status = StatusCompleted
	} else {
		s.Status = StatusFailed
	}
	s.mu.Unlock()
	s.broadcast(streamEvent{Type: "done", ExitCode: exitCode})
	sm.persistAllToDisk()
}

type runtimeConfig struct {
	ExecTimeout time.Duration
	MaxOutput   int64
}

type Config struct {
	Addr         string
	Secret       string
	CertFile     string
	KeyFile      string
	ExecTimeout  time.Duration
	MaxOutput    int64
	ProjectsRoot string
	AllowCmds    []string // if non-empty, only commands with a matching prefix are permitted
	SessionsFile string   // path to sessions persistence file; empty = no persistence
	LogFormat    string   // "text" (default) or "json"
	// DisableCommandGuard turns off the built-in block on interactive / GUI /
	// stateful commands that can't work in rover's non-interactive shell.
	// Default (false) = guard enabled.
	DisableCommandGuard bool
}

type Server struct {
	cfg      Config
	logger   *slog.Logger
	mux      *http.ServeMux
	srv      *http.Server
	sessions *SessionManager
	launcher *launcher.Manager
	loginRL  *rateLimiter
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	w.Write(b)
}

// secretOK does a constant-time comparison of the raw secret (used only at login).
func (s *Server) secretOK(provided string) bool {
	if s.cfg.Secret == "" {
		return true
	}
	return hmac.Equal([]byte(provided), []byte(s.cfg.Secret))
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Secret == "" {
			next(w, r)
			return
		}
		// Accept header token for normal requests, or query param for EventSource
		// (EventSource cannot set custom headers).
		token := r.Header.Get("X-Rover-Secret")
		if token == "" {
			token = r.URL.Query().Get("secret")
		}
		if err := auth.VerifyToken(s.cfg.Secret, token); err != nil {
			jsonError(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func newLogger(format string) *slog.Logger {
	var h slog.Handler
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if format == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

func addSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

func New(cfg Config) *Server {
	logger := newLogger(cfg.LogFormat)
	sm := NewSessionManager()
	sm.execTimeout = cfg.ExecTimeout
	sm.maxOutput = cfg.MaxOutput
	sm.persistPath = cfg.SessionsFile
	sm.loadFromDisk()
	s := &Server{
		cfg:      cfg,
		logger:   logger,
		sessions: sm,
		loginRL:  newRateLimiter(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleWebUI)
	mux.HandleFunc("GET /", s.handleWebUI)
	mux.HandleFunc("GET /ping", s.handlePing)
	mux.HandleFunc("GET /api/auth", s.handleAuthStatus)
	mux.HandleFunc("POST /api/auth", s.handleLogin)
	mux.HandleFunc("GET /api/sessions", s.requireAuth(s.handleListSessions))
	mux.HandleFunc("POST /api/sessions", s.requireAuth(s.handleCreateSession))
	mux.HandleFunc("GET /api/sessions/{id}", s.requireAuth(s.handleGetSession))
	mux.HandleFunc("GET /api/sessions/{id}/stream", s.requireAuth(s.handleSessionStream))
	mux.HandleFunc("GET /api/config", s.requireAuth(s.handleGetConfig))
	mux.HandleFunc("PUT /api/config", s.requireAuth(s.handleUpdateConfig))

	if cfg.ProjectsRoot != "" {
		s.launcher = launcher.NewManager(cfg.ProjectsRoot)
		s.launcher.SetLogger(func(msg string, args ...any) {
			s.logger.Info(fmt.Sprintf(msg, args...))
		})
		// Project proxies inherit rover's own bind interface so they are never
		// reachable from a network rover itself is not exposed to.
		if host, _, err := net.SplitHostPort(cfg.Addr); err == nil {
			s.launcher.SetBindHost(host)
		}
		mux.HandleFunc("GET /api/projects", s.requireAuth(s.handleListProjects))
		mux.HandleFunc("POST /api/projects", s.requireAuth(s.handleAddProject))
		mux.HandleFunc("PUT /api/projects/{name}", s.requireAuth(s.handleUpdateProject))
		mux.HandleFunc("DELETE /api/projects/{name}", s.requireAuth(s.handleRemoveProject))
		mux.HandleFunc("POST /api/projects/{name}/start", s.requireAuth(s.handleStartProject))
		mux.HandleFunc("POST /api/projects/{name}/stop", s.requireAuth(s.handleStopProject))
		mux.HandleFunc("GET /api/projects/{name}/stream", s.requireAuth(s.handleProjectStream))
		mux.HandleFunc("GET /api/projects/dirs", s.requireAuth(s.handleListProjectDirs))
		mux.HandleFunc("GET /api/projects/{name}/files", s.requireAuth(s.handleListProjectFiles))
		mux.HandleFunc("PUT /api/projects/{name}/proxy", s.requireAuth(s.handleToggleProxy))
	}

	s.mux = mux

	s.srv = &http.Server{
		Addr:         cfg.Addr,
		Handler:      addSecurityHeaders(mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) ListenAndServe() error {
	useTLS := s.cfg.CertFile != "" && s.cfg.KeyFile != ""
	if !useTLS {
		s.logger.Warn("TLS not configured, traffic is plaintext")
	}
	s.logger.Info("listening", "addr", s.cfg.Addr, "tls", useTLS, "version", version.Build)
	if len(s.cfg.AllowCmds) > 0 {
		s.logger.Info("command allowlist active", "prefixes", s.cfg.AllowCmds)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		s.logger.Info("shutting down")
		if s.launcher != nil {
			s.logger.Info("stopping all launched projects")
			s.launcher.StopAll()
		}
		s.sessions.persistAllToDisk()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()

	var err error
	if useTLS {
		s.srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		err = s.srv.ListenAndServeTLS(s.cfg.CertFile, s.cfg.KeyFile)
	} else {
		err = s.srv.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleWebUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(webUI)
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.logger.Info("ping", "src", ip)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}{Status: "ok", Version: version.Build})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions := s.sessions.List()
	summaries := make([]struct {
		ID        string        `json:"id"`
		Command   string        `json:"command"`
		Status    SessionStatus `json:"status"`
		StartTime time.Time     `json:"start_time"`
		EndTime   *time.Time    `json:"end_time,omitempty"`
		ExitCode  int           `json:"exit_code"`
	}, len(sessions))
	for i, sess := range sessions {
		sess.mu.RLock()
		summaries[i] = struct {
			ID        string        `json:"id"`
			Command   string        `json:"command"`
			Status    SessionStatus `json:"status"`
			StartTime time.Time     `json:"start_time"`
			EndTime   *time.Time    `json:"end_time,omitempty"`
			ExitCode  int           `json:"exit_code"`
		}{
			ID: sess.ID, Command: sess.Command, Status: sess.Status,
			StartTime: sess.StartTime, EndTime: sess.EndTime, ExitCode: sess.ExitCode,
		}
		sess.mu.RUnlock()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(summaries)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCommandBytes+512))
	if err != nil {
		jsonError(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var req struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Command == "" {
		jsonError(w, "command is required", http.StatusBadRequest)
		return
	}
	if len(req.Command) > maxCommandBytes {
		jsonError(w, "command exceeds 10 KB limit", http.StatusRequestEntityTooLarge)
		return
	}

	if len(s.cfg.AllowCmds) > 0 {
		allowed := false
		for _, prefix := range s.cfg.AllowCmds {
			if strings.HasPrefix(req.Command, prefix) {
				allowed = true
				break
			}
		}
		if !allowed {
			s.logger.Warn("command blocked by allowlist", "src", r.RemoteAddr, "cmd", req.Command)
			jsonError(w, "command not permitted by server allowlist", http.StatusForbidden)
			return
		}
	}

	if !s.cfg.DisableCommandGuard {
		if blocked, reason := commandGuard(req.Command); blocked {
			s.logger.Warn("command blocked by guard", "src", r.RemoteAddr, "cmd", req.Command)
			jsonError(w, "blocked: "+reason, http.StatusUnprocessableEntity)
			return
		}
	}

	s.logger.Info("exec", "src", r.RemoteAddr, "cmd", req.Command)
	sess := s.sessions.Create(req.Command)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Message string `json:"message"`
	}{
		ID:      sess.ID,
		Status:  "running",
		Message: "session created",
	})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.sessions.Get(id)
	if sess == nil {
		jsonError(w, "session not found", http.StatusNotFound)
		return
	}

	sess.mu.RLock()
	detail := struct {
		ID        string        `json:"id"`
		Command   string        `json:"command"`
		Status    SessionStatus `json:"status"`
		StartTime time.Time     `json:"start_time"`
		EndTime   *time.Time    `json:"end_time,omitempty"`
		ExitCode  int           `json:"exit_code"`
		Stdout    string        `json:"stdout"`
		Stderr    string        `json:"stderr"`
	}{
		ID: sess.ID, Command: sess.Command, Status: sess.Status,
		StartTime: sess.StartTime, EndTime: sess.EndTime, ExitCode: sess.ExitCode,
		Stdout: sess.Stdout, Stderr: sess.Stderr,
	}
	sess.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(detail)
}

func (s *Server) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := s.sessions.Get(id)
	if sess == nil {
		jsonError(w, "session not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Disable the write deadline for long-lived SSE streams.
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Replay existing output
	sess.mu.RLock()
	existingStdout := sess.Stdout
	existingStderr := sess.Stderr
	done := sess.Status != StatusRunning
	sess.mu.RUnlock()

	if existingStdout != "" {
		fmt.Fprintf(w, "data: %s\n\n", toJSON(streamEvent{Type: "stdout", Data: existingStdout}))
		flusher.Flush()
	}
	if existingStderr != "" {
		fmt.Fprintf(w, "data: %s\n\n", toJSON(streamEvent{Type: "stderr", Data: existingStderr}))
		flusher.Flush()
	}
	if done {
		sess.mu.RLock()
		ec := sess.ExitCode
		sess.mu.RUnlock()
		fmt.Fprintf(w, "data: %s\n\n", toJSON(streamEvent{Type: "done", ExitCode: ec}))
		flusher.Flush()
		return
	}

	ch := sess.subscribe()
	defer sess.unsubscribe(ch)

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data := toJSON(ev)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			if ev.Type == "done" {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects := s.launcher.Scan()
	running := s.launcher.ListRunning()

	type projectView struct {
		launcher.ProjectInfo
		IsRunning bool   `json:"is_running"`
		URL       string `json:"running_url,omitempty"`
		StartedAt string `json:"started_at,omitempty"`
		ProxyURL  string `json:"proxy_url,omitempty"`
	}

	runMap := make(map[string]launcher.RunningProject)
	for _, rp := range running {
		runMap[rp.Name] = rp
	}

	views := make([]projectView, 0, len(projects))
	for _, p := range projects {
		v := projectView{ProjectInfo: p}
		if rp, ok := runMap[p.Name]; ok {
			v.IsRunning = true
			v.URL = rp.URL
			v.Port = rp.Port
			v.ProjectInfo.URL = rp.URL
			v.StartedAt = rp.StartTime.Format(time.RFC3339)
			if p.ProxyEnabled && rp.ProxyURL != "" {
				v.ProxyURL = rp.ProxyURL
			}
		}
		views = append(views, v)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(views)
}

func (s *Server) handleStartProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	// Optional one-off port override for this start only (e.g. when the default
	// port is occupied). An empty body means "use the registered default".
	var body struct {
		Port int `json:"port"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 256)).Decode(&body)
	}

	if err := s.launcher.Start(name, body.Port); err != nil {
		if errors.Is(err, launcher.ErrPortInUse) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{
				"error":       err.Error(),
				"port_in_use": true,
			})
			return
		}
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "starting", "name": name})
}

func (s *Server) handleStopProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.launcher.Stop(name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stopped", "name": name})
}

func (s *Server) handleProjectStream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	ch, err := s.launcher.Subscribe(name)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	defer s.launcher.Unsubscribe(name, ch)

	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Disable the write deadline for long-lived SSE streams.
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			if ev.Type == "done" {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleAddProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		StartCmd string `json:"start_cmd"`
		Port     int    `json:"port"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxAddProjectBody)).Decode(&body); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Name == "" || body.StartCmd == "" {
		jsonError(w, "name and start_cmd are required", http.StatusBadRequest)
		return
	}
	if body.Port <= 0 {
		jsonError(w, "a valid port is required", http.StatusBadRequest)
		return
	}

	p, err := s.launcher.AddProject(body.Name, body.StartCmd, body.Port)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(p)
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Port     int    `json:"port"`
		StartCmd string `json:"start_cmd"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 512)).Decode(&body); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	switch {
	case body.Port > 0 && body.StartCmd != "":
		if _, err := s.launcher.UpdateProjectPort(name, body.Port); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		p, err := s.launcher.UpdateProjectCommand(name, body.StartCmd)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p)
	case body.Port > 0:
		p, err := s.launcher.UpdateProjectPort(name, body.Port)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p)
	case body.StartCmd != "":
		p, err := s.launcher.UpdateProjectCommand(name, body.StartCmd)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p)
	default:
		jsonError(w, "provide port (>0) and/or start_cmd to update", http.StatusBadRequest)
	}
}

func (s *Server) handleRemoveProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.launcher.RemoveProject(name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "removed"})
}

func (s *Server) handleListProjectDirs(w http.ResponseWriter, r *http.Request) {
	dirs := s.launcher.ListProjectDirs()
	if dirs == nil {
		dirs = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(dirs)
}

func (s *Server) handleListProjectFiles(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	files := s.launcher.ListEligibleFiles(name)
	if files == nil {
		files = []launcher.FileInfo{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

func (s *Server) handleToggleProxy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256)).Decode(&body); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	p, err := s.launcher.SetProxyEnabled(name, body.Enabled)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(p)
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func platformShell() (shell, flag string) {
	if runtime.GOOS == "windows" {
		return "cmd", "/C"
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh, "-c"
	}
	return "sh", "-c"
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"exec_timeout_seconds": s.sessions.GetExecTimeout().Seconds(),
		"max_output_bytes":     s.sessions.GetMaxOutput(),
	})
}

func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024))
	if err != nil {
		jsonError(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var req struct {
		ExecTimeoutSeconds *float64 `json:"exec_timeout_seconds"`
		MaxOutputBytes     *int64   `json:"max_output_bytes"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if req.ExecTimeoutSeconds != nil {
		d := time.Duration(*req.ExecTimeoutSeconds * float64(time.Second))
		if d < 0 {
			jsonError(w, "exec_timeout_seconds must be >= 0", http.StatusBadRequest)
			return
		}
		s.sessions.SetExecTimeout(d)
	}
	if req.MaxOutputBytes != nil {
		if *req.MaxOutputBytes < 0 {
			jsonError(w, "max_output_bytes must be >= 0", http.StatusBadRequest)
			return
		}
		s.sessions.SetMaxOutput(*req.MaxOutputBytes)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"exec_timeout_seconds": s.sessions.GetExecTimeout().Seconds(),
		"max_output_bytes":     s.sessions.GetMaxOutput(),
	})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"required": s.cfg.Secret != ""})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Secret == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": "", "expires_at": ""})
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !s.loginRL.allow(ip) {
		jsonError(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	var req struct {
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 512)).Decode(&req); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if !s.secretOK(req.Secret) {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		s.logger.Warn("login failed", "src", ip)
		jsonError(w, "invalid secret", http.StatusUnauthorized)
		return
	}
	token, err := auth.IssueToken(s.cfg.Secret)
	if err != nil {
		jsonError(w, "failed to issue token", http.StatusInternalServerError)
		return
	}
	ip2, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.logger.Info("login", "src", ip2)
	expiresAt := time.Now().Add(auth.TokenTTL).UTC().Format(time.RFC3339)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": token, "expires_at": expiresAt})
}

var webUI = []byte(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1.0,viewport-fit=cover">
<meta name="theme-color" content="#0a0d12">
<title>Rover</title>
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{
--bg:#0a0d12;--bg-1:#0f141c;--bg-2:#141c26;--bg-3:#1b2532;
--border:rgba(255,255,255,.07);--border-2:rgba(255,255,255,.12);--border-focus:#6366f1;
--text:#e7edf5;--text-dim:#94a3b8;--text-faint:#5f7186;
--accent:#6366f1;--accent-2:#8b5cf6;--accent-soft:rgba(99,102,241,.15);--accent-line:rgba(99,102,241,.4);
--green:#22c55e;--green-soft:rgba(34,197,94,.14);--green-hover:#16a34a;
--red:#f0555f;--red-soft:rgba(240,85,95,.14);--red-hover:#e0343f;
--amber:#f5b942;--amber-soft:rgba(245,185,66,.14);--blue:#60a5fa;
--mono:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,'Liberation Mono',monospace;
--sans:-apple-system,BlinkMacSystemFont,'Segoe UI',Inter,Roboto,Helvetica,Arial,sans-serif;
--shadow-sm:0 1px 2px rgba(0,0,0,.4);
--shadow-md:0 4px 16px rgba(0,0,0,.35);
--shadow-lg:0 18px 50px rgba(0,0,0,.55);
--r-sm:7px;--r-md:10px;--r-lg:14px;
}
html,body{height:100%}
body{
background:var(--bg);color:var(--text);font-family:var(--sans);
display:flex;flex-direction:column;height:100dvh;line-height:1.5;
-webkit-font-smoothing:antialiased;text-rendering:optimizeLegibility;overflow:hidden;
background-image:radial-gradient(1100px 520px at 50% -280px,rgba(99,102,241,.14),transparent 70%);
}
::selection{background:var(--accent-soft)}
/* thin scrollbars */
*{scrollbar-width:thin;scrollbar-color:rgba(255,255,255,.14) transparent}
*::-webkit-scrollbar{width:9px;height:9px}
*::-webkit-scrollbar-thumb{background:rgba(255,255,255,.13);border-radius:20px;border:2px solid transparent;background-clip:padding-box}
*::-webkit-scrollbar-thumb:hover{background:rgba(255,255,255,.22);background-clip:padding-box}

/* ── Buttons (base system) ───────────────────────────── */
.btn,button{font-family:inherit}
.btn{display:inline-flex;align-items:center;justify-content:center;gap:6px;cursor:pointer;
border:1px solid transparent;border-radius:var(--r-sm);font-size:12.5px;font-weight:600;
padding:7px 14px;transition:background .14s,border-color .14s,color .14s,transform .06s,box-shadow .14s;
white-space:nowrap;-webkit-tap-highlight-color:transparent;background:none;color:var(--text)}
.btn:active{transform:translateY(.5px)}
.btn-primary{background:var(--green);color:#04120a;font-weight:700}
.btn-primary:hover:not(:disabled){background:var(--green-hover);color:#fff}
.btn-accent{background:var(--accent);color:#fff}
.btn-accent:hover:not(:disabled){background:#4f52e6}
.btn-danger{background:var(--red);color:#fff}
.btn-danger:hover:not(:disabled){background:var(--red-hover)}
.btn-ghost{background:rgba(255,255,255,.02);border-color:var(--border-2);color:var(--text-dim)}
.btn-ghost:hover:not(:disabled){border-color:var(--accent-line);color:var(--text);background:rgba(99,102,241,.08)}
.btn:disabled{opacity:.4;cursor:default;transform:none}

/* ── Header ──────────────────────────────────────────── */
header{background:rgba(15,20,28,.82);backdrop-filter:blur(14px) saturate(1.4);-webkit-backdrop-filter:blur(14px) saturate(1.4);
border-bottom:1px solid var(--border);padding:11px 18px;display:flex;align-items:center;gap:12px;flex-shrink:0;z-index:20}
.brand{display:flex;align-items:center;gap:9px}
header .logo{width:30px;height:30px;border-radius:9px;background:linear-gradient(140deg,#6366f1,#8b5cf6 60%,#a855f7);
display:flex;align-items:center;justify-content:center;font-size:15px;color:#fff;flex-shrink:0;font-weight:800;
box-shadow:0 4px 14px rgba(99,102,241,.45),inset 0 1px 0 rgba(255,255,255,.35)}
header h1{font-size:16px;font-weight:750;letter-spacing:-.4px}
/* segmented tab control */
.tab-bar{display:flex;gap:3px;margin-left:6px;margin-right:auto;background:rgba(255,255,255,.035);
border:1px solid var(--border);padding:3px;border-radius:9px}
.tab-btn{background:0 0;border:none;color:var(--text-dim);cursor:pointer;padding:5px 15px;font-size:12.5px;
font-weight:650;border-radius:6px;transition:all .14s;position:relative}
.tab-btn:hover{color:var(--text)}
.tab-btn.active{color:#fff;background:linear-gradient(180deg,rgba(99,102,241,.9),rgba(99,102,241,.72));box-shadow:var(--shadow-sm)}
#settingsBtn{width:34px;height:34px;display:flex;align-items:center;justify-content:center;background:rgba(255,255,255,.02);
border:1px solid var(--border-2);color:var(--text-dim);cursor:pointer;font-size:17px;border-radius:8px;transition:all .14s;line-height:1;padding:0}
#settingsBtn:hover{color:var(--text);border-color:var(--accent-line);background:rgba(99,102,241,.08);transform:rotate(35deg)}

/* ── Login ───────────────────────────────────────────── */
#loginOverlay{display:none;position:fixed;inset:0;background:var(--bg);z-index:200;align-items:center;justify-content:center;flex-direction:column;
background-image:radial-gradient(760px 460px at 50% 32%,rgba(99,102,241,.2),transparent 70%)}
#loginOverlay.show{display:flex;animation:fadeIn .3s ease-out}
#loginBox{background:linear-gradient(180deg,rgba(20,28,38,.95),rgba(15,20,28,.95));border:1px solid var(--border-2);
border-radius:18px;padding:36px 32px;width:90%;max-width:372px;text-align:center;box-shadow:var(--shadow-lg)}
#loginBox .logo{width:56px;height:56px;border-radius:15px;background:linear-gradient(140deg,#6366f1,#8b5cf6 60%,#a855f7);
display:flex;align-items:center;justify-content:center;font-size:27px;color:#fff;font-weight:800;margin:0 auto 18px;
box-shadow:0 10px 30px rgba(99,102,241,.5),inset 0 1px 0 rgba(255,255,255,.35)}
#loginBox h2{font-size:21px;font-weight:750;letter-spacing:-.4px}
#loginBox p{font-size:13.5px;color:var(--text-dim);margin:5px 0 22px}
.field-wrap{position:relative}
#loginBox input{width:100%;background:var(--bg);border:1px solid var(--border-2);border-radius:9px;color:var(--text);
padding:12px 15px;font-size:14px;font-family:var(--mono);outline:none;transition:border-color .14s,box-shadow .14s}
#loginBox input:focus{border-color:var(--border-focus);box-shadow:0 0 0 3px var(--accent-soft)}
#loginBox button{width:100%;margin-top:14px;padding:12px;font-size:14px;border-radius:9px}
#loginError{font-size:12.5px;color:var(--red);margin-top:12px;display:none}
#loginError.show{display:block}

/* ── Views ───────────────────────────────────────────── */
#terminalView{display:flex;flex-direction:column;flex:1;min-height:0}
#projectsView{display:none;flex-direction:column;flex:1;min-height:0;padding:20px 18px 28px;overflow-y:auto}
#history{flex:1;overflow-y:auto;padding:22px 18px;display:flex;flex-direction:column;gap:4px;scroll-behavior:smooth}
#history:empty::after{content:'';display:none}
.empty-state{margin:auto;text-align:center;color:var(--text-faint);padding:40px 20px;max-width:420px}
.empty-state .es-icon{width:56px;height:56px;margin:0 auto 16px;border-radius:15px;background:var(--bg-2);border:1px solid var(--border);
display:flex;align-items:center;justify-content:center;font-size:26px;opacity:.9}
.empty-state h3{font-size:15px;font-weight:700;color:var(--text-dim);margin-bottom:6px}
.empty-state p{font-size:13px;line-height:1.6}
.empty-state code{font-family:var(--mono);background:var(--bg-2);border:1px solid var(--border);border-radius:5px;padding:1px 6px;color:var(--text-dim);font-size:12px}

/* ── Chat bubbles ────────────────────────────────────── */
.chat-entry{animation:fadeIn .22s ease-out;margin:0 auto 14px;width:100%;max-width:880px}
@keyframes fadeIn{from{opacity:0;transform:translateY(7px)}to{opacity:1;transform:translateY(0)}}
.msg-command{display:flex;justify-content:flex-end;padding-left:44px}
.msg-command .bubble{background:linear-gradient(180deg,var(--accent),#5457e6);color:#fff;padding:9px 15px;
border-radius:14px 14px 5px 14px;max-width:82%;word-break:break-word;overflow-wrap:anywhere;
font-family:var(--mono);font-size:13px;line-height:1.55;box-shadow:0 3px 12px rgba(99,102,241,.28)}
.msg-command .time{font-size:10.5px;color:var(--text-faint);text-align:right;margin:3px 4px 0 0}
.msg-response{display:flex;justify-content:flex-start;padding-right:44px;margin-top:5px}
.msg-response .bubble{background:var(--bg-2);border:1px solid var(--border);color:var(--text);padding:10px 15px;
border-radius:14px 14px 14px 5px;max-width:88%;word-break:break-word;overflow-wrap:anywhere;
font-family:var(--mono);font-size:13px;line-height:1.55;white-space:pre-wrap;box-shadow:var(--shadow-sm)}
.msg-response .bubble:empty::after{content:'running…';color:var(--text-faint);font-style:italic}
.resp-stdout{color:#d6dee8}
.resp-stderr{color:var(--red)}
.resp-stdout:empty,.resp-stderr:empty{display:none}
.exit-code{display:inline-flex;align-items:center;gap:5px;font-size:11px;margin-top:9px;padding:3px 9px;
border-radius:20px;font-weight:700;font-family:var(--sans);letter-spacing:.2px}
.exit-code::before{content:'';width:6px;height:6px;border-radius:50%}
.exit-ok{color:#7ee2a4;background:var(--green-soft)}
.exit-ok::before{background:var(--green)}
.exit-err{color:#ff9ba1;background:var(--red-soft)}
.exit-err::before{background:var(--red)}

/* ── Input bar ───────────────────────────────────────── */
.input-shell{border-top:1px solid var(--border);background:rgba(15,20,28,.85);backdrop-filter:blur(12px);-webkit-backdrop-filter:blur(12px);flex-shrink:0}
.input-bar{padding:12px 18px;display:flex;gap:10px;align-items:center;max-width:916px;margin:0 auto;padding-bottom:max(12px,env(safe-area-inset-bottom))}
#cmd{flex:1;background:var(--bg);border:1px solid var(--border-2);border-radius:10px;color:var(--text);
padding:11px 15px;font-size:14px;font-family:var(--mono);outline:none;transition:border-color .14s,box-shadow .14s}
#cmd:focus{border-color:var(--border-focus);box-shadow:0 0 0 3px var(--accent-soft)}
#cmd::placeholder{color:var(--text-faint)}
#runBtn{border-radius:10px;padding:11px 22px;font-size:14px;flex-shrink:0}
#runBtn .spinner{display:none;width:14px;height:14px;border:2px solid rgba(255,255,255,.35);border-top-color:#fff;border-radius:50%;animation:spin .6s linear infinite}
#runBtn.loading .spinner{display:inline-block}
#runBtn.loading .label{display:none}
@keyframes spin{to{transform:rotate(360deg)}}
.cmd-hint{display:none;font-size:12.5px;line-height:1.5;max-width:916px;margin:0 auto;padding:9px 18px 0}
.cmd-hint.show{display:block}
.cmd-hint .hint-inner{display:flex;gap:8px;align-items:flex-start;padding:9px 13px;border-radius:9px;border:1px solid}
.cmd-hint .hint-ico{flex-shrink:0;font-size:13px;line-height:1.4}
.cmd-hint.lv-err .hint-inner{color:#ffb4b9;background:var(--red-soft);border-color:rgba(240,85,95,.3)}
.cmd-hint.lv-warn .hint-inner{color:#ffd894;background:var(--amber-soft);border-color:rgba(245,185,66,.3)}
.cmd-hint.lv-info .hint-inner{color:var(--text-dim);background:rgba(255,255,255,.03);border-color:var(--border-2)}

/* ── Modal ───────────────────────────────────────────── */
.modal-overlay{display:none;position:fixed;inset:0;background:rgba(4,7,11,.66);backdrop-filter:blur(3px);-webkit-backdrop-filter:blur(3px);
z-index:100;align-items:center;justify-content:center;padding:18px}
.modal-overlay.show{display:flex;animation:fadeIn .18s ease-out}
.modal{background:linear-gradient(180deg,var(--bg-2),var(--bg-1));border:1px solid var(--border-2);border-radius:16px;
padding:24px;width:100%;max-width:420px;box-shadow:var(--shadow-lg);animation:pop .2s cubic-bezier(.2,.8,.3,1.2)}
@keyframes pop{from{opacity:0;transform:scale(.96) translateY(6px)}to{opacity:1;transform:none}}
.modal h2{font-size:16px;margin-bottom:4px;font-weight:750;letter-spacing:-.3px}
.modal .modal-sub{font-size:12.5px;color:var(--text-dim);margin-bottom:18px}
.modal-field{margin-bottom:15px}
.modal-field label{display:block;font-size:12px;color:var(--text-dim);margin-bottom:6px;font-weight:600}
.modal-field input{width:100%;background:var(--bg);border:1px solid var(--border-2);border-radius:8px;color:var(--text);
padding:10px 13px;font-size:13.5px;font-family:var(--mono);outline:none;transition:border-color .14s,box-shadow .14s}
.modal-field input:focus{border-color:var(--border-focus);box-shadow:0 0 0 3px var(--accent-soft)}
.modal-field .field-note{font-size:11px;color:var(--text-faint);margin-top:5px;line-height:1.4}
.modal-actions{display:flex;gap:9px;justify-content:flex-end;margin-top:22px}
.modal-actions .btn{padding:9px 18px}
.modal-success{color:#7ee2a4;font-size:12.5px;margin-top:12px;text-align:center;display:none}
.modal-error{color:var(--red);font-size:12.5px;margin-top:12px;text-align:center;display:none}
#modalMsgText{font-size:13.5px;color:var(--text-dim);line-height:1.55;margin-bottom:2px}

/* ── Projects grid + cards ───────────────────────────── */
.projects-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(330px,1fr));gap:14px;width:100%;max-width:1240px;margin:0 auto}
.project-card{background:linear-gradient(180deg,var(--bg-2),var(--bg-1));border:1px solid var(--border);border-radius:var(--r-lg);
padding:17px 18px;transition:border-color .16s,box-shadow .16s,transform .16s;box-shadow:var(--shadow-sm)}
.project-card:hover{border-color:var(--border-2);box-shadow:var(--shadow-md);transform:translateY(-1px)}
.project-card-header{display:flex;align-items:center;gap:9px;margin-bottom:12px}
.project-name{font-size:15px;font-weight:700;flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;letter-spacing:-.2px}
.status-pill{display:inline-flex;align-items:center;gap:6px;font-size:10.5px;font-weight:700;text-transform:uppercase;
letter-spacing:.5px;padding:3px 9px 3px 8px;border-radius:20px;border:1px solid transparent}
.status-pill .status-dot{width:7px;height:7px;border-radius:50%;flex-shrink:0}
.status-pill.stopped{color:var(--text-faint);background:rgba(255,255,255,.04);border-color:var(--border)}
.status-pill.stopped .status-dot{background:var(--text-faint)}
.status-pill.starting{color:var(--amber);background:var(--amber-soft)}
.status-pill.starting .status-dot{background:var(--amber);animation:pulse 1s ease-in-out infinite}
.status-pill.running{color:#7ee2a4;background:var(--green-soft)}
.status-pill.running .status-dot{background:var(--green);box-shadow:0 0 0 3px rgba(34,197,94,.2)}
.status-pill.failed{color:#ff9ba1;background:var(--red-soft)}
.status-pill.failed .status-dot{background:var(--red)}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.35}}
.btn-remove{margin-left:2px;width:26px;height:26px;padding:0;flex-shrink:0;border-radius:7px;font-size:15px;line-height:1;
background:none;border:1px solid transparent;color:var(--text-faint);cursor:pointer;transition:all .14s}
.btn-remove:hover{background:var(--red-soft);color:var(--red);border-color:rgba(240,85,95,.3)}
.project-meta{display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin-bottom:12px}
.meta-chip{display:inline-flex;align-items:center;gap:5px;font-size:11.5px;color:var(--text-dim);font-family:var(--mono);
background:rgba(255,255,255,.035);border:1px solid var(--border);border-radius:6px;padding:3px 9px}
.meta-chip b{color:var(--text);font-weight:600}
.project-url{display:inline-flex;align-items:center;gap:5px;font-size:11.5px;color:var(--blue);text-decoration:none;font-family:var(--mono);
background:rgba(96,165,250,.1);border:1px solid rgba(96,165,250,.22);border-radius:6px;padding:3px 9px;max-width:100%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.project-url:hover{background:rgba(96,165,250,.18)}
.project-url.proxy{color:#7ee2a4;background:var(--green-soft);border-color:rgba(34,197,94,.24)}
.project-actions{display:flex;gap:6px;flex-wrap:wrap;align-items:center}
.project-actions .btn{padding:6px 12px;font-size:11.5px}
.project-actions .spacer{flex:1}
.btn-proxy-on{background:var(--green-soft);border:1px solid rgba(34,197,94,.3);color:#7ee2a4}
.btn-proxy-on:hover{background:rgba(34,197,94,.22)}
.btn-proxy-off{background:rgba(255,255,255,.02);border:1px solid var(--border-2);color:var(--text-faint)}
.btn-proxy-off:hover{border-color:var(--accent-line);color:var(--text-dim)}
.project-desc{font-size:12px;color:var(--text-dim);margin-top:12px;line-height:1.5}
.project-start-cmd{font-size:11px;color:var(--text-faint);font-family:var(--mono);margin-top:7px;
background:var(--bg);border:1px solid var(--border);border-radius:6px;padding:6px 9px;overflow-x:auto;white-space:nowrap}
.project-start-cmd::before{content:'$ ';color:var(--accent);opacity:.8}
.project-console{display:none;margin-top:13px;border-top:1px solid var(--border);padding-top:12px}
.project-console.open{display:block;animation:fadeIn .18s ease-out}
.project-console-header{display:flex;justify-content:space-between;align-items:center;margin-bottom:7px}
.project-console-header span{font-size:10.5px;color:var(--text-faint);font-weight:700;text-transform:uppercase;letter-spacing:.5px}
.project-console-output{background:#070a0e;border:1px solid var(--border);border-radius:8px;padding:10px;font-family:var(--mono);
font-size:11.5px;line-height:1.5;max-height:220px;overflow-y:auto;white-space:pre-wrap;word-break:break-all;color:#c6d0dc}
.project-console-output:empty::after{content:'Waiting for output…';color:var(--text-faint);font-style:italic}
.project-console-output .url-line{color:var(--blue);font-weight:600}
.projects-empty{grid-column:1/-1;color:var(--text-faint);text-align:center;padding:70px 20px;font-size:14px}
.projects-empty .es-icon{width:56px;height:56px;margin:0 auto 16px;border-radius:15px;background:var(--bg-2);border:1px solid var(--border);
display:flex;align-items:center;justify-content:center;font-size:26px}
.projects-empty h3{font-size:15px;color:var(--text-dim);font-weight:700;margin-bottom:6px}
.projects-empty p{font-size:13px}

/* ── Add-project picker ──────────────────────────────── */
.picker-layout{display:flex;gap:14px;min-height:250px}
.picker-col{flex:1;border:1px solid var(--border-2);border-radius:10px;overflow:hidden;display:flex;flex-direction:column;background:var(--bg)}
.picker-col-header{padding:9px 12px;font-size:10.5px;font-weight:700;color:var(--text-faint);text-transform:uppercase;letter-spacing:.5px;
background:rgba(255,255,255,.02);border-bottom:1px solid var(--border)}
.picker-list{flex:1;overflow-y:auto;max-height:230px}
.picker-item{padding:9px 12px;font-size:12px;cursor:pointer;transition:background .1s;border-bottom:1px solid var(--border);font-family:var(--mono);color:var(--text-dim)}
.picker-item:last-child{border-bottom:none}
.picker-item:hover{background:var(--accent-soft);color:var(--text)}
.picker-item.selected{background:var(--accent-soft);color:#c7ccff;font-weight:600;box-shadow:inset 2px 0 0 var(--accent)}
.picker-item .sub{font-size:10.5px;color:var(--text-faint);font-weight:400;margin-left:6px}
#addProjectDetails{margin-top:14px;padding:15px;background:var(--bg);border-radius:10px;border:1px solid var(--border)}
#addProjectStatus{margin-top:10px;font-size:12.5px}
#addProjectStatus.success{color:#7ee2a4}
#addProjectStatus.error{color:var(--red)}
#addProjectStatus.loading{color:var(--amber)}

/* ── Toasts ──────────────────────────────────────────── */
#toastWrap{position:fixed;bottom:0;right:0;z-index:400;padding:16px 16px calc(84px + env(safe-area-inset-bottom));display:flex;flex-direction:column;gap:9px;align-items:flex-end;pointer-events:none}
.toast{pointer-events:auto;display:flex;align-items:flex-start;gap:10px;min-width:230px;max-width:360px;
background:linear-gradient(180deg,var(--bg-3),var(--bg-2));border:1px solid var(--border-2);border-left:3px solid var(--accent);
border-radius:10px;padding:11px 14px;box-shadow:var(--shadow-lg);font-size:13px;color:var(--text);
animation:toastIn .24s cubic-bezier(.2,.8,.3,1.2)}
.toast.out{animation:toastOut .2s ease-in forwards}
.toast.err{border-left-color:var(--red)}
.toast.ok{border-left-color:var(--green)}
.toast .t-ico{flex-shrink:0;font-size:14px;line-height:1.4}
.toast .t-body{flex:1;line-height:1.45;word-break:break-word}
@keyframes toastIn{from{opacity:0;transform:translateX(24px)}to{opacity:1;transform:none}}
@keyframes toastOut{to{opacity:0;transform:translateX(24px)}}

@media(max-width:600px){
header{padding:9px 11px;gap:8px}header h1{font-size:14px}
.tab-btn{font-size:12px;padding:5px 11px}
#history{padding:14px 11px}.msg-command{padding-left:22px}.msg-response{padding-right:22px}
.input-bar{padding:10px 11px;gap:7px}#cmd{font-size:13.5px}#runBtn{padding:11px 16px}
.cmd-hint{padding:8px 11px 0}
.projects-grid{grid-template-columns:1fr}#projectsView{padding:14px 11px 22px}
.modal{padding:20px}
}
</style>
</head>
<body>
<header>
<div class="brand"><div class="logo">R</div><h1>Rover</h1></div>
<div class="tab-bar">
<button class="tab-btn active" data-tab="terminal">Terminal</button>
<button class="tab-btn" data-tab="projects">Projects</button>
</div>
<button id="newChatBtn" class="btn btn-ghost">Clear</button>
<button id="addProjectBtn" class="btn btn-accent" style="display:none">+ Add Project</button>
<button id="refreshProjectsBtn" class="btn btn-ghost" style="display:none">Refresh</button>
<button id="settingsBtn" title="Settings">&#x2699;</button>
</header>
<div id="loginOverlay">
<div id="loginBox">
<div class="logo">R</div>
<h2>Rover</h2>
<p>Enter the server secret to continue</p>
<div class="field-wrap"><input type="password" id="loginSecret" placeholder="Secret" autocomplete="off"></div>
<button id="loginBtn" class="btn btn-primary">Login</button>
<div id="loginError">Invalid secret. Try again.</div>
</div>
</div>
<div class="modal-overlay" id="settingsModal">
<div class="modal">
<h2>Settings</h2>
<div class="modal-sub">Limits apply to new Terminal commands.</div>
<div class="modal-field">
<label for="modalTimeout">Execution timeout</label>
<input type="number" id="modalTimeout" min="0" step="1">
<div class="field-note">Seconds. 0 = no limit.</div>
</div>
<div class="modal-field">
<label for="modalMaxOutput">Max output</label>
<input type="number" id="modalMaxOutput" min="0" step="1">
<div class="field-note">Bytes. 0 = no limit.</div>
</div>
<div class="modal-actions">
<button id="modalCancel" class="btn btn-ghost">Cancel</button>
<button id="modalSave" class="btn btn-primary">Save</button>
</div>
<div id="modalSuccess" class="modal-success">Settings saved</div>
<div id="modalError" class="modal-error"></div>
</div>
</div>
<div id="terminalView">
<div id="history"></div>
<div id="cmdHint" class="cmd-hint"></div>
<div class="input-shell">
<div class="input-bar">
<input type="text" id="cmd" placeholder="Type a command…" autofocus spellcheck="false" autocomplete="off">
<button id="runBtn" class="btn btn-primary"><span class="spinner"></span><span class="label">Run</span></button>
</div>
</div>
</div>
<div id="projectsView"></div>
<div class="modal-overlay" id="addProjectModal">
<div class="modal" style="max-width:660px">
<h2>Add Project</h2>
<div class="modal-sub">Pick a directory and its start file — rover validates by launching it briefly.</div>
<div class="picker-layout">
<div class="picker-col">
<div class="picker-col-header">Directory</div>
<div class="picker-list" id="dirList"></div>
</div>
<div class="picker-col">
<div class="picker-col-header">Start file</div>
<div class="picker-list" id="fileList"></div>
</div>
</div>
<div id="addProjectDetails" style="display:none">
<div class="modal-field">
<label>Start command</label>
<input type="text" id="addStartCmd" placeholder="e.g. python server.py --host 0.0.0.0">
<div class="field-note">Edit to add extra CLI flags.</div>
</div>
<div class="modal-field">
<label>Port</label>
<input type="number" id="addPort" min="1" max="65535" placeholder="e.g. 8770">
<div class="field-note">Rover passes this to the app as --port.</div>
</div>
<div id="addProjectStatus"></div>
</div>
<div class="modal-actions">
<button id="addProjectCancel" class="btn btn-ghost">Cancel</button>
<button id="addProjectConfirm" style="display:none" class="btn btn-primary">Validate &amp; Add</button>
</div>
</div>
</div>
<div class="modal-overlay" id="promptModal">
<div class="modal">
<h2 id="promptTitle">Edit</h2>
<div class="modal-field" id="promptFieldWrap">
<label id="promptLabel"></label>
<input type="text" id="promptInput">
<div class="field-note" id="promptNote" style="display:none"></div>
</div>
<div id="promptMsg" style="display:none"><div id="modalMsgText"></div></div>
<div class="modal-actions">
<button id="promptCancel" class="btn btn-ghost">Cancel</button>
<button id="promptOk" class="btn btn-accent">OK</button>
</div>
</div>
</div>
<div id="toastWrap"></div>
<script>
(function(){'use strict';
const $=id=>document.getElementById(id);
const cmdEl=$('cmd');
const runBtn=$('runBtn');
const historyEl=$('history');
const newChatBtn=$('newChatBtn');
const refreshBtn=$('refreshProjectsBtn');
const settingsBtn=$('settingsBtn');
const settingsModal=$('settingsModal');
const modalTimeout=$('modalTimeout');
const modalMaxOutput=$('modalMaxOutput');
const modalSave=$('modalSave');
const modalCancel=$('modalCancel');
const modalSuccess=$('modalSuccess');
const modalError=$('modalError');
const projectsView=$('projectsView');
const terminalView=$('terminalView');

const activeStreams={};
let selectedDir='';
let selectedFile='';
let authenticated=false;

function esc(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;')}
function timeStr(t){try{const d=new Date(t);return d.toLocaleTimeString([],{hour:'2-digit',minute:'2-digit',second:'2-digit'})}catch(e){return t}}

// ── Toast notifications (replace native alert) ──────────
const toastWrap=$('toastWrap');
function toast(msg,type){
const t=document.createElement('div');
t.className='toast'+(type?' '+type:'');
const ico=type==='err'?'✕':type==='ok'?'✓':'ℹ';
const iSpan=document.createElement('span');iSpan.className='t-ico';iSpan.textContent=ico;
const body=document.createElement('span');body.className='t-body';body.textContent=msg;
t.appendChild(iSpan);t.appendChild(body);
toastWrap.appendChild(t);
const ttl=type==='err'?6000:3500;
const kill=()=>{t.classList.add('out');setTimeout(()=>t.remove(),200)};
const timer=setTimeout(kill,ttl);
t.addEventListener('click',()=>{clearTimeout(timer);kill()});
}

// ── Reusable prompt / confirm modal (replace native prompt/confirm) ──
const promptModal=$('promptModal');
let promptResolver=null;
function closePromptModal(val){
promptModal.classList.remove('show');
const r=promptResolver;promptResolver=null;
if(r)r(val);
}
function uiPrompt(opts){
opts=opts||{};
$('promptTitle').textContent=opts.title||'Edit';
$('promptFieldWrap').style.display='';
$('promptMsg').style.display='none';
$('promptLabel').textContent=opts.label||'';
const inp=$('promptInput');
inp.type=opts.type||'text';
inp.value=opts.value!=null?opts.value:'';
inp.placeholder=opts.placeholder||'';
const note=$('promptNote');
if(opts.note){note.textContent=opts.note;note.style.display=''}else{note.style.display='none'}
const ok=$('promptOk');
ok.textContent=opts.okLabel||'Save';
ok.className='btn btn-accent';
promptModal.classList.add('show');
setTimeout(()=>{inp.focus();inp.select()},40);
return new Promise(res=>{promptResolver=v=>res(v)});
}
function uiConfirm(opts){
opts=opts||{};
$('promptTitle').textContent=opts.title||'Confirm';
$('promptFieldWrap').style.display='none';
$('promptMsg').style.display='';
$('modalMsgText').textContent=opts.message||'';
const ok=$('promptOk');
ok.textContent=opts.okLabel||'Confirm';
ok.className='btn '+(opts.danger?'btn-danger':'btn-accent');
promptModal.classList.add('show');
setTimeout(()=>ok.focus(),40);
return new Promise(res=>{promptResolver=v=>res(v===true)});
}
$('promptOk').addEventListener('click',()=>{
if($('promptFieldWrap').style.display==='none'){closePromptModal(true)}
else{closePromptModal($('promptInput').value)}
});
$('promptCancel').addEventListener('click',()=>closePromptModal(null));
promptModal.addEventListener('click',e=>{if(e.target===promptModal)closePromptModal(null)});
$('promptInput').addEventListener('keydown',e=>{
if(e.key==='Enter'){e.preventDefault();closePromptModal($('promptInput').value)}
else if(e.key==='Escape'){e.preventDefault();closePromptModal(null)}
});
document.addEventListener('keydown',e=>{if(e.key==='Escape'&&promptModal.classList.contains('show')&&$('promptFieldWrap').style.display==='none')closePromptModal(null)});

// ── Auth ────────────────────────────────────────────────
function getToken(){return sessionStorage.getItem('rover_token')||''}
function tokenExpired(){
const exp=sessionStorage.getItem('rover_token_expires');
return !exp||new Date(exp)<=new Date();
}
async function checkAuth(){
try{
const r=await fetch('/api/auth');
const data=await r.json();
if(!data.required){authenticated=true;initApp();return}
const saved=getToken();
if(saved&&!tokenExpired()){authenticated=true;initApp();return}
sessionStorage.removeItem('rover_token');
sessionStorage.removeItem('rover_token_expires');
$('loginOverlay').classList.add('show');
$('loginSecret').focus();
}catch(e){authenticated=true;initApp()}
}
$('loginBtn').addEventListener('click',doLogin);
$('loginSecret').addEventListener('keydown',function(e){if(e.key==='Enter')doLogin()});
async function doLogin(){
const secret=$('loginSecret').value.trim();
if(!secret)return;
$('loginError').classList.remove('show');
try{
const r=await fetch('/api/auth',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({secret})});
if(r.ok){
const data=await r.json();
sessionStorage.setItem('rover_token',data.token||'');
sessionStorage.setItem('rover_token_expires',data.expires_at||'');
authenticated=true;
$('loginOverlay').classList.remove('show');
initApp();
}else{
$('loginError').textContent='Invalid secret. Try again.';
$('loginError').classList.add('show');
$('loginSecret').value='';
$('loginSecret').focus();
}
}catch(e){
$('loginError').textContent='Connection error: '+e.message;
$('loginError').classList.add('show');
}
}

// ── Settings ────────────────────────────────────────────
settingsBtn.addEventListener('click',async()=>{
modalSuccess.style.display='none';
modalError.style.display='none';
try{
const r=await fetch('/api/config',{headers:{'X-Rover-Secret':getToken()}});
if(!r.ok)throw new Error('Failed to fetch config');
const cfg=await r.json();
modalTimeout.value=Math.round(cfg.exec_timeout_seconds);
modalMaxOutput.value=cfg.max_output_bytes;
}catch(e){
modalError.textContent='Failed to load settings: '+e.message;
modalError.style.display='';
}
settingsModal.classList.add('show');
});
modalCancel.addEventListener('click',()=>{settingsModal.classList.remove('show')});
settingsModal.addEventListener('click',e=>{if(e.target===settingsModal)settingsModal.classList.remove('show')});
modalSave.addEventListener('click',async()=>{
modalSuccess.style.display='none';
modalError.style.display='none';
const timeout=parseFloat(modalTimeout.value)||0;
const maxOutput=parseInt(modalMaxOutput.value,10)||0;
try{
const r=await fetch('/api/config',{
method:'PUT',
headers:{'Content-Type':'application/json','X-Rover-Secret':getToken()},
body:JSON.stringify({exec_timeout_seconds:timeout,max_output_bytes:maxOutput})
});
if(!r.ok){
const body=await r.json();
throw new Error(body.error||'Failed to save');
}
modalSuccess.textContent='Saved (effective for new commands)';
modalSuccess.style.display='';
setTimeout(()=>{settingsModal.classList.remove('show')},1100);
}catch(e){
modalError.textContent=e.message;
modalError.style.display='';
}
});

// ── Terminal history / chat ─────────────────────────────
function scrollToBottom(){requestAnimationFrame(()=>{historyEl.scrollTop=historyEl.scrollHeight})}
function clearEmptyState(){const es=historyEl.querySelector('.empty-state');if(es)es.remove()}
function showEmptyState(){
if(historyEl.querySelector('.chat-entry'))return;
clearEmptyState();
const d=document.createElement('div');
d.className='empty-state';
d.innerHTML='<div class="es-icon">⌘</div><h3>No commands yet</h3>'+
'<p>Run a shell command on the rover host. Each runs in a fresh, non-interactive shell &mdash; chain steps with <code>&amp;&amp;</code>.</p>';
historyEl.appendChild(d);
}

function createChatEntry(sess,skipOutput){
const entry=document.createElement('div');
entry.className='chat-entry';
entry.dataset.id=sess.id;

const cmdDiv=document.createElement('div');
cmdDiv.className='msg-command';
cmdDiv.innerHTML='<div class="bubble"></div>';
cmdDiv.querySelector('.bubble').textContent=sess.command;

const cmdTime=document.createElement('div');
cmdTime.className='time';
cmdTime.textContent=timeStr(sess.start_time);

const respDiv=document.createElement('div');
respDiv.className='msg-response';
respDiv.innerHTML='<div class="bubble"><div class="resp-stdout"></div><div class="resp-stderr"></div></div>';

if(!skipOutput){
const stdoutEl=respDiv.querySelector('.resp-stdout');
const stderrEl=respDiv.querySelector('.resp-stderr');
if(sess.stdout)stdoutEl.textContent=sess.stdout;
if(sess.stderr)stderrEl.textContent=sess.stderr;
if(sess.status!=='running'){
const exitDiv=document.createElement('div');
exitDiv.className='exit-code '+(sess.exit_code===0?'exit-ok':'exit-err');
exitDiv.textContent='Exit '+sess.exit_code;
respDiv.querySelector('.bubble').appendChild(exitDiv);
}
}

entry.appendChild(cmdDiv);
entry.appendChild(cmdTime);
entry.appendChild(respDiv);
return entry;
}

async function loadHistory(){
try{
const r=await fetch('/api/sessions',{headers:{'X-Rover-Secret':getToken()}});
if(!r.ok){showEmptyState();return}
const sessions=await r.json();
historyEl.innerHTML='';
if(!sessions||sessions.length===0){showEmptyState();return}
const token=getToken();
for(const s of sessions){
const entry=createChatEntry(s,false);
historyEl.appendChild(entry);
// The /api/sessions list omits stdout/stderr (kept lightweight); fetch each
// completed session's detail to restore its output after a reload.
if(s.status!=='running')fillSessionOutput(s.id,entry,token);
}
scrollToBottom();
}catch(e){showEmptyState()}
}

async function fillSessionOutput(id,entry,token){
try{
const r=await fetch('/api/sessions/'+id,{headers:{'X-Rover-Secret':token}});
if(!r.ok)return;
const d=await r.json();
const so=entry.querySelector('.resp-stdout');
const se=entry.querySelector('.resp-stderr');
if(so&&d.stdout)so.textContent=d.stdout;
if(se&&d.stderr)se.textContent=d.stderr;
scrollToBottom();
}catch(e){}
}

let cmdHistory=[];
let histIdx=-1;
cmdEl.addEventListener('keydown',e=>{
if(e.key==='Enter'&&!(e.shiftKey||e.ctrlKey||e.metaKey)){e.preventDefault();run()}
if(e.key==='ArrowUp'){e.preventDefault();if(cmdHistory.length&&histIdx>=-1){histIdx=histIdx===-1?cmdHistory.length-1:Math.max(0,histIdx-1);cmdEl.value=cmdHistory[histIdx]||'';setTimeout(()=>cmdEl.setSelectionRange(cmdEl.value.length,cmdEl.value.length),0)}}
if(e.key==='ArrowDown'){e.preventDefault();if(cmdHistory.length&&histIdx>=0){histIdx=Math.min(cmdHistory.length-1,histIdx+1);cmdEl.value=cmdHistory[histIdx]||''}else{histIdx=-1;cmdEl.value=''}}
});

// Advisory pre-flight check: heuristics only — flags commands likely to misbehave
// because rover runs each command in a fresh, non-interactive shell on the host.
function commandHint(raw){
var cmd=(raw||'').trim();
if(!cmd)return null;
var rest=cmd.replace(/^(sudo\s+)?((\w+=\S+)\s+)*/,'');
var tokens=rest.split(/\s+/);
var prog=(tokens[0]||'').toLowerCase().replace(/.*[\\/]/,'').replace(/\.(exe|cmd|bat)$/,'');
var args=tokens.slice(1);
var lc=rest.toLowerCase();
var chained=/&&|;|\|/.test(rest);
function has(a){return args.indexOf(a)!==-1}

if((prog==='cd'||prog==='export'||prog==='set'||prog==='source'||prog==='.'||prog==='pushd'||/\bactivate\b/.test(lc)||lc.indexOf('nvm use')===0||lc.indexOf('conda activate')===0)&&!chained)
return {level:'warn',msg:'rover runs every command in a fresh shell, so a directory or environment change here will NOT carry over to your next command. Chain them in one line with &&, e.g. cd dir && your-command.'};

var gui=['start','explorer','open','xdg-open','gnome-open','kde-open','code','notepad','notepad++','gedit','chrome','google-chrome','chromium','firefox','msedge','edge','safari','brave'];
if(gui.indexOf(prog)!==-1&&!(has('--version')||has('-v')||has('--help')||has('-h')))
return {level:'warn',msg:'This opens an app/window on the machine running rover, not in your browser here. In the foreground it blocks until that window closes (or the timeout); detached, it keeps running on the host with no output shown.'};

var editors=['vim','vi','nano','emacs','pico','micro','vimtutor'];
var pagers=['less','more','man','top','htop','btop'];
var prompts=['ssh','telnet','ftp','sftp','su','passwd','gpg'];
var repls=['python','python3','node','ruby','irb','php','psql','mysql','sqlite3','mongo','mongosh','redis-cli','bash','sh','zsh','fish','pwsh','powershell','julia','gdb'];
if(editors.indexOf(prog)!==-1) return {level:'err',msg:prog+' is an interactive editor. rover has no terminal or stdin, so it will hang until the timeout. Not usable here.'};
if(pagers.indexOf(prog)!==-1) return {level:'err',msg:prog+' needs an interactive terminal that rover does not provide, so it will hang until the timeout.'};
if(prompts.indexOf(prog)!==-1) return {level:'err',msg:prog+' will prompt for input (e.g. a password or confirmation); rover cannot send input, so it will hang until the timeout. Use a non-interactive form (keys/flags).'};
if(repls.indexOf(prog)!==-1&&args.length===0) return {level:'err',msg:prog+' with no script opens an interactive REPL; rover has no stdin, so it will hang until the timeout. Pass a script or use -c.'};
if(prog==='tail'&&(has('-f')||has('--follow'))) return {level:'warn',msg:'tail -f never ends and a Terminal command cannot be stopped, so it runs until the 10-min / 1 MB limit kills it.'};

if(prog==='git'){
if(args[0]==='commit'&&!(has('-m')||has('--message')||has('--amend')||has('-F')||has('--file'))) return {level:'warn',msg:'git commit without -m opens an editor, which will hang (no terminal). Use git commit -m "message".'};
if(args[0]==='rebase'&&(has('-i')||has('--interactive'))) return {level:'err',msg:'git rebase -i is interactive and will hang; rover has no terminal.'};
}
if(prog==='npm'&&args[0]==='init'&&!(has('-y')||has('--yes'))) return {level:'warn',msg:'npm init prompts interactively; add -y, or it will hang until the timeout.'};

if(prog==='vite'||prog==='nodemon'||/\b(dev|serve|watch|runserver)\b/.test(lc)||(prog==='npm'&&(has('dev')||has('start'))))
return {level:'info',msg:'This looks long-running. The Terminal tab has no stop button, so it runs until it exits or hits the 10-min / 1 MB limit. To run a server, use the Projects tab instead.'};

return null;
}

function renderCmdHint(){
var el=$('cmdHint');if(!el)return;
var h=commandHint(cmdEl.value);
if(!h){el.className='cmd-hint';el.innerHTML='';return}
el.className='cmd-hint show lv-'+h.level;
// err/warn categories are rejected server-side (HTTP 422); info is advisory only.
var blocked=(h.level==='err'||h.level==='warn');
var ico=blocked?'🚫':'ℹ';
var label=blocked?'Blocked: ':'';
el.innerHTML='<div class="hint-inner"><span class="hint-ico">'+ico+'</span><span>'+
(label?'<b>'+label+'</b>':'')+esc(h.msg)+'</span></div>';
}
cmdEl.addEventListener('input',renderCmdHint);

async function run(){
const cmd=cmdEl.value.trim();
if(!cmd){cmdEl.focus();return}
const token=getToken();

cmdHistory.push(cmd);
if(cmdHistory.length>100)cmdHistory.shift();
histIdx=-1;

runBtn.disabled=true;
runBtn.classList.add('loading');

try{
const r=await fetch('/api/sessions',{
method:'POST',
headers:{'Content-Type':'application/json','X-Rover-Secret':token},
body:JSON.stringify({command:cmd})
});
if(!r.ok){
const body=await r.json();
toast(body.error||('Error '+r.status),'err');
return;
}
const data=await r.json();

const sessRes=await fetch('/api/sessions/'+data.id,{headers:{'X-Rover-Secret':token}});
const sess=await sessRes.json();
clearEmptyState();
const entry=createChatEntry(sess,true);
historyEl.appendChild(entry);
scrollToBottom();

const esUrl='/api/sessions/'+data.id+'/stream'+(token?'?secret='+encodeURIComponent(token):'');
const es=new EventSource(esUrl);
es.onmessage=function(event){
try{
const d=JSON.parse(event.data);
const stdoutEl=entry.querySelector('.resp-stdout');
const stderrEl=entry.querySelector('.resp-stderr');
switch(d.type){
case'stdout':{if(stdoutEl)stdoutEl.textContent+=d.data;break}
case'stderr':{if(stderrEl)stderrEl.textContent+=d.data;break}
case'done':{
es.close();
const bubble=entry.querySelector('.msg-response .bubble');
let exitDiv=bubble.querySelector('.exit-code');
if(!exitDiv){exitDiv=document.createElement('div');bubble.appendChild(exitDiv)}
exitDiv.className='exit-code '+(d.exit_code===0?'exit-ok':'exit-err');
exitDiv.textContent='Exit '+d.exit_code;
break;
}
}
scrollToBottom();
}catch(e){}
};
es.onerror=function(){es.close()};
}catch(e){
toast('Connection error: '+e.message,'err');
}finally{
runBtn.disabled=false;
runBtn.classList.remove('loading');
cmdEl.value='';
renderCmdHint();
cmdEl.focus();
}
}

function initApp(){
runBtn.addEventListener('click',run);
newChatBtn.addEventListener('click',()=>{historyEl.innerHTML='';showEmptyState()});
loadHistory();

// ── Tab switching ─────────────────────────────────────
document.querySelectorAll('.tab-btn').forEach(btn=>{
btn.addEventListener('click',()=>{
document.querySelectorAll('.tab-btn').forEach(b=>b.classList.remove('active'));
btn.classList.add('active');
const tab=btn.dataset.tab;
if(tab==='terminal'){
terminalView.style.display='flex';
projectsView.style.display='none';
newChatBtn.style.display='';
$('addProjectBtn').style.display='none';
refreshBtn.style.display='none';
cmdEl.focus();
}else{
terminalView.style.display='none';
projectsView.style.display='flex';
newChatBtn.style.display='none';
$('addProjectBtn').style.display='';
refreshBtn.style.display='';
loadProjects();
}
});
});
}

// ── Projects ──────────────────────────────────────────
refreshBtn.addEventListener('click',loadProjects);

async function loadProjects(){
try{
const r=await fetch('/api/projects',{headers:{'X-Rover-Secret':getToken()}});
if(!r.ok)return;
const projects=await r.json();
renderProjects(projects);
}catch(e){
projectsView.innerHTML='<div class="projects-empty"><div class="es-icon">⚠</div><h3>Failed to load projects</h3><p>Could not reach the rover server.</p></div>';
}
}

function renderProjects(projects){
if(!projects||projects.length===0){
projectsView.innerHTML='<div class="projects-grid"><div class="projects-empty"><div class="es-icon">📦</div><h3>No projects yet</h3><p>Click <b>+ Add Project</b> to register a local server.</p></div></div>';
return;
}
let html='<div class="projects-grid">';
for(const p of projects){
const status=determineStatus(p);
const dotCls=status==='running'?'running':status==='starting'?'starting':status==='failed'?'failed':'stopped';
const statusLabel=status==='running'?'Running':status==='starting'?'Starting':status==='failed'?'Failed':'Stopped';
const urlHtml=p.running_url?'<a href="'+esc(p.running_url)+'" target="_blank" rel="noopener" class="project-url">↗ '+esc(p.running_url)+'</a>':'';
const proxyUrlHtml=p.proxy_url?'<a href="'+esc(p.proxy_url)+'" target="_blank" rel="noopener" class="project-url proxy">⎐ '+esc(p.proxy_url)+'</a>':'';
const proxyLabel=p.proxy_enabled?'Proxy ON':'Proxy OFF';
const proxyBtnCls=p.proxy_enabled?'btn-proxy-on':'btn-proxy-off';
html+='<div class="project-card" data-name="'+esc(p.name)+'">'+
'<div class="project-card-header">'+
'<span class="project-name">'+esc(p.name)+'</span>'+
'<span class="status-pill '+dotCls+'" id="pill-'+esc(p.name)+'"><span class="status-dot"></span>'+statusLabel+'</span>'+
'<button class="btn-remove" data-project="'+esc(p.name)+'" title="Remove project">✕</button>'+
'</div>'+
'<div class="project-meta">'+
'<span class="meta-chip">Port <b>'+(p.port||'—')+'</b></span>'+
'<span id="url-'+esc(p.name)+'">'+urlHtml+'</span>'+
(proxyUrlHtml?'<span id="proxyurl-'+esc(p.name)+'">'+proxyUrlHtml+'</span>':'')+
'</div>'+
'<div class="project-actions">'+
'<button class="btn btn-primary btn-start" id="start-'+esc(p.name)+'" data-project="'+esc(p.name)+'" '+(status==='running'||status==='starting'?'disabled':'')+'>Start</button>'+
'<button class="btn btn-danger btn-stop" id="stop-'+esc(p.name)+'" data-project="'+esc(p.name)+'" '+(status==='running'?'':'disabled')+'>Stop</button>'+
'<button class="btn btn-ghost btn-log" id="log-'+esc(p.name)+'" data-project="'+esc(p.name)+'">Log</button>'+
'<span class="spacer"></span>'+
'<button class="btn btn-ghost btn-port" id="port-'+esc(p.name)+'" data-project="'+esc(p.name)+'" data-port="'+(p.port||'')+'">Port</button>'+
'<button class="btn btn-ghost btn-cmd" id="cmd-'+esc(p.name)+'" data-project="'+esc(p.name)+'" data-cmd="'+esc(p.start_cmd)+'">Cmd</button>'+
'<button class="btn '+proxyBtnCls+'" id="proxy-'+esc(p.name)+'" data-project="'+esc(p.name)+'" data-enabled="'+p.proxy_enabled+'">'+proxyLabel+'</button>'+
'</div>'+
(p.description?'<div class="project-desc">'+esc(p.description)+'</div>':'')+
'<div class="project-start-cmd">'+esc(p.start_cmd)+'</div>'+
'<div class="project-console" id="console-'+esc(p.name)+'">'+
'<div class="project-console-header"><span>Console output</span></div>'+
'<div class="project-console-output" id="output-'+esc(p.name)+'"></div>'+
'</div>'+
'</div>';
}
html+='</div>';
projectsView.innerHTML=html;

// Auto-connect to streams for running projects
for(const p of projects){
if(p.is_running&&!activeStreams[p.name]){
connectProjectStream(p.name);
}
}

// Event delegation for project buttons (avoids inline onclick JS string injection)
projectsView.addEventListener('click',function(e){
const btn=e.target.closest('[data-project]');
if(!btn)return;
const name=btn.dataset.project;
if(btn.classList.contains('btn-start'))startProject(name);
else if(btn.classList.contains('btn-stop'))stopProject(name);
else if(btn.classList.contains('btn-log'))toggleLog(name);
else if(btn.classList.contains('btn-port'))editPort(name,btn.dataset.port);
else if(btn.classList.contains('btn-remove')){e.stopPropagation();removeProject(name);}
else if(btn.classList.contains('btn-proxy-on')||btn.classList.contains('btn-proxy-off'))toggleProxy(name);
else if(btn.classList.contains('btn-cmd'))editCommand(name);
});
}

function determineStatus(p){
if(p.is_running)return'running';
return'stopped';
}

function setPill(name,cls,label){
const pill=$('pill-'+name);
if(!pill)return;
pill.className='status-pill '+cls;
pill.innerHTML='<span class="status-dot"></span>'+label;
}

window.startProject=async function(name,portOverride){
const startBtn=$('start-'+name);
const stopBtn=$('stop-'+name);
if(!startBtn)return;
setPill(name,'starting','Starting');
startBtn.disabled=true;
if(stopBtn)stopBtn.disabled=true;
try{
const body=portOverride?JSON.stringify({port:portOverride}):'';
const r=await fetch('/api/projects/'+encodeURIComponent(name)+'/start',{method:'POST',headers:{'Content-Type':'application/json','X-Rover-Secret':getToken()},body:body});
if(!r.ok){
const e=await r.json();
setPill(name,'stopped','Stopped');
startBtn.disabled=false;
if(r.status===409&&e.port_in_use){
// Occupied port — offer a one-off alternative for THIS start only.
const ans=await uiPrompt({title:'Port in use',label:(e.error||'Port in use')+'. Enter a different port for this start:',type:'number',value:'',placeholder:'e.g. 8790',okLabel:'Start',note:'The saved default port is unchanged.'});
const np=parseInt(ans,10);
if(np>0)startProject(name,np);
return;
}
toast('Failed to start: '+(e.error||r.status),'err');
return;
}
setPill(name,'running','Running');
if(stopBtn)stopBtn.disabled=false;
connectProjectStream(name);
}catch(e){
setPill(name,'stopped','Stopped');
startBtn.disabled=false;
toast('Connection error: '+e.message,'err');
}
};

window.editPort=async function(name,current){
const ans=await uiPrompt({title:'Edit port',label:'Default port for "'+name+'"',type:'number',value:current||'',placeholder:'e.g. 8770',okLabel:'Save',note:'Rover passes it as --port on start.'});
if(ans===null)return;
const np=parseInt(ans,10);
if(!(np>0)){toast('Please enter a valid port number.','err');return;}
try{
const r=await fetch('/api/projects/'+encodeURIComponent(name),{method:'PUT',headers:{'Content-Type':'application/json','X-Rover-Secret':getToken()},body:JSON.stringify({port:np})});
const data=await r.json();
if(!r.ok){toast('Failed to update port: '+(data.error||r.status),'err');return;}
toast('Port updated to '+np,'ok');
loadProjects();
}catch(e){
toast('Connection error: '+e.message,'err');
}
};

window.editCommand=async function(name){
const btn=$('cmd-'+name);
if(!btn)return;
const current=btn.dataset.cmd||'';
const ans=await uiPrompt({title:'Edit start command',label:'Start command for "'+name+'"',type:'text',value:current,placeholder:'e.g. python server.py',okLabel:'Save'});
if(ans===null)return;
if(!ans.trim()){toast('Start command cannot be empty.','err');return;}
try{
const r=await fetch('/api/projects/'+encodeURIComponent(name),{method:'PUT',headers:{'Content-Type':'application/json','X-Rover-Secret':getToken()},body:JSON.stringify({start_cmd:ans.trim()})});
const data=await r.json();
if(!r.ok){toast('Failed to update command: '+(data.error||r.status),'err');return;}
toast('Start command updated','ok');
loadProjects();
}catch(e){
toast('Connection error: '+e.message,'err');
}
};

function connectProjectStream(name){
if(activeStreams[name]){
activeStreams[name].close();
}
const outputEl=$('output-'+name);
const sec=getToken();
const esUrl='/api/projects/'+encodeURIComponent(name)+'/stream'+(sec?'?secret='+encodeURIComponent(sec):'');
const es=new EventSource(esUrl);
activeStreams[name]=es;
es.onmessage=function(event){
try{
const d=JSON.parse(event.data);
if(!outputEl)return;
if(d.type==='stdout'||d.type==='stderr'){
outputEl.textContent+=d.data;
outputEl.scrollTop=outputEl.scrollHeight;
// Extract URL from output
const urlMatch=d.data.match(/https?:\/\/[^\s"'<>]+/);
if(urlMatch){
const urlEl=$('url-'+name);
if(urlEl)urlEl.innerHTML='<a href="'+esc(urlMatch[0])+'" target="_blank" rel="noopener" class="project-url">↗ '+esc(urlMatch[0])+'</a>';
}
}else if(d.type==='url'){
const urlEl=$('url-'+name);
if(urlEl)urlEl.innerHTML='<a href="'+esc(d.data)+'" target="_blank" rel="noopener" class="project-url">↗ '+esc(d.data)+'</a>';
if(outputEl){
outputEl.innerHTML+='<div class="url-line">'+esc(d.data)+'</div>';
}
}else if(d.type==='done'){
es.close();
delete activeStreams[name];
setPill(name,'stopped','Stopped');
const startBtn=$('start-'+name);
const stopBtn=$('stop-'+name);
if(startBtn)startBtn.disabled=false;
if(stopBtn)stopBtn.disabled=true;
}
}catch(e){}
};
es.onerror=function(){
es.close();
delete activeStreams[name];
setPill(name,'stopped','Stopped');
const startBtn=$('start-'+name);
const stopBtn=$('stop-'+name);
if(startBtn)startBtn.disabled=false;
if(stopBtn)stopBtn.disabled=true;
};
}

window.stopProject=async function(name){
try{
const r=await fetch('/api/projects/'+encodeURIComponent(name)+'/stop',{method:'POST',headers:{'X-Rover-Secret':getToken()}});
if(!r.ok){
const e=await r.json();
toast('Failed to stop: '+(e.error||r.status),'err');
return;
}
if(activeStreams[name]){
activeStreams[name].close();
delete activeStreams[name];
}
setPill(name,'stopped','Stopped');
const startBtn=$('start-'+name);
const stopBtn=$('stop-'+name);
if(startBtn)startBtn.disabled=false;
if(stopBtn)stopBtn.disabled=true;
}catch(e){
toast('Connection error: '+e.message,'err');
}
};

window.toggleLog=function(name){
const consoleEl=$('console-'+name);
if(consoleEl){
consoleEl.classList.toggle('open');
const logBtn=$('log-'+name);
if(logBtn)logBtn.textContent=consoleEl.classList.contains('open')?'Hide':'Log';
}
};

// ── Add / Remove Project ─────────────────────────────
$('addProjectBtn').addEventListener('click',openAddProjectModal);
$('addProjectCancel').addEventListener('click',closeAddProjectModal);
$('addProjectModal').addEventListener('click',function(e){
if(e.target===this)closeAddProjectModal();
});
$('addProjectConfirm').addEventListener('click',validateAndAddProject);

function openAddProjectModal(){
$('addProjectModal').classList.add('show');
$('addProjectDetails').style.display='none';
$('addProjectConfirm').style.display='none';
$('addProjectStatus').className='';
$('addProjectStatus').textContent='';
$('addPort').value='';
selectedDir='';
selectedFile='';
loadProjectDirs();
}
function closeAddProjectModal(){
$('addProjectModal').classList.remove('show');
}
async function loadProjectDirs(){
const list=$('dirList');
list.innerHTML='<div class="picker-item" style="cursor:default;color:var(--text-faint)">Loading…</div>';
try{
const r=await fetch('/api/projects/dirs',{headers:{'X-Rover-Secret':getToken()}});
const dirs=await r.json();
if(!dirs||dirs.length===0){
list.innerHTML='<div class="picker-item" style="cursor:default;color:var(--text-faint)">No unregistered projects found.</div>';
return;
}
list.innerHTML='';
for(const d of dirs){
const item=document.createElement('div');
item.className='picker-item';
item.textContent=d;
item.dataset.dir=d;
item.addEventListener('click',function(){
document.querySelectorAll('#dirList .picker-item').forEach(el=>el.classList.remove('selected'));
this.classList.add('selected');
selectedDir=this.dataset.dir;
loadProjectFiles(selectedDir);
});
list.appendChild(item);
}
}catch(e){
list.innerHTML='<div class="picker-item" style="cursor:default;color:var(--text-faint)">Failed to load directories.</div>';
}
}
async function loadProjectFiles(dir){
const list=$('fileList');
list.innerHTML='<div class="picker-item" style="cursor:default;color:var(--text-faint)">Loading…</div>';
$('addProjectDetails').style.display='none';
$('addProjectConfirm').style.display='none';
selectedFile='';
try{
const r=await fetch('/api/projects/'+encodeURIComponent(dir)+'/files',{headers:{'X-Rover-Secret':getToken()}});
const files=await r.json();
if(!files||files.length===0){
list.innerHTML='<div class="picker-item" style="cursor:default;color:var(--text-faint)">No eligible files found.</div>';
return;
}
list.innerHTML='';
for(const f of files){
const item=document.createElement('div');
item.className='picker-item';
item.innerHTML=esc(f.path)+' <span class="sub">'+esc(f.start_cmd)+'</span>';
item.dataset.cmd=f.start_cmd;
item.dataset.path=f.path;
item.addEventListener('click',function(){
document.querySelectorAll('#fileList .picker-item').forEach(el=>el.classList.remove('selected'));
this.classList.add('selected');
selectedFile=this.dataset.path;
$('addStartCmd').value=this.dataset.cmd;
$('addProjectDetails').style.display='';
$('addProjectConfirm').style.display='';
$('addProjectStatus').className='';
$('addProjectStatus').textContent='';
});
list.appendChild(item);
}
}catch(e){
list.innerHTML='<div class="picker-item" style="cursor:default;color:var(--text-faint)">Failed to load files.</div>';
}
}
async function validateAndAddProject(){
if(!selectedDir||!selectedFile)return;
const btn=$('addProjectConfirm');
const status=$('addProjectStatus');
const port=parseInt($('addPort').value,10);
if(!(port>0)){
status.className='error';
status.textContent='Please enter a valid port for this project.';
return;
}
btn.disabled=true;
status.className='loading';
status.textContent='Starting and validating project on port '+port+' (max 15 seconds)…';
try{
const r=await fetch('/api/projects',{
method:'POST',
headers:{'Content-Type':'application/json','X-Rover-Secret':getToken()},
body:JSON.stringify({name:selectedDir,start_cmd:$('addStartCmd').value,port:port})
});
const data=await r.json();
if(!r.ok){
status.className='error';
status.textContent='Failed: '+(data.error||'unknown error');
btn.disabled=false;
return;
}
status.className='success';
status.textContent='Project "'+selectedDir+'" added at '+data.url+' (port '+data.port+')';
toast('Project "'+selectedDir+'" added','ok');
setTimeout(()=>{
closeAddProjectModal();
loadProjects();
},1400);
}catch(e){
status.className='error';
status.textContent='Connection error: '+e.message;
btn.disabled=false;
}
}

window.removeProject=async function(name){
const ok=await uiConfirm({title:'Remove project',message:'Remove "'+name+'" from the launcher? This only unregisters it from rover — your files are untouched.',okLabel:'Remove',danger:true});
if(!ok)return;
try{
const r=await fetch('/api/projects/'+encodeURIComponent(name),{method:'DELETE',headers:{'X-Rover-Secret':getToken()}});
if(!r.ok){
const data=await r.json();
toast('Failed to remove: '+(data.error||r.status),'err');
return;
}
toast('Removed "'+name+'"','ok');
loadProjects();
}catch(e){
toast('Connection error: '+e.message,'err');
}
};

window.toggleProxy=async function(name){
const btn=$('proxy-'+name);
if(!btn)return;
const wasEnabled=btn.dataset.enabled==='true';
const newEnabled=!wasEnabled;
try{
const r=await fetch('/api/projects/'+encodeURIComponent(name)+'/proxy',{
method:'PUT',
headers:{'Content-Type':'application/json','X-Rover-Secret':getToken()},
body:JSON.stringify({enabled:newEnabled})
});
if(!r.ok){const e=await r.json();toast('Failed to toggle proxy: '+(e.error||r.status),'err');return;}
loadProjects();
}catch(e){toast('Connection error: '+e.message,'err');}
};

checkAuth();
})();

</script>
</body>
</html>
`)
