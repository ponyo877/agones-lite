package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// parseWithConfig parses args, then fills in from a YAML file every flag
// the command line did not set. The file is named by the --config flag and
// uses the flag names as its keys:
//
//	data-dir: /var/lib/agones-lite
//	node-listen: ":9445"
//	tls-san: [cp.example.com, 192.0.2.1]
//
// A key without a value leaves its flag alone.
func parseWithConfig(fs *flag.FlagSet, args []string) error {
	file := fs.String("config", "", "YAML file whose keys are the names of these flags; flags given on the command line win")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return nil
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var settings map[string]any
	if err := yaml.Unmarshal(raw, &settings); err != nil {
		return fmt.Errorf("%s: %w", *file, err)
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	for name, value := range settings {
		if err := fill(fs, name, value, given[name]); err != nil {
			return fmt.Errorf("%s: %s: %w", *file, name, err)
		}
	}
	return nil
}

// fill sets one flag from the file, unless the command line set it already
// or the file has no value for it.
func fill(fs *flag.FlagSet, name string, value any, given bool) error {
	if fs.Lookup(name) == nil {
		return errors.New("there is no such flag")
	}
	if given || value == nil {
		return nil
	}
	text, err := typed(value)
	if err != nil {
		return err
	}
	return fs.Set(name, text)
}

// typed returns a value from the file the way it would be typed on the
// command line, which is what a flag reads.
func typed(value any) (string, error) {
	switch v := value.(type) {
	case []any:
		return joined(v)
	case map[string]any:
		return "", errors.New("a flag takes one value or a list of them, not a map")
	case float64:
		// YAML numbers arrive as float64, and Go would print a million of
		// them as 1e+06.
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	}
	return fmt.Sprint(value), nil
}

// joined is a list the way the flags take one: separated by commas.
func joined(list []any) (string, error) {
	items := make([]string, len(list))
	for i, item := range list {
		switch item.(type) {
		case nil, []any, map[string]any:
			return "", errors.New("every item of a list must be a single value")
		}
		items[i], _ = typed(item)
	}
	return strings.Join(items, ","), nil
}
