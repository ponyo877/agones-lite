package cli

import (
	"flag"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// parseWithConfig parses args, then fills in from a YAML file every flag
// the command line did not set. The file is named by the --config flag and
// uses the flag names as its keys:
//
//	data-dir: /var/lib/agones-lite
//	node-listen: ":9445"
//	tls-san: cp.example.com
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
		if given[name] {
			continue
		}
		if err := fs.Set(name, fmt.Sprint(value)); err != nil {
			return fmt.Errorf("%s: %s: %w", *file, name, err)
		}
	}
	return nil
}
