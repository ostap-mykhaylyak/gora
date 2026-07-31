// Command gora is a MySQL proxy for WordPress and WooCommerce.
package main

import (
	"fmt"
	"io"
	"os"
)

const (
	defaultConfigPath = "/etc/gora/config.yaml"

	binaryPath    = "/sbin/gora"
	servicePath   = "/etc/systemd/system/gora.service"
	logrotatePath = "/etc/logrotate.d/gora"
	logDir        = "/var/log/gora"
	runtimeDir    = "/run/gora"
	pidFilePath   = runtimeDir + "/gora.pid"
	systemUser    = "gora"
)

// Set at build time via -ldflags (see Makefile and .goreleaser.yaml).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main with the process boundaries injected, so every path through
// the command line is reachable from a test.
func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, "gora:", err)
		fmt.Fprint(stderr, "\n", usage)
		return 2
	}

	switch {
	case opts.help:
		fmt.Fprint(stdout, usage)
		return 0
	case opts.version:
		fmt.Fprintf(stdout, "gora %s (commit %s, built %s)\n", version, commit, date)
		return 0
	}

	switch {
	case opts.install:
		err = install(opts.configPath, stdout)
	case opts.checkConfig:
		err = checkConfig(opts.configPath, stdout)
	case opts.advice:
		err = printAdvice(opts.configPath, stdout)
	default:
		switch opts.command {
		case "start":
			err = start(opts.configPath, stdout)
		case "stop":
			err = stop(stdout)
		case "restart":
			err = restart(opts.configPath, stdout)
		case "reload":
			err = reload(stdout)
		case "status":
			err = printStatus(opts.configPath, stdout)
		default:
			// No command and no action: the user is looking around.
			fmt.Fprint(stdout, usage)
			return 0
		}
	}

	if err != nil {
		fmt.Fprintln(stderr, "gora:", err)
		return 1
	}
	return 0
}
