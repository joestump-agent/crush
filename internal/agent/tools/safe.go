package tools

import (
	"runtime"
	"slices"
	"strings"
)

var safeCommands = []string{
	// Bash builtins and core utils
	"cal",
	"date",
	"df",
	"du",
	"echo",
	"env",
	"free",
	"groups",
	"hostname",
	"id",
	"kill",
	"killall",
	"ls",
	"nice",
	"nohup",
	"printenv",
	"ps",
	"pwd",
	"set",
	"time",
	"timeout",
	"top",
	"type",
	"uname",
	"unset",
	"uptime",
	"whatis",
	"whereis",
	"which",
	"whoami",

	// Git
	"git blame",
	"git branch",
	"git config --get",
	"git config --list",
	"git describe",
	"git diff",
	"git grep",
	"git log",
	"git ls-files",
	"git ls-remote",
	"git remote",
	"git rev-parse",
	"git shortlog",
	"git show",
	"git status",
	"git tag",
}

// readOnlyCommands are the commands the todo enforcement ladder treats
// as read-only (IsReadOnlyCommand): the same read-only utilities as
// safeCommands, minus the ones that can wrap another program (kill,
// killall, nice, nohup, time, timeout, env), change shell state (set,
// unset) or write (git branch, git tag, git remote). The bash tool's
// permission prompt keeps the wider safeCommands list.
var readOnlyCommands = []string{
	// Bash builtins and core utils
	"cal",
	"date",
	"df",
	"du",
	"echo",
	"free",
	"groups",
	"hostname",
	"id",
	"ls",
	"printenv",
	"ps",
	"pwd",
	"top",
	"type",
	"uname",
	"uptime",
	"whatis",
	"whereis",
	"which",
	"whoami",

	// Git
	"git blame",
	"git config --get",
	"git config --list",
	"git describe",
	"git diff",
	"git grep",
	"git log",
	"git ls-files",
	"git ls-remote",
	"git rev-parse",
	"git shortlog",
	"git show",
	"git status",
}

var chainingMetacharacters = []string{
	";",
	"|",
	"&&",
	"$(",
	"`",
}

// containsCommandChaining reports whether s contains shell metacharacters
// that enable command chaining or substitution.
func containsCommandChaining(s string) bool {
	return slices.ContainsFunc(chainingMetacharacters, func(c string) bool {
		return strings.Contains(s, c)
	})
}

// prefixMatchesReadOnlyCommand reports whether command is a single
// invocation of one of cmds: no chaining or substitution
// metacharacters, and the command starts with an entry followed by
// whitespace, a flag or end of string.
func prefixMatchesReadOnlyCommand(command string, cmds []string) bool {
	cmdLower := strings.ToLower(command)
	if containsCommandChaining(cmdLower) {
		return false
	}
	for _, safe := range cmds {
		if !strings.HasPrefix(cmdLower, safe) {
			continue
		}
		if len(cmdLower) == len(safe) || cmdLower[len(safe)] == ' ' || cmdLower[len(safe)] == '-' {
			return true
		}
	}
	return false
}

// IsReadOnlyCommand reports whether command parses as a read-only
// command, for the todo enforcement ladder's mutation check: a single
// invocation of a known read-only utility, with no chaining or
// substitution. It matches the narrower readOnlyCommands list, because
// the ladder must not count a command read-only when it can wrap another
// program, change shell state, kill a process or write; unparseable
// input fails closed. Redirection, backgrounding and grouping are
// mutations the prefix check cannot see, so their metacharacters fail
// closed too.
func IsReadOnlyCommand(command string) bool {
	if strings.ContainsAny(command, ">&<\n()") {
		return false
	}
	return prefixMatchesReadOnlyCommand(command, readOnlyCommands)
}

func init() {
	if runtime.GOOS == "windows" {
		safeCommands = append(
			safeCommands,
			// Windows-specific commands
			"ipconfig",
			"nslookup",
			"ping",
			"systeminfo",
			"tasklist",
			"where",
		)
		// All of them are read-only, so the ladder's list carries them
		// too.
		readOnlyCommands = append(
			readOnlyCommands,
			"ipconfig",
			"nslookup",
			"ping",
			"systeminfo",
			"tasklist",
			"where",
		)
	}
}
