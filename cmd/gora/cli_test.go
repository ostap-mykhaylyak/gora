package main

import (
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want options
	}{
		{
			name: "no arguments",
			args: nil,
			want: options{configPath: defaultConfigPath},
		},
		{
			name: "command alone",
			args: []string{"start"},
			want: options{command: "start", configPath: defaultConfigPath},
		},
		{
			name: "command with a config path",
			args: []string{"start", "--config", "/tmp/gora.yaml"},
			want: options{command: "start", configPath: "/tmp/gora.yaml"},
		},
		{
			name: "options may come before the command",
			args: []string{"--config=/tmp/gora.yaml", "reload"},
			want: options{command: "reload", configPath: "/tmp/gora.yaml"},
		},
		{
			name: "install",
			args: []string{"--init"},
			want: options{install: true, configPath: defaultConfigPath},
		},
		{
			name: "check config",
			args: []string{"--check-config", "--config", "/etc/other.yaml"},
			want: options{checkConfig: true, configPath: "/etc/other.yaml"},
		},
		{
			name: "version",
			args: []string{"--version"},
			want: options{version: true, configPath: defaultConfigPath},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseArgs(tt.args)
			if err != nil {
				t.Fatalf("parseArgs(%q) returned %v", tt.args, err)
			}
			if got != tt.want {
				t.Fatalf("parseArgs(%q) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

func TestParseArgsErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"single dash is refused", []string{"-config", "/tmp/x.yaml"}},
		{"single dash on a long name", []string{"-version"}},
		{"unknown option", []string{"--verbose"}},
		{"unknown command", []string{"begin"}},
		{"two commands", []string{"start", "stop"}},
		{"config without a value", []string{"--config"}},
		{"empty config value", []string{"--config="}},
		{"boolean option with a value", []string{"--init=true"}},
		{"command with init", []string{"--init", "start"}},
		{"init with check-config", []string{"--init", "--check-config"}},
		{"bare dash", []string{"-"}},
		{"double dash alone", []string{"--"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseArgs(tt.args); err == nil {
				t.Fatalf("parseArgs(%q) accepted an invalid command line", tt.args)
			}
		})
	}
}

// The single-dash message must name the exact replacement, because that is
// the whole point of rejecting it instead of accepting it silently.
func TestParseArgsSingleDashMessage(t *testing.T) {
	_, err := parseArgs([]string{"-config", "/tmp/x.yaml"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if want := "--config"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not suggest %q", err, want)
	}
}
