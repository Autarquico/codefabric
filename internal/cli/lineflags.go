package cli

import (
	"fmt"
	"strings"
)

// ParseCommandLine reads args with the flag set of the command at path, in a
// fresh command tree, without running it: the positional values, and the value
// of each of the flags in names that was given. It is how a caller that cannot
// run a command (an attach, which the TUI runs itself) reads a line the way the
// CLI would. A flag of the command that is not in names is an error.
func ParseCommandLine(opts Options, path, args, names []string) (positional []string, flags map[string]string, err error) {
	cmd, _, err := CommandTree(opts).Find(path)
	if err != nil || cmd == nil || cmd.Parent() == nil {
		return nil, nil, fmt.Errorf("there is no command %s", strings.Join(path, " "))
	}
	if err := cmd.ParseFlags(args); err != nil {
		return nil, nil, err
	}
	flags = map[string]string{}
	for _, name := range names {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			flags[name] = f.Value.String()
		}
	}
	if cmd.Flags().NFlag() != len(flags) {
		return nil, nil, fmt.Errorf("%s takes only the flags %s", strings.Join(path, " "), strings.Join(names, ", "))
	}
	return cmd.Flags().Args(), flags, nil
}

// FlagGiven reports whether args, read with the flag set of the command at
// path, set the flag called name (a persistent flag of the root included). It
// reads them the way the CLI does, so a flag hidden as the value of another
// (`--name -- --controller x`) is seen, and a `--` that ends the flags is too.
func FlagGiven(opts Options, path, args []string, name string) (bool, error) {
	cmd, _, err := CommandTree(opts).Find(path)
	if err != nil || cmd == nil || cmd.Parent() == nil {
		return false, fmt.Errorf("there is no command %s", strings.Join(path, " "))
	}
	if err := cmd.ParseFlags(args); err != nil {
		return false, err
	}
	f := cmd.Flags().Lookup(name)
	return f != nil && f.Changed, nil
}
