package cmd

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ylnhari/rover/internal/launcher"
	"github.com/ylnhari/rover/internal/server"
)

func defaultSessionsFile() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "sessions.json")
}

func runServe(args []string) error {
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
	proxyAuth := fs.String("proxy-auth", "auto", "require rover login for project proxies: auto|on|off (auto = off on loopback/tailnet binds, on elsewhere)")
	takeoverPort := fs.Bool("takeover-port", false, "if rover's own port is occupied, kill the listener instead of failing (default: fail and name the occupant)")
	validationTimeout := fs.Duration("validation-timeout", 30*time.Second, "how long project registration/start probes wait for the app to start listening")
	registry := fs.String("registry", "", "path to a ports.json-format port registry (or $ROVER_REGISTRY); when set it decides each project's port, matched by project path")

	if err := fs.Parse(args); err != nil {
		return err
	}

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
			fmt.Println("WARNING: Running without a secret. Authentication is disabled, but rover is bound to loopback only.")
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
	if !proxyAuthOn && !isLoopbackBind(host) && !isTailnetBind(host) {
		fmt.Printf("WARNING: bound to %q with proxy auth OFF — every proxied project is reachable WITHOUT authentication on that network. Use --proxy-auth on, or bind to a Tailscale IP.\n", *addr)
	}

	if err := ensurePortFree(*addr, *takeoverPort); err != nil {
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
			fmt.Printf("Projects root: %s\n", projectsRoot)
		} else {
			fmt.Printf("WARNING: Projects root %q not accessible, launcher disabled\n", projectsRoot)
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
		fmt.Printf("Port registry:  %s\n", portRegistry)
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
		DisableCommandGuard: *noGuard,
		ProxyAuthOn:         proxyAuthOn,
		ValidationTimeout:   *validationTimeout,
	}).ListenAndServe()
}

// resolveProxyAuth turns the --proxy-auth mode into an effective on/off.
// "auto" = off when the proxies can only be reached from trusted networks
// (loopback, or a Tailscale CGNAT address where the tailnet's WireGuard device
// auth is the boundary), on everywhere else (LAN / all-interfaces binds).
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
		if secret == "" || isLoopbackBind(host) || isTailnetBind(host) {
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
func ensurePortFree(addr string, takeover bool) error {
	ln, err := net.Listen("tcp", addr)
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

	occ := launcher.FindListenerOnPort(port)
	if !takeover {
		if occ != nil {
			return fmt.Errorf("port %s is in use by %s — stop it, use a different --addr, or pass --takeover-port to kill it", addr, occ)
		}
		return fmt.Errorf("port %s is in use (listener could not be identified) — stop it or use a different --addr", addr)
	}

	if occ == nil {
		return fmt.Errorf("port %s is in use but the listener could not be identified; refusing to take over", addr)
	}
	fmt.Printf("Port %s is in use by %s; --takeover-port set, killing it...\n", addr, occ)
	if err := launcher.KillConfirmedListener(port, occ.PID); err != nil {
		return fmt.Errorf("takeover failed: %w", err)
	}

	for i := 0; i < 5; i++ {
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			fmt.Printf("Successfully freed port %s\n", addr)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("port %s is still in use after takeover: %w", addr, err)
}
