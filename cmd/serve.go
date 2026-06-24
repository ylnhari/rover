package cmd

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

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
	allow := fs.String("allow", "", "comma-separated command prefixes to allow (empty = allow all)")
	logFormat := fs.String("log-format", "text", "log output format: text or json")
	noGuard := fs.Bool("no-command-guard", false, "allow interactive/GUI/stateful commands that normally can't work over rover (default: blocked)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	sec := *secret
	if sec == "" {
		sec = os.Getenv("ROVER_SECRET")
	}
	if sec == "" {
		fmt.Println("WARNING: Running in secret-less mode. No authentication required.")
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

	if err := ensurePortFree(*addr); err != nil {
		return fmt.Errorf("failed to free port: %w", err)
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

	return server.New(server.Config{
		Addr:         *addr,
		Secret:       sec,
		CertFile:     *certFile,
		KeyFile:      *keyFile,
		ExecTimeout:  *execTimeout,
		MaxOutput:    *maxOutput,
		ProjectsRoot:        projectsRoot,
		AllowCmds:           allowCmds,
		SessionsFile:        defaultSessionsFile(),
		LogFormat:           *logFormat,
		DisableCommandGuard: *noGuard,
	}).ListenAndServe()
}

func ensurePortFree(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		ln.Close()
		return nil
	}

	fmt.Printf("Port %s is in use, attempting to kill the process...\n", addr)
	if err := killProcessOnPort(addr); err != nil {
		return fmt.Errorf("failed to kill process on %s: %w", addr, err)
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
	return fmt.Errorf("port %s is still in use after kill attempt: %w", addr, err)
}

func killProcessOnPort(addr string) error {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}

	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("netstat", "-ano")
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("netstat failed: %w", err)
		}
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "LISTENING") && strings.Contains(line, ":"+portStr+" ") {
				fields := strings.Fields(line)
				if len(fields) > 4 {
					pid := fields[len(fields)-1]
					kill := exec.Command("taskkill", "/F", "/PID", pid)
					kill.Stdout = os.Stdout
					kill.Stderr = os.Stderr
					if err := kill.Run(); err != nil {
						return fmt.Errorf("failed to kill PID %s: %w", pid, err)
					}
					fmt.Printf("Killed process PID %s on port %s\n", pid, portStr)
					return nil
				}
			}
		}
		return fmt.Errorf("no process found listening on port %s", portStr)
	default:
		cmd := exec.Command("lsof", "-ti", ":"+portStr)
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("no process found on port %s: %w", portStr, err)
		}
		pids := strings.Fields(string(out))
		if len(pids) == 0 {
			return fmt.Errorf("no process found on port %s", portStr)
		}
		for _, pid := range pids {
			pid = strings.TrimSpace(pid)
			if pid == "" || pid == strconv.Itoa(os.Getpid()) {
				continue
			}
			kill := exec.Command("kill", "-9", pid)
			kill.Stdout = os.Stdout
			kill.Stderr = os.Stderr
			if err := kill.Run(); err != nil {
				return fmt.Errorf("failed to kill PID %s: %w", pid, err)
			}
			fmt.Printf("Killed process PID %s on port %s\n", pid, portStr)
		}
		return nil
	}
}
