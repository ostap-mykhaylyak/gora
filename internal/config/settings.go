package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// The settings catalogue.
//
// Every field of Config is reachable by its dotted path — "cache.default_ttl"
// — which is what the command line sets, gets and lists. The catalogue is
// built by reflection over the same yaml tags the parser uses, so a setting
// cannot exist in the file and be missing from the tooling, or the other
// way round.
//
// A field tagged `hot:"true"` is one a running gora applies on reload.
// Everything else is written now and takes effect at the next restart, and
// the difference is reported rather than left to be discovered.

// Setting is one configurable value.
type Setting struct {
	Path  string
	Kind  string
	Value string
	Hot   bool
	// List is true for the settings that are not single values; they have
	// commands of their own, because "set" is the wrong verb for them.
	List bool
}

// Settings returns every setting, in the order they appear in the file,
// with the values this configuration currently has.
func Settings(cfg Config) []Setting {
	var out []Setting
	walk(reflect.ValueOf(cfg), "", false, &out)
	return out
}

// Lookup returns the setting at a path, with its current value.
func Lookup(cfg Config, path string) (Setting, bool) {
	for _, s := range Settings(cfg) {
		if s.Path == path {
			return s, true
		}
	}
	return Setting{}, false
}

func walk(v reflect.Value, prefix string, hotParent bool, out *[]Setting) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := field.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		hot := hotParent || field.Tag.Get("hot") == "true"

		value := v.Field(i)
		switch value.Kind() {
		case reflect.Struct:
			if value.Type() == reflect.TypeOf(Duration(0)) {
				*out = append(*out, Setting{Path: path, Kind: "duration",
					Value: Duration(value.Int()).String(), Hot: hot})
				continue
			}
			walk(value, path, hot, out)
		case reflect.Slice:
			*out = append(*out, Setting{Path: path, Kind: "list",
				Value: listValue(value), Hot: hot, List: true})
		case reflect.Int64:
			if value.Type() == reflect.TypeOf(Duration(0)) {
				*out = append(*out, Setting{Path: path, Kind: "duration",
					Value: Duration(value.Int()).String(), Hot: hot})
				continue
			}
			*out = append(*out, Setting{Path: path, Kind: "number",
				Value: strconv.FormatInt(value.Int(), 10), Hot: hot})
		case reflect.Int:
			*out = append(*out, Setting{Path: path, Kind: "number",
				Value: strconv.FormatInt(value.Int(), 10), Hot: hot})
		case reflect.Bool:
			*out = append(*out, Setting{Path: path, Kind: "true/false",
				Value: strconv.FormatBool(value.Bool()), Hot: hot})
		case reflect.String:
			*out = append(*out, Setting{Path: path, Kind: "text",
				Value: value.String(), Hot: hot})
		}
	}
}

func listValue(v reflect.Value) string {
	if v.Len() == 0 {
		return "(empty)"
	}
	if v.Type().Elem().Kind() == reflect.String {
		parts := make([]string, v.Len())
		for i := 0; i < v.Len(); i++ {
			parts[i] = v.Index(i).String()
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprintf("%d entries", v.Len())
}

// CheckValue reports whether a value can be assigned to a setting, so a
// mistake is refused before the file is touched rather than after.
func CheckValue(s Setting, value string) error {
	switch s.Kind {
	case "number":
		if _, err := strconv.Atoi(value); err != nil {
			return fmt.Errorf("%s takes a number, not %q", s.Path, value)
		}
	case "true/false":
		if value != "true" && value != "false" {
			return fmt.Errorf("%s takes true or false, not %q", s.Path, value)
		}
	case "duration":
		var d Duration
		if err := d.parse(value); err != nil {
			return fmt.Errorf("%s: %w", s.Path, err)
		}
	case "list":
		return fmt.Errorf("%s is a list; there are commands for it, see --help", s.Path)
	}
	return nil
}

// parse accepts what the YAML unmarshaller accepts, so a value refused on
// the command line is exactly a value that would have been refused in the
// file.
func (d *Duration) parse(s string) error {
	var node struct {
		D Duration `yaml:"d"`
	}
	if err := unmarshalYAML("d: "+renderValue(s), &node); err != nil {
		return err
	}
	*d = node.D
	return nil
}
