package cmd

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ylnhari/rover/internal/launcher"
	"github.com/ylnhari/rover/internal/rotatelog"
	"github.com/ylnhari/rover/internal/server"
)

const (
	operationalLogMaxBytes = 2 * 1024 * 1024
	operationalLogBackups  = 3
)

func defaultSessionsFile() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "sessions.json")
}

func defaultOperationalLogFile() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "rover.log")
}

func operationalLogOutput() (io.Writer, io.Closer) {
	path := defaultOperationalLogFile()
	if path == "" {
		return os.Stdout, nil
	}
	output, closer, err := newOperationalLogOutput(path, os.Stdout, operationalLogMaxBytes, operationalLogBackups)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: operational log unavailable at %s: %v\n", path, err)
		return os.Stdout, nil
	}
	fmt.Fprintf(output, "Operational log: %s\n", path)
	return output, closer
}

func newOperationalLogOutput(path string, console io.Writer, maxBytes int64, backups int) (io.Writer, io.Closer, error) {
	file, err := rotatelog.New(path, maxBytes, backups)
	if err != nil {
		return console, nil, err
	}
	// Keep the file first: a scheduled task may have an unusable stdout handle,
	// and MultiWriter stops at the first writer error.
	return io.MultiWriter(file, console), file, nil
}

func runServe(args []string) (runErr error) {
	fs := flag.NewFlagSet("rover serve", flag.ContinueOnError)
	addr := fs.String("addr", ":2278", "address to listen on (host:port)")
	secret := fs.String("secret", "", "shared secret for auth (or $ROVER_SECRET)")
	certFile := fs.String("tls-cert", "", "path to TLS certificate file")
	keyFile := fs.String("tls-key", "", "path to TLS private key file")
	execTimeout := fs.Duration("exec-timeout", 10*time.Minute, "max execution time per command (0 = no timeout)")
	maxOutput := fs.Int64("max-output", 1*1024*1024, "max output bytes per command (0 = no limit)")
	projectsDir := fs.String("projects-dir", "", "path to projects root (default: parent of rover directory)")
	allow := fs.String("allow", "", "comma-separated command prefixes to allow (empty = allow all); also applies to project start commands")
	logFormat := fs.String("log-format", "text", "log output format: text or json")
	noGuard := fs.Bool("no-command-guard", false, "allow interactive/GUI/stateful commands that normally can't work over rover (default: blocked)")
	proxyAuth := fs.String("proxy-auth", "auto", "globally require rover login for project proxies: auto|on|off; per-project requires_auth is always enforced")
	takeoverPort := fs.Bool("takeover-port", false, "if rover's own port is occupied, kill the listener instead of failing (default: fail and name the occupant)")
	validationTimeout := fs.Duration("validation-timeout", 30*time.Second, "how long project registration/start probes wait for the app to start listening")
	registry := fs.String("registry", "", "path to a ports.json-format port registry (or $ROVER_REGISTRY); when set it decides each project's port, matched by project path")

	if err := fs.Parse(args); err != nil {
		return err
	}

	logOutput, logCloser := operationalLogOutput()
	if logCloser != nil {
		defer logCloser.Close()
	}
	defer func() {
		if runErr != nil {
			fmt.Fprintf(logOutput, "rover: %v\n", runErr)
		}
	}()

	sec := *secret
	if sec == "" {
		sec = os.Getenv("ROVER_SECRET")
	}
	host, _, _ := net.SplitHostPort(*addr)
	if sec == "" {
		// Secret-less mode grants unauthenticated command execution, so it is
		// only permitted when rover is bound to loopback. Binding to any other
		// interface (including the all-interfaces default ":port") without a
		// secret would expose the host to the network and is refused.
		if isLoopbackBind(host) {
			fmt.Fprintln(logOutput, "WARNING: Running without a secret. Authentication is disabled, but rover is bound to loopback only.")
		} else {
			return fmt.Errorf("refusing to start without a secret while bound to %q: set --secret or $ROVER_SECRET, or bind to 127.0.0.1 for local-only use", *addr)
		}
	}

	var allowCmds []string
	if *allow != "" {
		for _, p := range strings.Split(*allow, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				allowCmds = append(allowCmds, p)
			}
		}
	}

	proxyAuthOn, err := resolveProxyAuth(*proxyAuth, host, sec)
	if err != nil {
		return err
	}
	if !proxyAuthOn && !isLoopbackBind(host) {
		fmt.Fprintf(logOutput, "WARNING: bound to %q with global proxy auth OFF — projects without requires_auth are reachable WITHOUT authentication on that network. Use --proxy-auth on, require auth per project, or bind to a Tailscale IP.\n", *addr)
	}

	if err := ensurePortFree(*addr, *takeoverPort, logOutput); err != nil {
		return err
	}

	projectsRoot := *projectsDir
	if projectsRoot == "" {
		exe, err := os.Executable()
		if err == nil {
			projectsRoot = filepath.Dir(filepath.Dir(exe))
		}
		if projectsRoot == "" || projectsRoot == "." {
			if wd, err := os.Getwd(); err == nil {
				projectsRoot = filepath.Dir(wd)
			}
		}
	}
	if projectsRoot != "" {
		if info, err := os.Stat(projectsRoot); err == nil && info.IsDir() {
			fmt.Fprintf(logOutput, "Projects root: %s\n", projectsRoot)
		} else {
			fmt.Fprintf(logOutput, "WARNING: Projects root %q not accessible, launcher disabled\n", projectsRoot)
			projectsRoot = ""
		}
	}

	portRegistry := *registry
	if portRegistry == "" {
		portRegistry = os.Getenv("ROVER_REGISTRY")
	}
	if portRegistry != "" {
		// Fail here rather than at the first start attempt: an operator who
		// pointed rover at a registry wants to know now if it is unreadable.
		if _, err := os.Stat(portRegistry); err != nil {
			return fmt.Errorf("--registry %s: %w", portRegistry, err)
		}
		fmt.Fprintf(logOutput, "Port registry:  %s\n", portRegistry)
	}

	return server.New(server.Config{
		Addr:                *addr,
		Secret:              sec,
		CertFile:            *certFile,
		KeyFile:             *keyFile,
		ExecTimeout:         *execTimeout,
		MaxOutput:           *maxOutput,
		ProjectsRoot:        projectsRoot,
		PortRegistry:        portRegistry,
		AllowCmds:           allowCmds,
		SessionsFile:        defaultSessionsFile(),
		LogFormat:           *logFormat,
		LogOutput:           logOutput,
		DisableCommandGuard: *noGuard,
		ProxyAuthOn:         proxyAuthOn,
		ValidationTimeout:   *validationTimeout,
	}).ListenAndServe()
}

// resolveProxyAuth turns the --proxy-auth mode into an effective on/off.
// "auto" = off only for a loopback bind, where nothing off-machine can reach
// the proxy at all. Anywhere reachable by another device - LAN, all-interfaces,
// or a Tailscale CGNAT address - resolves on.
//
// A tailnet used to be treated as trusted enough on the grounds that WireGuard
// device auth is the boundary. That reasoning does not survive contact with
// what the proxy actually carries: every device on the tailnet, every process
// on those devices, and any web page they open could reach a proxied app with
// no rover credential, while rover's own API correctly answered 401 over the
// same transport. The proxy was the weaker door to the more sensitive data.
func resolveProxyAuth(mode, host, secret string) (bool, error) {
	switch mode {
	case "on":
		if secret == "" {
			return false, fmt.Errorf("--proxy-auth on requires a secret")
		}
		return true, nil
	case "off":
		return false, nil
	case "auto":
		if secret == "" || isLoopbackBind(host) {
			return false, nil
		}
		return true, nil
	default:
		return false, fmt.Errorf("invalid --proxy-auth %q: use auto, on or off", mode)
	}
}

// isLoopbackBind reports whether host refers only to the local machine. An
// empty host (from an all-interfaces bind like ":2278") is NOT loopback.
func isLoopbackBind(host string) bool {
	switch host {
	case "localhost":
		return true
	case "":
		return false
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// isTailnetBind reports whether host is a Tailscale CGNAT address
// (100.64.0.0/10), where device-level WireGuard auth already gates access.
func isTailnetBind(host string) bool {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat.Contains(ip)
}

// ensurePortFree checks rover's own port. If occupied it names the listener
// and fails, unless --takeover-port explicitly authorizes killing it.
func ensurePortFree(addr string, takeover bool, output io.Writer) error {
	return ensurePortFreeUsing(addr, takeover, output, net.Listen, launcher.FindListenerOnPort, launcher.KillConfirmedListener, time.Sleep)
}

func ensurePortFreeUsing(
	addr string,
	takeover bool,
	output io.Writer,
	listen func(string, string) (net.Listener, error),
	findListener func(int) *launcher.Occupant,
	killListener func(int, int) error,
	wait func(time.Duration),
) error {
	ln, err := listen("tcp", addr)
	if err == nil {
		ln.Close()
		return nil
	}

	_, portStr, perr := net.SplitHostPort(addr)
	if perr != nil {
		return fmt.Errorf("port check: %w", err)
	}
	port, perr := strconv.Atoi(portStr)
	if perr != nil {
		return fmt.Errorf("port check: %w", err)
	}

	occ := findListener(port)
	if !takeover {
		if occ != nil {
			return fmt.Errorf("port %s is in use by %s — stop it, use a different --addr, or pass --takeover-port to kill it", addr, occ)
		}
		return fmt.Errorf("port %s is in use (listener could not be identified) — stop it or use a different --addr", addr)
	}

	if occ == nil {
		return fmt.Errorf("port %s is in use but the listener could not be identified; refusing to take over", addr)
	}
	fmt.Fprintf(output, "Port %s is in use by %s; --takeover-port set, killing it...\n", addr, occ)
	if err := killListener(port, occ.PID); err != nil {
		return fmt.Errorf("takeover failed: %w", err)
	}

	for i := 0; i < 5; i++ {
		ln, err = listen("tcp", addr)
		if err == nil {
			ln.Close()
			fmt.Fprintf(output, "Successfully freed port %s\n", addr)
			return nil
		}
		wait(200 * time.Millisecond)
	}
	return fmt.Errorf("port %s is still in use after takeover: %w", addr, err)
}
