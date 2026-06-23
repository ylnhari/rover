package server

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
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
	maxCommandBytes  = 10 * 1024
	maxAddProjectBody = 4 * 1024
	scanBufSize      = 256 * 1024
	maxSessions      = 500
	loginMaxPerMin   = 10
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
		mux.HandleFunc("GET /api/projects", s.requireAuth(s.handleListProjects))
		mux.HandleFunc("POST /api/projects", s.requireAuth(s.handleAddProject))
		mux.HandleFunc("DELETE /api/projects/{name}", s.requireAuth(s.handleRemoveProject))
		mux.HandleFunc("POST /api/projects/{name}/start", s.requireAuth(s.handleStartProject))
		mux.HandleFunc("POST /api/projects/{name}/stop", s.requireAuth(s.handleStopProject))
		mux.HandleFunc("GET /api/projects/{name}/stream", s.requireAuth(s.handleProjectStream))
		mux.HandleFunc("GET /api/projects/dirs", s.requireAuth(s.handleListProjectDirs))
		mux.HandleFunc("GET /api/projects/{name}/files", s.requireAuth(s.handleListProjectFiles))
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
		}
		views = append(views, v)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(views)
}

func (s *Server) handleStartProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.launcher.Start(name); err != nil {
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
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxAddProjectBody)).Decode(&body); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Name == "" || body.StartCmd == "" {
		jsonError(w, "name and start_cmd are required", http.StatusBadRequest)
		return
	}

	p, err := s.launcher.AddProject(body.Name, body.StartCmd)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(p)
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
<meta name="viewport" content="width=device-width,initial-scale=1.0,user-scalable=no">
<title>Rover</title>
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{--bg:#0b0f14;--bg-alt:#111820;--border:#1e2a3a;--border-focus:#3b82f6;--text:#e2e8f0;--text-dim:#6b7d96;--green:#22c55e;--green-hover:#16a34a;--red:#ef4444;--blue:#3b82f6;--amber:#f59e0b;--font:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;--msg-sent:#1a6b3c;--msg-recv:#1e2a3a}
html,body{height:100%}
body{background:var(--bg);color:var(--text);font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;display:flex;flex-direction:column;height:100dvh;line-height:1.5;-webkit-font-smoothing:antialiased;overflow:hidden}
header{background:var(--bg-alt);border-bottom:1px solid var(--border);padding:10px 16px;display:flex;align-items:center;gap:8px;flex-shrink:0;z-index:10}
header .logo{width:28px;height:28px;border-radius:6px;background:linear-gradient(135deg,#3b82f6,#8b5cf6);display:flex;align-items:center;justify-content:center;font-size:14px;color:#fff;flex-shrink:0;font-weight:700}
header h1{font-size:15px;font-weight:700;letter-spacing:-.3px}
.tab-bar{display:flex;gap:2px;margin-left:12px;margin-right:auto}
.tab-btn{background:0 0;border:none;color:var(--text-dim);cursor:pointer;padding:4px 12px;font-size:12px;font-weight:600;border-radius:5px;transition:all .15s}
.tab-btn:hover{color:var(--text);background:rgba(255,255,255,.05)}
.tab-btn.active{color:var(--blue);background:rgba(59,130,246,.12)}
#newChatBtn{background:0 0;border:1px solid var(--border);border-radius:5px;color:var(--text-dim);cursor:pointer;padding:4px 10px;font-size:11px;font-weight:600;transition:all .15s;white-space:nowrap}
#newChatBtn:hover{border-color:var(--blue);color:var(--blue)}
#addProjectBtn{background:var(--blue);border:none;border-radius:5px;color:#fff;cursor:pointer;padding:4px 10px;font-size:11px;font-weight:600;transition:all .15s;white-space:nowrap;display:none}
#addProjectBtn:hover{background:#2563eb}
#refreshProjectsBtn{background:0 0;border:1px solid var(--border);border-radius:5px;color:var(--text-dim);cursor:pointer;padding:4px 10px;font-size:11px;font-weight:600;transition:all .15s;white-space:nowrap;display:none}
#refreshProjectsBtn:hover{border-color:var(--blue);color:var(--blue)}
#settingsBtn{background:0 0;border:none;color:var(--text-dim);cursor:pointer;font-size:18px;padding:2px 6px;transition:color .15s;line-height:1}
#settingsBtn:hover{color:var(--text)}
#loginOverlay{display:none;position:fixed;inset:0;background:var(--bg);z-index:200;align-items:center;justify-content:center;flex-direction:column}
#loginOverlay.show{display:flex}
#loginBox{background:var(--bg-alt);border:1px solid var(--border);border-radius:12px;padding:32px;width:90%;max-width:360px;text-align:center}
#loginBox .logo{width:48px;height:48px;border-radius:12px;background:linear-gradient(135deg,#3b82f6,#8b5cf6);display:flex;align-items:center;justify-content:center;font-size:24px;color:#fff;font-weight:700;margin:0 auto 16px}
#loginBox h2{font-size:18px;font-weight:700;margin-bottom:4px}
#loginBox p{font-size:13px;color:var(--text-dim);margin-bottom:20px}
#loginBox input{width:100%;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);padding:10px 14px;font-size:14px;font-family:var(--font);outline:none;transition:border-color .15s;box-sizing:border-box}
#loginBox input:focus{border-color:var(--border-focus)}
#loginBox button{width:100%;margin-top:12px;background:var(--green);border:none;border-radius:6px;color:#fff;padding:10px;font-size:14px;font-weight:600;cursor:pointer;transition:background .15s}
#loginBox button:hover{background:var(--green-hover)}
#loginError{font-size:12px;color:var(--red);margin-top:10px;display:none}
#loginError.show{display:block}
#terminalView{display:flex;flex-direction:column;flex:1;min-height:0}
#projectsView{display:none;flex-direction:column;flex:1;min-height:0;padding:16px;overflow-y:auto}
#history{flex:1;overflow-y:auto;padding:16px;display:flex;flex-direction:column;gap:2px;scroll-behavior:smooth}
#history:empty::after{content:'No commands yet. Type a command below.';color:var(--text-dim);font-style:italic;text-align:center;padding:40px 0;font-size:14px;pointer-events:none}
.chat-entry{animation:fadeIn .2s ease-out;margin-bottom:10px}
@keyframes fadeIn{from{opacity:0;transform:translateY(8px)}to{opacity:1;transform:translateY(0)}}
.msg-command{display:flex;justify-content:flex-end;margin-bottom:2px;padding-left:40px}
.msg-command .bubble{background:var(--msg-sent);color:#fff;padding:8px 14px;border-radius:16px 4px 16px 16px;max-width:80%;word-break:break-word;font-family:var(--font);font-size:13px;line-height:1.5}
.msg-command .time{font-size:10px;color:var(--text-dim);text-align:right;margin-top:2px;margin-right:4px;padding-left:40px}
.msg-response{display:flex;justify-content:flex-start;margin-bottom:2px;padding-right:40px}
.msg-response .bubble{background:var(--msg-recv);color:var(--text);padding:8px 14px;border-radius:4px 16px 16px 16px;max-width:85%;word-break:break-word;font-family:var(--font);font-size:13px;line-height:1.5;white-space:pre-wrap}
.msg-response .time{font-size:10px;color:var(--text-dim);margin-top:2px;margin-left:4px;padding-right:40px}
.resp-stdout{color:#d4d4d4}
.resp-stderr{color:var(--red)}
.exit-code{font-size:11px;margin-top:6px;padding-top:4px;border-top:1px solid var(--border);font-weight:600}
.exit-ok{color:var(--green)}
.exit-err{color:var(--red)}
.input-bar{background:var(--bg-alt);border-top:1px solid var(--border);padding:10px 16px;display:flex;gap:8px;flex-shrink:0;padding-bottom:max(10px,env(safe-area-inset-bottom))}
#cmd{flex:1;background:var(--bg);border:1px solid var(--border);border-radius:8px;color:var(--text);padding:10px 14px;font-size:14px;font-family:var(--font);outline:none;transition:border-color .15s}
#cmd:focus{border-color:var(--border-focus)}
#cmd::placeholder{color:var(--text-dim)}
#runBtn{background:var(--green);border:none;border-radius:8px;color:#fff;padding:10px 20px;font-size:14px;font-weight:600;cursor:pointer;transition:all .15s;display:flex;align-items:center;gap:6px;flex-shrink:0}
#runBtn:hover:not(:disabled){background:var(--green-hover)}
#runBtn:active:not(:disabled){transform:scale(.97)}
#runBtn:disabled{background:var(--border);color:var(--text-dim);cursor:default}
#runBtn .spinner{display:none;width:14px;height:14px;border:2px solid rgba(255,255,255,.3);border-top-color:#fff;border-radius:50%;animation:spin .6s linear infinite}
#runBtn.loading .spinner{display:inline-block}
#runBtn.loading .label{display:none}
@keyframes spin{to{transform:rotate(360deg)}}
.modal-overlay{display:none;position:fixed;inset:0;background:rgba(0,0,0,.6);z-index:100;align-items:center;justify-content:center}
.modal-overlay.show{display:flex}
.modal{background:var(--bg-alt);border:1px solid var(--border);border-radius:12px;padding:24px;width:90%;max-width:400px}
.modal h2{font-size:15px;margin-bottom:16px;font-weight:700}
.modal-field{margin-bottom:14px}
.modal-field label{display:block;font-size:12px;color:var(--text-dim);margin-bottom:4px;font-weight:600}
.modal-field input{width:100%;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);padding:8px 12px;font-size:13px;font-family:var(--font);outline:none;transition:border-color .15s}
.modal-field input:focus{border-color:var(--border-focus)}
.modal-actions{display:flex;gap:8px;justify-content:flex-end;margin-top:20px}
.modal-actions button{padding:8px 16px;border-radius:6px;font-size:13px;font-weight:600;cursor:pointer;border:none;transition:all .15s}
#modalSave{background:var(--green);color:#fff}
#modalSave:hover{background:var(--green-hover)}
#modalCancel{background:var(--bg);border:1px solid var(--border);color:var(--text)}
#modalCancel:hover{border-color:var(--blue);color:var(--blue)}
.modal-success{color:var(--green);font-size:12px;margin-top:8px;text-align:center;display:none}
.modal-error{color:var(--red);font-size:12px;margin-top:8px;text-align:center;display:none}
.projects-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(340px,1fr));gap:12px}
.project-card{background:var(--bg-alt);border:1px solid var(--border);border-radius:10px;padding:16px;transition:border-color .15s}
.project-card:hover{border-color:var(--border-focus)}
.project-card-header{display:flex;align-items:center;gap:8px;margin-bottom:8px}
.project-name{font-size:14px;font-weight:700;flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.status-dot{width:8px;height:8px;border-radius:50%;flex-shrink:0}
.status-dot.stopped{background:var(--text-dim)}
.status-dot.starting{background:var(--amber);animation:pulse 1s ease-in-out infinite}
.status-dot.running{background:var(--green)}
.status-dot.failed{background:var(--red)}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.4}}
.project-details{font-size:12px;color:var(--text-dim);margin-bottom:10px;display:flex;gap:16px;flex-wrap:wrap}
.project-details span{display:flex;align-items:center;gap:4px}
.project-url{color:var(--blue);text-decoration:none;font-family:var(--font);font-size:12px}
.project-url:hover{text-decoration:underline}
.project-actions{display:flex;gap:6px;margin-bottom:8px}
.project-actions button{padding:5px 14px;border-radius:5px;font-size:11px;font-weight:600;cursor:pointer;border:none;transition:all .15s}
.btn-start{background:var(--green);color:#fff}
.btn-start:hover{background:var(--green-hover)}
.btn-stop{background:var(--red);color:#fff}
.btn-stop:hover{background:#dc2626}
.btn-log{background:0 0;border:1px solid var(--border);color:var(--text-dim)}
.btn-log:hover{border-color:var(--blue);color:var(--blue)}
.btn-start:disabled,.btn-stop:disabled{opacity:.5;cursor:default}
.project-console{display:none;margin-top:8px;border-top:1px solid var(--border);padding-top:8px}
.project-console.open{display:block}
.project-console-header{display:flex;justify-content:space-between;align-items:center;margin-bottom:4px}
.project-console-header span{font-size:11px;color:var(--text-dim);font-weight:600}
.project-console-output{background:var(--bg);border:1px solid var(--border);border-radius:6px;padding:8px;font-family:var(--font);font-size:12px;line-height:1.4;max-height:200px;overflow-y:auto;white-space:pre-wrap;word-break:break-all;color:#d4d4d4}
.project-console-output .url-line{color:var(--blue);font-weight:600}
.project-desc{font-size:11px;color:var(--text-dim);margin-top:4px}
.project-start-cmd{font-size:11px;color:var(--text-dim);font-family:var(--font);margin-top:2px;opacity:.7}
.projects-empty{color:var(--text-dim);text-align:center;padding:60px 20px;font-style:italic;font-size:14px}
.btn-remove{background:0 0;border:1px solid var(--red);color:var(--red);padding:3px 10px;border-radius:4px;font-size:10px;font-weight:600;cursor:pointer;transition:all .15s;margin-left:auto}
.btn-remove:hover{background:var(--red);color:#fff}
.picker-layout{display:flex;gap:16px;min-height:250px}
.picker-col{flex:1;border:1px solid var(--border);border-radius:6px;overflow:hidden;display:flex;flex-direction:column}
.picker-col-header{padding:8px 10px;font-size:11px;font-weight:600;color:var(--text-dim);background:var(--bg);border-bottom:1px solid var(--border)}
.picker-list{flex:1;overflow-y:auto;max-height:220px}
.picker-item{padding:7px 10px;font-size:12px;cursor:pointer;transition:background .1s;border-bottom:1px solid var(--border);font-family:var(--font)}
.picker-item:hover{background:rgba(59,130,246,.1)}
.picker-item.selected{background:rgba(59,130,246,.2);color:var(--blue);font-weight:600}
.picker-item .sub{font-size:10px;color:var(--text-dim);font-weight:400;margin-left:8px}
#addProjectDetails{margin-top:12px;padding:12px;background:var(--bg);border-radius:6px;border:1px solid var(--border)}
#addProjectStatus{margin-top:8px;font-size:12px}
#addProjectStatus.success{color:var(--green)}
#addProjectStatus.error{color:var(--red)}
#addProjectStatus.loading{color:var(--amber)}
@media(max-width:600px){header{padding:8px 10px;gap:6px}header h1{font-size:13px}.tab-btn{font-size:11px;padding:3px 8px}#secret{width:100px;font-size:11px;padding:4px 8px}#history{padding:10px 10px}.msg-command{padding-left:20px}.msg-response{padding-right:20px}.input-bar{padding:8px 10px;gap:6px}#cmd{font-size:13px;padding:8px 12px}#runBtn{padding:8px 14px;font-size:13px}.projects-grid{grid-template-columns:1fr}#projectsView{padding:10px}}
</style>
</head>
<body>
<header>
<div class="logo">R</div>
<h1>Rover</h1>
<div class="tab-bar">
<button class="tab-btn active" data-tab="terminal">Terminal</button>
<button class="tab-btn" data-tab="projects">Projects</button>
</div>
<button id="newChatBtn">Clear Chat</button>
<button id="addProjectBtn">Add Project</button>
<button id="refreshProjectsBtn">Refresh</button>
<button id="settingsBtn" title="Settings">&#x2699;</button>
</header>
<div id="loginOverlay">
<div id="loginBox">
<div class="logo">R</div>
<h2>Rover</h2>
<p>Enter the server secret to continue</p>
<input type="password" id="loginSecret" placeholder="Secret" autocomplete="off">
<button id="loginBtn">Login</button>
<div id="loginError">Invalid secret. Try again.</div>
</div>
</div>
<div class="modal-overlay" id="settingsModal">
<div class="modal">
<h2>Rover Settings</h2>
<div class="modal-field">
<label for="modalTimeout">Execution Timeout (seconds, 0 = no limit)</label>
<input type="number" id="modalTimeout" min="0" step="1">
</div>
<div class="modal-field">
<label for="modalMaxOutput">Max Output (bytes, 0 = no limit)</label>
<input type="number" id="modalMaxOutput" min="0" step="1">
</div>
<div class="modal-actions">
<button id="modalCancel">Cancel</button>
<button id="modalSave">Save</button>
</div>
<div id="modalSuccess" class="modal-success">Settings saved</div>
<div id="modalError" class="modal-error"></div>
</div>
</div>
<div id="terminalView">
<div id="history"></div>
<div class="input-bar">
<input type="text" id="cmd" placeholder="Type a command..." autofocus spellcheck="false">
<button id="runBtn"><span class="spinner"></span><span class="label">Run</span></button>
</div>
</div>
<div id="projectsView"></div>
<div class="modal-overlay" id="addProjectModal">
<div class="modal" style="max-width:640px">
<h2>Add Project</h2>
<div class="picker-layout">
<div class="picker-col">
<div class="picker-col-header">Select Directory</div>
<div class="picker-list" id="dirList"></div>
</div>
<div class="picker-col">
<div class="picker-col-header">Select Start File</div>
<div class="picker-list" id="fileList"></div>
</div>
</div>
<div id="addProjectDetails" style="display:none">
<div class="modal-field">
<label>Start Command</label>
<input type="text" id="addStartCmd" readonly>
</div>
<div id="addProjectStatus"></div>
</div>
<div class="modal-actions">
<button id="addProjectCancel">Cancel</button>
<button id="addProjectConfirm" style="display:none" class="btn-start">Validate &amp; Add</button>
</div>
</div>
</div>
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
$('loginError').classList.add('show');
$('loginSecret').value='';
$('loginSecret').focus();
}
}catch(e){
$('loginError').textContent='Connection error: '+e.message;
$('loginError').classList.add('show');
}
}

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
		modalSuccess.textContent='Settings saved (effective for new commands)';
		modalSuccess.style.display='';
		setTimeout(()=>{settingsModal.classList.remove('show')},1200);
	}catch(e){
		modalError.textContent=e.message;
		modalError.style.display='';
	}
});

function esc(s){return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;')}

function timeStr(t){try{const d=new Date(t);return d.toLocaleTimeString()}catch(e){return t}}

function scrollToBottom(){requestAnimationFrame(()=>{historyEl.scrollTop=historyEl.scrollHeight})}

function createChatEntry(sess,skipOutput){
const entry=document.createElement('div');
entry.className='chat-entry';
entry.dataset.id=sess.id;

const cmdDiv=document.createElement('div');
cmdDiv.className='msg-command';
cmdDiv.innerHTML='<div class="bubble">'+esc(sess.command)+'</div>';

const cmdTime=document.createElement('div');
cmdTime.className='time';
cmdTime.textContent=timeStr(sess.start_time);

const respDiv=document.createElement('div');
respDiv.className='msg-response';
respDiv.innerHTML='<div class="bubble"><div class="resp-stdout"></div><div class="resp-stderr"></div></div>';

const respTime=document.createElement('div');
respTime.className='time';
respTime.textContent=timeStr(sess.start_time);

if(!skipOutput){
const stdoutEl=respDiv.querySelector('.resp-stdout');
const stderrEl=respDiv.querySelector('.resp-stderr');
if(sess.stdout)stdoutEl.textContent=sess.stdout;
if(sess.stderr)stderrEl.textContent=sess.stderr;
if(sess.status!=='running'){
const exitDiv=document.createElement('div');
exitDiv.className='exit-code '+(sess.exit_code===0?'exit-ok':'exit-err');
exitDiv.textContent='Exit code: '+sess.exit_code;
respDiv.querySelector('.bubble').appendChild(exitDiv);
}
}

entry.appendChild(cmdDiv);
entry.appendChild(cmdTime);
entry.appendChild(respDiv);
entry.appendChild(respTime);
return entry;
}

async function loadHistory(){
try{
const r=await fetch('/api/sessions',{headers:{'X-Rover-Secret':getToken()}});
if(!r.ok)return;
const sessions=await r.json();
historyEl.innerHTML='';
for(const s of sessions){
historyEl.appendChild(createChatEntry(s,false));
}
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
alert('Error: '+(body.error||r.status));
return;
}
const data=await r.json();

const sessRes=await fetch('/api/sessions/'+data.id,{headers:{'X-Rover-Secret':token}});
const sess=await sessRes.json();
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
exitDiv.textContent='Exit code: '+d.exit_code;
break;
}
}
scrollToBottom();
}catch(e){}
};
es.onerror=function(){es.close()};
}catch(e){
alert('Connection error: '+e.message);
}finally{
runBtn.disabled=false;
runBtn.classList.remove('loading');
cmdEl.value='';
cmdEl.focus();
}
}

function initApp(){
runBtn.addEventListener('click',run);
newChatBtn.addEventListener('click',()=>{historyEl.innerHTML=''});
loadHistory();

// ── Tab switching ─────────────────────────────────────────────
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

// ── Projects ──────────────────────────────────────────────────
refreshBtn.addEventListener('click',loadProjects);

// ── Projects ──────────────────────────────────────────────────
async function loadProjects(){
try{
const r=await fetch('/api/projects',{headers:{'X-Rover-Secret':getToken()}});
if(!r.ok)return;
const projects=await r.json();
renderProjects(projects);
}catch(e){
projectsView.innerHTML='<div class="projects-empty">Failed to load projects.</div>';
}
}

function renderProjects(projects){
if(!projects||projects.length===0){
projectsView.innerHTML='<div class="projects-empty">No projects found.</div>';
return;
}
let html='<div class="projects-grid">';
for(const p of projects){
const status=determineStatus(p);
const dotCls=status==='running'?'running':status==='starting'?'starting':status==='failed'?'failed':'stopped';
const btnDisabled=status==='starting'?'disabled':'';
const urlHtml=p.running_url?'<a href="'+p.running_url+'" target="_blank" class="project-url">'+esc(p.running_url)+'</a>':'';
	html+='<div class="project-card" data-name="'+esc(p.name)+'">'+
		'<div class="project-card-header">'+
		'<span class="status-dot '+dotCls+'" id="dot-'+esc(p.name)+'"></span>'+
		'<span class="project-name">'+esc(p.name)+'</span>'+
		'<button class="btn-remove" data-project="'+esc(p.name)+'" title="Remove project">Remove</button>'+
		'</div>'+
'<div class="project-details">'+
'<span>Port: '+(p.port||'auto')+'</span>'+
'<span id="url-'+esc(p.name)+'">'+urlHtml+'</span>'+
'</div>'+
'<div class="project-actions">'+
'<button class="btn-start" id="start-'+esc(p.name)+'" data-project="'+esc(p.name)+'" '+(status==='running'||status==='starting'?'disabled':'')+'>Start</button>'+
'<button class="btn-stop" id="stop-'+esc(p.name)+'" data-project="'+esc(p.name)+'" '+(status==='running'?'':'disabled')+'>Stop</button>'+
'<button class="btn-log" id="log-'+esc(p.name)+'" data-project="'+esc(p.name)+'">Log</button>'+
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
else if(btn.classList.contains('btn-remove')){e.stopPropagation();removeProject(name);}
});
}

function determineStatus(p){
if(p.is_running)return'running';
return'stopped';
}

window.startProject=async function(name){
const dot=$('dot-'+name);
const startBtn=$('start-'+name);
const stopBtn=$('stop-'+name);
if(!dot||!startBtn)return;
dot.className='status-dot starting';
startBtn.disabled=true;
stopBtn.disabled=true;
try{
const r=await fetch('/api/projects/'+encodeURIComponent(name)+'/start',{method:'POST',headers:{'X-Rover-Secret':getToken()}});
if(!r.ok){
const e=await r.json();
alert('Failed to start: '+(e.error||r.status));
dot.className='status-dot stopped';
startBtn.disabled=false;
return;
}
		dot.className='status-dot running';
		if(stopBtn)stopBtn.disabled=false;
		connectProjectStream(name);
	}catch(e){
dot.className='status-dot stopped';
startBtn.disabled=false;
alert('Connection error: '+e.message);
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
if(urlEl)urlEl.innerHTML='<a href="'+urlMatch[0]+'" target="_blank" class="project-url">'+esc(urlMatch[0])+'</a>';
}
}else if(d.type==='url'){
const urlEl=$('url-'+name);
if(urlEl)urlEl.innerHTML='<a href="'+d.data+'" target="_blank" class="project-url">'+esc(d.data)+'</a>';
// Also add a URL line to output
if(outputEl){
outputEl.innerHTML+='<div class="url-line">'+esc(d.data)+'</div>';
}
}else if(d.type==='done'){
es.close();
delete activeStreams[name];
const dot=$('dot-'+name);
if(dot)dot.className='status-dot stopped';
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
const dot=$('dot-'+name);
if(dot)dot.className='status-dot stopped';
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
alert('Failed to stop: '+(e.error||r.status));
return;
}
if(activeStreams[name]){
activeStreams[name].close();
delete activeStreams[name];
}
const dot=$('dot-'+name);
if(dot)dot.className='status-dot stopped';
const startBtn=$('start-'+name);
const stopBtn=$('stop-'+name);
if(startBtn)startBtn.disabled=false;
if(stopBtn)stopBtn.disabled=true;
}catch(e){
alert('Connection error: '+e.message);
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

// ── Add / Remove Project ─────────────────────────────────────
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
	selectedDir='';
	selectedFile='';
	loadProjectDirs();
}

function closeAddProjectModal(){
	$('addProjectModal').classList.remove('show');
}

async function loadProjectDirs(){
	const list=$('dirList');
	list.innerHTML='<div class="picker-item" style="cursor:default;color:var(--text-dim)">Loading...</div>';
	try{
		const r=await fetch('/api/projects/dirs',{headers:{'X-Rover-Secret':getToken()}});
		const dirs=await r.json();
		if(!dirs||dirs.length===0){
			list.innerHTML='<div class="picker-item" style="cursor:default;color:var(--text-dim)">No unregistered projects found.</div>';
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
		list.innerHTML='<div class="picker-item" style="cursor:default;color:var(--text-dim)">Failed to load directories.</div>';
	}
}

async function loadProjectFiles(dir){
	const list=$('fileList');
	list.innerHTML='<div class="picker-item" style="cursor:default;color:var(--text-dim)">Loading...</div>';
	$('addProjectDetails').style.display='none';
	$('addProjectConfirm').style.display='none';
	selectedFile='';
	try{
		const r=await fetch('/api/projects/'+encodeURIComponent(dir)+'/files',{headers:{'X-Rover-Secret':getToken()}});
		const files=await r.json();
		if(!files||files.length===0){
			list.innerHTML='<div class="picker-item" style="cursor:default;color:var(--text-dim)">No eligible files found.</div>';
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
		list.innerHTML='<div class="picker-item" style="cursor:default;color:var(--text-dim)">Failed to load files.</div>';
	}
}

async function validateAndAddProject(){
	if(!selectedDir||!selectedFile)return;
	const btn=$('addProjectConfirm');
	const status=$('addProjectStatus');
	btn.disabled=true;
	status.className='loading';
	status.textContent='Starting and validating project (max 15 seconds)...';
	try{
		const r=await fetch('/api/projects',{
			method:'POST',
			headers:{'Content-Type':'application/json','X-Rover-Secret':getToken()},
			body:JSON.stringify({name:selectedDir,start_cmd:$('addStartCmd').value})
		});
		const data=await r.json();
		if(!r.ok){
			status.className='error';
			status.textContent='Failed: '+(data.error||'unknown error');
			btn.disabled=false;
			return;
		}
		status.className='success';
		status.textContent='Project "'+esc(selectedDir)+'" added at '+esc(data.url)+' (port '+data.port+')';
		setTimeout(()=>{
			closeAddProjectModal();
			loadProjects();
		},1500);
	}catch(e){
		status.className='error';
		status.textContent='Connection error: '+e.message;
		btn.disabled=false;
	}
}

window.removeProject=async function(name){
	if(!confirm('Remove project "'+name+'" from the launcher?'))return;
	try{
		const r=await fetch('/api/projects/'+encodeURIComponent(name),{method:'DELETE',headers:{'X-Rover-Secret':getToken()}});
		if(!r.ok){
			const data=await r.json();
			alert('Failed to remove: '+(data.error||r.status));
			return;
		}
		loadProjects();
	}catch(e){
		alert('Connection error: '+e.message);
	}
};

checkAuth();
})();
</script>
</body>
</html>`)
