package cmd

import (
	"fmt"
	"os"
)

func Execute() error {
	if len(os.Args) < 2 {
		printHelp()
		return nil
	}
	switch os.Args[1] {
	case "serve":
		return runServe(os.Args[2:])
	case "version", "--version", "-V":
		fmt.Printf("rover dev\n")
		return nil
	case "help", "--help", "-h":
		printHelp()
		return nil
	default:
		fmt.Fprintf(os.Stderr, "rover: unknown command %q\n\n", os.Args[1])
		printHelp()
		return fmt.Errorf("unknown command: %s", os.Args[1])
	}
}

func printHelp() {
	fmt.Print(`Rover — Remote Command Executor & Project Launcher

USAGE:
  rover <command> [flags]

COMMANDS:
  serve    Start the rover web server (remote exec + project launcher)
  version  Print version information
  help     Show this help message

SERVE FLAGS:
  --addr           host:port   listen address                         (default: :2278)
  --secret         string      shared secret (or $ROVER_SECRET)
  --allow          string      comma-separated command prefix allowlist (empty = allow all)
  --tls-cert       path        TLS certificate file
  --tls-key        path        TLS private key file
  --exec-timeout   duration    max run time per command               (default: 10m)
  --max-output     int         max output bytes per command           (default: 1MB)
  --projects-dir   path        projects root directory
  --log-format     text|json   log output format                      (default: text)

QUICK START:
  # No auth (trusted network only)
  rover serve

  # With secret
  ROVER_SECRET=$(openssl rand -hex 32) rover serve

  # Restrict which commands may run
  rover serve --allow "git,go test,npm"

  # TLS
  rover serve --tls-cert cert.pem --tls-key key.pem --secret $SECRET
`)
}
