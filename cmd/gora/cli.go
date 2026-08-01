package main

import (
	"fmt"
	"sort"
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
  top             watch what the running instance is doing, refreshed live

Options (always two dashes):
  --config <path> configuration file (default /etc/gora/config.yaml)
  --init          install gora as a systemd service and exit
  --check-config  validate the configuration and exit
  --advice        print what the profiler has suggested, and exit
  --json          with status: print the raw snapshot instead of a report
  --settings              list every setting, its value and when it applies
  --get <setting>         print one setting
  --set <setting>=<value> change one setting (a value of - is read from stdin)
  --unset <setting>       remove one setting, back to its default
  --add-user <name>       add an account clients authenticate with
  --remove-user <name>    remove one
  --init-cluster  configure the servers into a replicating cluster and exit
  --add-replica <addr>    bring a node into the cluster, without a restart
  --remove-replica <addr> take a node out of the cluster, without a restart
  --promote <addr>        make that node the primary and exit
  --version       print the version and exit
  --help          print this help and exit
`

// commands are the service verbs, in the order they appear in the usage.
var commands = []string{"start", "stop", "restart", "reload", "status", "top"}

type options struct {
	command       string
	configPath    string
	install       bool
	checkConfig   bool
	advice        bool
	initCluster   bool
	addReplica    string
	removeReplica string
	promote       string
	asJSON        bool
	settings      bool
	get           string
	set           string
	unset         string
	addUser       string
	removeUser    string
	version       bool
	help          bool
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
			case "settings":
				if err := noValue(name, hasValue); err != nil {
					return opts, err
				}
				opts.settings = true
			case "get", "set", "unset", "add-user", "remove-user":
				if !hasValue {
					i++
					if i >= len(args) {
						return opts, fmt.Errorf("--%s needs a value", name)
					}
					value = args[i]
				}
				if value == "" {
					return opts, fmt.Errorf("--%s needs a value", name)
				}
				switch name {
				case "get":
					opts.get = value
				case "set":
					opts.set = value
				case "unset":
					opts.unset = value
				case "add-user":
					opts.addUser = value
				case "remove-user":
					opts.removeUser = value
				}
			case "json":
				if err := noValue(name, hasValue); err != nil {
					return opts, err
				}
				opts.asJSON = true
			case "init-cluster":
				if err := noValue(name, hasValue); err != nil {
					return opts, err
				}
				opts.initCluster = true
			case "promote", "add-replica", "remove-replica":
				if !hasValue {
					i++
					if i >= len(args) {
						return opts, fmt.Errorf("--%s needs a node address", name)
					}
					value = args[i]
				}
				if value == "" {
					return opts, fmt.Errorf("--%s needs a node address", name)
				}
				switch name {
				case "promote":
					opts.promote = value
				case "add-replica":
					opts.addReplica = value
				case "remove-replica":
					opts.removeReplica = value
				}
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

	actions := map[string]bool{
		"--init":           opts.install,
		"--check-config":   opts.checkConfig,
		"--advice":         opts.advice,
		"--init-cluster":   opts.initCluster,
		"--promote":        opts.promote != "",
		"--add-replica":    opts.addReplica != "",
		"--remove-replica": opts.removeReplica != "",
		"--settings":       opts.settings,
		"--get":            opts.get != "",
		"--set":            opts.set != "",
		"--unset":          opts.unset != "",
		"--add-user":       opts.addUser != "",
		"--remove-user":    opts.removeUser != "",
	}
	var chosen []string
	for name, on := range actions {
		if on {
			chosen = append(chosen, name)
		}
	}
	sort.Strings(chosen)
	if len(chosen) > 1 {
		return opts, fmt.Errorf("%s do different things: run one at a time", strings.Join(chosen, " and "))
	}
	if opts.command != "" && len(chosen) == 1 {
		return opts, fmt.Errorf("%s does not take a command (drop %q)", chosen[0], opts.command)
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
