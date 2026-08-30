package cli

import (
	"errors"
	"fmt"
	"strings"
)

func (r *Root) version(args []string) error {
	if len(args) > 0 {
		fs := newFlags("version")
		if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
			return errors.New("version does not accept positional arguments")
		}
	}
	version := r.services.Version
	if version == "" {
		version = "dev"
	}
	fmt.Fprintln(r.services.Stdout, version)
	return nil
}

func (r *Root) completion(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ghaas completion {bash|zsh|fish}")
	}
	shell := strings.ToLower(args[0])
	names := completionNames()
	flags := completionFlags()
	switch shell {
	case "bash":
		fmt.Fprint(r.services.Stdout, bashCompletion(names, flags))
	case "zsh":
		fmt.Fprint(r.services.Stdout, zshCompletion(names, flags))
	case "fish":
		fmt.Fprint(r.services.Stdout, fishCompletion(names, flags))
	default:
		return fmt.Errorf("unsupported shell %q (choose bash, zsh, or fish)", args[0])
	}
	return nil
}

func completionNames() []string {
	names := make([]string, 0, len(commandTable))
	for _, spec := range commandTable {
		if !spec.Hidden && !strings.Contains(spec.Name, " ") {
			names = append(names, spec.Name)
		}
	}
	return names
}

func completionFlags() map[string][]string {
	flags := make(map[string][]string)
	for _, spec := range commandTable {
		if len(spec.Flags) > 0 {
			flags[spec.Name] = append([]string(nil), spec.Flags...)
		}
	}
	return flags
}

func bashCompletion(names []string, flags map[string][]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# ghaas completion\n_ghaas_complete() {\n  local cur=\"${COMP_WORDS[COMP_CWORD]}\"\n  local words=\"%s\"\n  if (( COMP_CWORD > 1 )); then\n    case \"${COMP_WORDS[1]}\" in\n", strings.Join(names, " "))
	for _, spec := range commandTable {
		if options := flags[spec.Name]; len(options) > 0 {
			fmt.Fprintf(&b, "      %s) words=\"%s\" ;;\n", spec.Name, strings.Join(options, " "))
		}
	}
	b.WriteString("    esac\n  fi\n  COMPREPLY=( $(compgen -W \"$words\" -- \"$cur\") )\n}\ncomplete -F _ghaas_complete ghaas\n")
	return b.String()
}

func zshCompletion(names []string, flags map[string][]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#compdef ghaas\n_ghaas() {\n  if (( CURRENT == 2 )); then\n    _describe command '(%s)'\n    return\n  fi\n  case $words[2] in\n", strings.Join(names, " "))
	for _, spec := range commandTable {
		if options := flags[spec.Name]; len(options) > 0 {
			fmt.Fprintf(&b, "    %s) _describe option '(%s)' ;;\n", spec.Name, strings.Join(options, " "))
		}
	}
	b.WriteString("  esac\n}\ncompdef _ghaas ghaas\n")
	return b.String()
}

func fishCompletion(names []string, flags map[string][]string) string {
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "complete -c ghaas -f -n '__fish_use_subcommand' -a %s\n", name)
	}
	for _, spec := range commandTable {
		for _, option := range spec.Flags {
			long := strings.TrimPrefix(option, "--")
			requiresValue := long == "function" || long == "ref" || long == "invocation"
			if requiresValue {
				fmt.Fprintf(&b, "complete -c ghaas -f -n '__fish_seen_subcommand_from %s' -l %s -r\n", spec.Name, long)
			} else {
				fmt.Fprintf(&b, "complete -c ghaas -f -n '__fish_seen_subcommand_from %s' -l %s\n", spec.Name, long)
			}
		}
	}
	return b.String()
}
