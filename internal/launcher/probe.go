package launcher

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProbeResult is the structured outcome of checking whether something actually
// serves on a port: a TCP-connect poll until a listener appears, followed by
// one HTTP request. Any HTTP response (including 4xx/5xx) proves the listener
// speaks HTTP.
type ProbeResult struct {
	Listening bool  `json:"listening"`
	HTTP      bool  `json:"http"`
	Status    int   `json:"status,omitempty"`
	BootMs    int64 `json:"boot_ms"`
}

// ValidationReport is returned by ValidateProject on success and failure alike
// so callers (and the UI) can show why validation passed or failed.
type ValidationReport struct {
	Probe       ProbeResult `json:"probe"`
	DetectedURL string      `json:"detected_url,omitempty"`
	Port        int         `json:"port,omitempty"`
	ExitCode    *int        `json:"exit_code,omitempty"`
	LogTail     string      `json:"log_tail,omitempty"`
}

// probeServer polls TCP connect on 127.0.0.1:port until a listener appears or
// ctx is done, then issues a single HTTP GET to classify the listener.
func probeServer(ctx context.Context, port int) ProbeResult {
	var res ProbeResult
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	start := time.Now()

	d := net.Dialer{Timeout: 500 * time.Millisecond}
	for {
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			conn.Close()
			res.Listening = true
			break
		}
		select {
		case <-ctx.Done():
			return res
		case <-time.After(250 * time.Millisecond):
		}
	}
	res.BootMs = time.Since(start).Milliseconds()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		return res
	}
	client := &http.Client{
		Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err == nil {
		res.HTTP = true
		res.Status = resp.StatusCode
		resp.Body.Close()
	}
	return res
}

// tailBuffer is an io.Writer that keeps only the last max bytes written.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

var probeURLRe = regexp.MustCompile(`https?://\S+`)

// ValidateProject verifies a start command really brings up a server: it
// refuses an occupied port up front, launches the command (with PORT set),
// probes until something listens on the port, and classifies it with one HTTP
// request. If the process dies first, the report carries its exit code and a
// log tail instead of a vague timeout. The launched process is always torn
// down before returning.
func (m *Manager) ValidateProject(dir, startCmd string, port int) (*ValidationReport, error) {
	report := &ValidationReport{Port: port}

	if port > 0 && !portAvailable(port) {
		occ := FindListenerOnPort(port)
		if occ != nil {
			return report, fmt.Errorf("port %d is already in use by %s — stop it first, adopt it, or choose another port", port, occ)
		}
		return report, fmt.Errorf("port %d is already in use — stop the occupant or choose another port", port)
	}

	ctx, cancel := context.WithTimeout(context.Background(), m.probeTimeout)
	defer cancel()

	var shell, flag string
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	} else {
		shell, flag = "sh", "-c"
	}

	// Deliberately NOT CommandContext: context cancellation would kill only
	// the shell and orphan its children; finish() kills the whole process
	// group/tree while the parent is still alive.
	cmd := exec.Command(shell, flag, startCmd)
	cmd.Dir = dir
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

	tail := &tailBuffer{max: 4096}
	cmd.Stdout = tail
	cmd.Stderr = tail

	if err := cmd.Start(); err != nil {
		return report, fmt.Errorf("start: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	probeCh := make(chan ProbeResult, 1)
	go func() { probeCh <- probeServer(ctx, port) }()

	finish := func() {
		killChildProcesses(cmd)
		cancel()
		select {
		case <-waitCh:
		case <-time.After(5 * time.Second):
		}
	}

	select {
	case res := <-probeCh:
		report.Probe = res
		finish()
		report.LogTail = tail.String()
		if !res.Listening {
			return report, fmt.Errorf("validation timed out after %s: nothing listening on port %d (output tail: %s)", m.probeTimeout, port, strings.TrimSpace(report.LogTail))
		}
		report.DetectedURL = detectURLFromLogs(report.LogTail)
		if report.DetectedURL == "" {
			report.DetectedURL = fmt.Sprintf("http://127.0.0.1:%d", port)
		}
		return report, nil
	case err := <-waitCh:
		cancel()
		report.LogTail = tail.String()
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		report.ExitCode = &code
		return report, fmt.Errorf("process exited with code %d before listening on port %d (output tail: %s)", code, port, strings.TrimSpace(report.LogTail))
	}
}

// taskValidationGrace is how long ValidateTask watches a port-less command for
// an early failure before declaring it viable.
const taskValidationGrace = 3 * time.Second

// ValidateTask verifies a port-less project's command at least starts: it is
// launched and watched for a short grace window. Exiting non-zero within it
// fails registration (with the log tail); exiting 0 (a one-shot script) or
// still running when the window closes (a long-lived worker) passes. There is
// no port to probe, so this is the strongest check available without guessing
// what the task is supposed to do. The launched process is always torn down.
func (m *Manager) ValidateTask(dir, startCmd string) (*ValidationReport, error) {
	report := &ValidationReport{}

	var shell, flag string
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	} else {
		shell, flag = "sh", "-c"
	}

	// Like ValidateProject, deliberately NOT CommandContext: killChildProcesses
	// must take down the whole tree, not just the shell.
	cmd := exec.Command(shell, flag, startCmd)
	cmd.Dir = dir
	setProcessGroup(cmd)
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env,
		"PYTHONUNBUFFERED=1",
		"PYTHONIOENCODING=utf-8",
		"PYTHONLEGACYWINDOWSSTDIO=utf-8",
	)

	tail := &tailBuffer{max: 4096}
	cmd.Stdout = tail
	cmd.Stderr = tail

	if err := cmd.Start(); err != nil {
		return report, fmt.Errorf("start: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case err := <-waitCh:
		report.LogTail = tail.String()
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		report.ExitCode = &code
		if code != 0 {
			return report, fmt.Errorf("task exited with code %d during validation (output tail: %s)", code, strings.TrimSpace(report.LogTail))
		}
		return report, nil
	case <-time.After(taskValidationGrace):
		killChildProcesses(cmd)
		select {
		case <-waitCh:
		case <-time.After(5 * time.Second):
		}
		report.LogTail = tail.String()
		return report, nil
	}
}

// detectURLFromLogs returns the first URL-looking string in the output, if any.
func detectURLFromLogs(logs string) string {
	match := probeURLRe.FindString(logs)
	if match == "" {
		return ""
	}
	return strings.TrimRight(match, ".,;:!?)}>]\"'`")
}
