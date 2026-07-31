package main

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed config.default.yaml
var defaultConfig []byte

//go:embed woocommerce.default.yaml
var defaultWooCommerceRules []byte

//go:embed gora.service
var serviceUnit string

//go:embed gora.logrotate
var logrotateConf []byte

// install sets gora up as a systemd service, ready to start: it copies the
// running binary to /sbin/gora, creates the gora system user and the config,
// conf.d, log and runtime directories, and writes the configuration, the
// systemd unit and the logrotate rules.
//
// Everything gora manages is rewritten on every run, so `gora --init` is
// also the upgrade procedure: download the new binary, run it, restart.
// The one thing that is never thrown away is the configuration you edited:
// an existing config.yaml is copied to config.yaml.bak first.
func install(configPath string, stdout io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("--init installs a system service and must run as root (try: sudo gora --init)")
	}

	uid, gid, err := ensureUser(systemUser)
	if err != nil {
		return err
	}

	if err := installBinary(binaryPath); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "installed binary:", binaryPath)

	confDir := filepath.Dir(configPath)
	confD := filepath.Join(confDir, "conf.d")
	if err := os.MkdirAll(confD, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", confD, err)
	}
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", logDir, err)
	}
	if err := os.Chown(logDir, uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", logDir, err)
	}

	if backup, ok, err := backupConfig(configPath); err != nil {
		return err
	} else if ok {
		fmt.Fprintln(stdout, "kept a copy of the previous configuration:", backup)
	}

	// 0640 and gora-owned: the file holds the database password, so it is
	// readable by the service and by root, and by nobody else.
	if err := writeFile(configPath, defaultConfig, 0o640); err != nil {
		return err
	}
	if err := os.Chown(configPath, uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", configPath, err)
	}
	// The WooCommerce profile is a drop-in, not part of config.yaml: it is
	// meant to be edited, reloaded and, if a shop needs something else,
	// replaced by a file of your own next to it.
	if err := writeFile(filepath.Join(confD, "woocommerce.yaml"), defaultWooCommerceRules, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "wrote configuration:", configPath)
	fmt.Fprintln(stdout, "wrote cache rules:  ", filepath.Join(confD, "woocommerce.yaml"))

	// The unit's ExecStart follows --config, so a non-default path keeps
	// working after a reinstall.
	unit := strings.ReplaceAll(serviceUnit, defaultConfigPath, configPath)
	if err := writeFile(servicePath, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := writeFile(logrotatePath, logrotateConf, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "installed service:", servicePath)

	if err := reloadSystemd(); err != nil {
		fmt.Fprintln(stdout, "warning:", err)
	}

	fmt.Fprintln(stdout, "\nEdit", configPath, "then start gora with:")
	fmt.Fprintln(stdout, "  systemctl enable --now gora")
	return nil
}

// backupConfig copies an existing configuration next to itself before it is
// overwritten. It reports whether there was anything to back up.
func backupConfig(configPath string) (string, bool, error) {
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", configPath, err)
	}
	backup := configPath + ".bak"
	if err := os.WriteFile(backup, data, 0o640); err != nil {
		return "", false, fmt.Errorf("writing %s: %w", backup, err)
	}
	return backup, true, nil
}

// writeFile writes data to path, replacing any existing file and enforcing
// the mode (WriteFile keeps the old one when the file already exists).
func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := os.WriteFile(path, data, perm); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Chmod(path, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// installBinary copies the running executable to dst through a temporary
// file and a rename, so replacing the binary while the service is running
// cannot fail with "text file busy" and can never leave a half-written file
// in place of a working one.
func installBinary(dst string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the running binary: %w", err)
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return fmt.Errorf("resolving the running binary: %w", err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return fmt.Errorf("reading the running binary: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(dst), err)
	}
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("installing %s: %w", dst, err)
	}
	return nil
}

// ensureUser creates the unprivileged system user gora runs as, and returns
// its uid and gid.
func ensureUser(name string) (int, int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		if _, ok := err.(user.UnknownUserError); !ok {
			return 0, 0, fmt.Errorf("looking up user %s: %w", name, err)
		}
		cmd := exec.Command("useradd", "--system", "--no-create-home",
			"--shell", "/usr/sbin/nologin", name)
		if out, err := cmd.CombinedOutput(); err != nil {
			return 0, 0, fmt.Errorf("creating user %s: %w: %s", name, err, out)
		}
		if u, err = user.Lookup(name); err != nil {
			return 0, 0, fmt.Errorf("looking up new user %s: %w", name, err)
		}
	}

	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("parsing the uid of %s: %w", name, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("parsing the gid of %s: %w", name, err)
	}
	return uid, gid, nil
}

// reloadSystemd picks up the unit just written. A missing systemctl (a
// container, a distribution without systemd) is reported, not fatal: the
// binary and the configuration are installed either way.
func reloadSystemd() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not found, skipping daemon-reload (run gora start yourself)")
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload failed: %w: %s", err, out)
	}
	return nil
}
