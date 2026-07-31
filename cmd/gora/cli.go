package main

import (
	"fmt"
	"strings"
)

// gora's command line has one rule, and it is worth stating in the code
// because it is the thing users get wrong: service verbs are bare words
// (start, stop, restart, reload, status), everything else is an option and
// always takes two dashes (--config, --init, --check-config, --version).
// A single dash is rejected on purpose instead of being quietly accepted,
// so muscle memory from other tools fails loudly and gets corrected once.

const usage = `gora - MySQL proxy for WordPress and WooCommerce

Usage:
  gora <command> [options]
  gora --init [--config <path>]

Commands (no dashes):
  start           run gora in the foreground; this is what systemd calls
  stop            stop the running instance (SIGTERM) and wait for it
  restart         stop the running instance, then run in the foreground
  reload          make the running instance re-read its configuration (SIGHUP)
  status          print the state of the running instance

Options (always two dashes):
  --config <path> configuration file (default /etc/gora/config.yaml)
  --init          install gora as a systemd service and exit
  --check-config  validate the configuration and exit
  --advice        print what the profiler has suggested, and exit
  --version       print the version and exit
  --help          print this help and exit
`

// commands are the service verbs, in the order they appear in the usage.
var commands = []string{"start", "stop", "restart", "reload", "status"}

type options struct {
	command     string
	configPath  string
	install     bool
	checkConfig bool
	advice      bool
	version     bool
	help        bool
}

// parseArgs turns the command line into options. It never exits the process
// and never writes anything, so the whole surface is testable.
func parseArgs(args []string) (options, error) {
	opts := options{configPath: defaultConfigPath}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case strings.HasPrefix(arg, "--"):
			name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
			switch name {
			case "config":
				if !hasValue {
					i++
					if i >= len(args) {
						return opts, fmt.Errorf("--config needs a path")
					}
					value = args[i]
				}
				if value == "" {
					return opts, fmt.Errorf("--config needs a path")
				}
				opts.configPath = value
			case "init":
				if err := noValue(name, hasValue); err != nil {
					return opts, err
				}
				opts.install = true
			case "check-config":
				if err := noValue(name, hasValue); err != nil {
					return opts, err
				}
				opts.checkConfig = true
			case "advice":
				if err := noValue(name, hasValue); err != nil {
					return opts, err
				}
				opts.advice = true
			case "version":
				if err := noValue(name, hasValue); err != nil {
					return opts, err
				}
				opts.version = true
			case "help":
				if err := noValue(name, hasValue); err != nil {
					return opts, err
				}
				opts.help = true
			case "":
				return opts, fmt.Errorf(`"--" alone is not an option`)
			default:
				return opts, fmt.Errorf("unknown option --%s", name)
			}

		case strings.HasPrefix(arg, "-"):
			// Single dash: tell the user the exact string to type instead.
			return opts, fmt.Errorf("options take two dashes: write --%s, not %s",
				strings.TrimLeft(arg, "-"), arg)

		default:
			if !isCommand(arg) {
				return opts, fmt.Errorf("unknown command %q (expected one of: %s)",
					arg, strings.Join(commands, ", "))
			}
			if opts.command != "" {
				return opts, fmt.Errorf("only one command at a time (got %q and %q)",
					opts.command, arg)
			}
			opts.command = arg
		}
	}

	if opts.command != "" && (opts.install || opts.checkConfig || opts.advice) {
		action := "--init"
		switch {
		case opts.checkConfig:
			action = "--check-config"
		case opts.advice:
			action = "--advice"
		}
		return opts, fmt.Errorf("%s does not take a command (drop %q)", action, opts.command)
	}
	if opts.install && opts.checkConfig {
		return opts, fmt.Errorf("--init and --check-config do different things: run one at a time")
	}

	return opts, nil
}

func noValue(name string, hasValue bool) error {
	if hasValue {
		return fmt.Errorf("--%s takes no value", name)
	}
	return nil
}

func isCommand(s string) bool {
	for _, c := range commands {
		if c == s {
			return true
		}
	}
	return false
}
