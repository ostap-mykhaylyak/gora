package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ostap-mykhaylyak/gora/internal/config"
)

// Changing settings from the command line.
//
// The configuration file stays the truth: these commands edit it, they do
// not keep a second copy of it somewhere. What they add over an editor is
// that the result is validated before it is written — a file gora wrote is
// a file gora can start with — and that a running instance is told to
// re-read it, so the settings that can change under a live proxy do.

// setSetting applies one `path=value` change.
func setSetting(configPath, assignment string, stdout io.Writer) error {
	path, value, ok := strings.Cut(assignment, "=")
	if !ok {
		return fmt.Errorf("--set takes path=value, for example --set cache.default_ttl=10m")
	}
	path = strings.TrimSpace(path)

	// A password on the command line is a password in the shell history.
	if value == "-" {
		read, err := readSecret(stdout, path)
		if err != nil {
			return err
		}
		value = read
	}

	data, cfg, err := readConfig(configPath)
	if err != nil {
		return err
	}
	setting, known := config.Lookup(cfg, path)
	if !known {
		return fmt.Errorf("there is no setting called %q (see gora --settings)", path)
	}
	if err := config.CheckValue(setting, value); err != nil {
		return err
	}

	edited, err := config.SetValue(data, path, value)
	if err != nil {
		return err
	}
	updated, err := config.Parse(edited, configPath)
	if err != nil {
		// The edit produced something gora would refuse to start with, so
		// it does not reach the disk.
		return fmt.Errorf("that change would make the configuration invalid, so it was not written: %w", err)
	}
	if err := writeConfig(configPath, edited); err != nil {
		return err
	}

	shown := value
	if isSecret(path) {
		shown = "(hidden)"
	}
	fmt.Fprintf(stdout, "%s = %s\n", path, shown)
	reportEffect(setting, updated, stdout)
	return nil
}

// unsetSetting removes a setting from the file, so it goes back to its
// default.
func unsetSetting(configPath, path string, stdout io.Writer) error {
	data, cfg, err := readConfig(configPath)
	if err != nil {
		return err
	}
	setting, known := config.Lookup(cfg, path)
	if !known {
		return fmt.Errorf("there is no setting called %q (see gora --settings)", path)
	}

	edited, err := config.DeleteValue(data, path)
	if err != nil {
		return err
	}
	updated, err := config.Parse(edited, configPath)
	if err != nil {
		return fmt.Errorf("removing that setting would make the configuration invalid, so it was not written: %w", err)
	}
	if err := writeConfig(configPath, edited); err != nil {
		return err
	}

	back, _ := config.Lookup(updated, path)
	fmt.Fprintf(stdout, "%s removed; back to the default (%s)\n", path, back.Value)
	reportEffect(setting, updated, stdout)
	return nil
}

// getSetting prints one value.
func getSetting(configPath, path string, stdout io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	setting, ok := config.Lookup(cfg, path)
	if !ok {
		return fmt.Errorf("there is no setting called %q (see gora --settings)", path)
	}
	if isSecret(path) {
		fmt.Fprintln(stdout, "(hidden)")
		return nil
	}
	fmt.Fprintln(stdout, setting.Value)
	return nil
}

// listSettings prints every setting, its value, and whether changing it
// needs a restart.
func listSettings(configPath string, stdout io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "%-34s %-28s %s\n", "SETTING", "VALUE", "APPLIED")
	for _, s := range config.Settings(cfg) {
		value := s.Value
		if isSecret(s.Path) && value != "" {
			value = "(hidden)"
		}
		if len(value) > 27 {
			value = value[:26] + "…"
		}
		applied := "on restart"
		switch {
		case s.List:
			applied = "see --help"
		case s.Hot:
			applied = "on reload"
		}
		fmt.Fprintf(stdout, "%-34s %-28s %s\n", s.Path, value, applied)
	}
	fmt.Fprintln(stdout, "\nChange one with: gora --set <setting>=<value>")
	fmt.Fprintln(stdout, "A password can be typed instead of shown: gora --set backend.password=-")
	return nil
}

// addUser adds an account clients authenticate with, reading the password
// from the terminal rather than the command line.
func addUser(configPath, username string, stdout io.Writer) error {
	data, cfg, err := readConfig(configPath)
	if err != nil {
		return err
	}
	for _, u := range cfg.Users {
		if u.Username == username {
			return fmt.Errorf("%q is already a user; remove it first to change its password", username)
		}
	}

	password, err := readSecret(stdout, "the password for "+username)
	if err != nil {
		return err
	}

	edited, err := config.AddUser(data, username, password)
	if err != nil {
		return err
	}
	if _, err := config.Parse(edited, configPath); err != nil {
		return fmt.Errorf("that change would make the configuration invalid, so it was not written: %w", err)
	}
	if err := writeConfig(configPath, edited); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "user %s added\n", username)
	fmt.Fprintln(stdout, "Clients authenticate against gora with it; MySQL never sees it.")
	fmt.Fprintln(stdout, "It takes effect at the next restart: existing sessions are not re-authenticated.")
	return nil
}

// removeUser takes an account out.
func removeUser(configPath, username string, stdout io.Writer) error {
	data, cfg, err := readConfig(configPath)
	if err != nil {
		return err
	}
	found := false
	for _, u := range cfg.Users {
		if u.Username == username {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("%q is not a configured user", username)
	}

	edited, err := config.RemoveUser(data, username)
	if err != nil {
		return err
	}
	if _, err := config.Parse(edited, configPath); err != nil {
		return fmt.Errorf("that change would make the configuration invalid, so it was not written: %w", err)
	}
	if err := writeConfig(configPath, edited); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "user %s removed; it takes effect at the next restart\n", username)
	return nil
}

// --- shared plumbing ---

func readConfig(configPath string) ([]byte, config.Config, error) {
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return nil, config.Config{}, fmt.Errorf("%s does not exist yet: run `sudo gora --init` first", configPath)
	}
	if err != nil {
		return nil, config.Config{}, err
	}
	cfg, err := config.Parse(data, configPath)
	if err != nil {
		return nil, config.Config{}, err
	}
	return data, cfg, nil
}

// writeConfig replaces the file through a rename, keeping the mode and
// ownership it had: this file holds the database password, and a rewrite
// that widened its permissions would be a quiet way to leak it.
func writeConfig(configPath string, data []byte) error {
	mode := os.FileMode(0o640)
	if info, err := os.Stat(configPath); err == nil {
		mode = info.Mode().Perm()
	}

	tmp := configPath + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := copyOwner(configPath, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, configPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", configPath, err)
	}
	return nil
}

// reportEffect says whether the change is in force now or at the next
// restart, and asks a running gora to re-read the file when it can be.
func reportEffect(setting config.Setting, cfg config.Config, stdout io.Writer) {
	pid, running := runningPID()
	if !setting.Hot {
		if running {
			fmt.Fprintln(stdout, "It takes effect at the next restart: systemctl restart gora")
		} else {
			fmt.Fprintln(stdout, "It takes effect the next time gora starts.")
		}
		return
	}
	if !running {
		fmt.Fprintln(stdout, "It takes effect the next time gora starts.")
		return
	}
	if err := hangup(pid); err != nil {
		fmt.Fprintf(stdout, "gora is running but could not be told to re-read the file (%v);\n"+
			"apply it with: systemctl reload gora\n", err)
		return
	}
	fmt.Fprintf(stdout, "gora (pid %d) has been told to re-read the file; it is in force now.\n", pid)
}

// isSecret reports whether a setting is one that should not be printed
// back. Showing a password because somebody asked for the value of every
// setting is a way to put it in a terminal recording.
func isSecret(path string) bool {
	return strings.HasSuffix(path, "password") || strings.HasSuffix(path, "_password")
}

// readSecret reads a value from the terminal. It is not hidden — that needs
// a terminal library gora does not carry — but it keeps the value out of
// the shell history, which is where it would otherwise sit forever.
func readSecret(stdout io.Writer, what string) (string, error) {
	fmt.Fprintf(stdout, "Type %s, then Enter: ", what)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading the value: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}
