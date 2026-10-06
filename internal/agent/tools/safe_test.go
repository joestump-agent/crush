package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContainsCommandChaining(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"plain ls", "ls -la", false},
		{"plain echo", "echo hello world", false},
		{"plain pwd", "pwd", false},
		{"plain git status", "git status", false},
		{"ls with redirect", "ls > /tmp/out", false},
		{"ls with pipe", "ls | grep foo", true},
		{"ls with double ampersand", "ls && echo done", true},
		{"ls with semicolon", "ls; echo done", true},
		{"ls with pipe pipe", "ls || echo fail", true},
		{"ls with backticks", "ls `echo foo`", true},
		{"ls with subshell", "ls $(echo foo)", true},
		{"ls with background ampersand", "ls & echo done", false},
		{"rm -rf with && ls (rm first)", "rm -rf / && ls", true},
		{"redirect with ampersand gt", "ls &> /dev/null", false},
		{"redirect with gt ampersand", "ls >& /dev/null", false},
		{"simple kill", "kill 1234", false},
		{"kill with pipe", "kill 1234 | echo foo", true},
		{"git log", "git log --oneline", false},
		{"git log with pipe", "git log | head", true},
		{"empty string", "", false},
		{"dollar sign in argument", "echo $HOME", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := containsCommandChaining(tt.input)
			assert.Equal(t, tt.expected, got, "containsCommandChaining(%q)", tt.input)
		})
	}
}

func TestIsReadOnlyCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"plain ls", "ls -la", true},
		{"bare ls", "ls", true},
		{"git status", "git status", true},
		{"git log", "git log --oneline", true},
		{"git blame", "git blame main", true},
		{"git config get", "git config --get user.name", true},
		{"git config list", "git config --list", true},
		{"uppercase", "GIT STATUS", true},
		{"echo", "echo hello", true},
		{"id with flag", "id -u", true},
		{"rm", "rm x", false},
		{"chained mutating", "git status && rm x", false},
		{"chained with semicolon", "ls; rm x", false},
		{"substitution", "echo $(rm x)", false},
		{"backtick substitution", "ls `rm x`", false},
		{"echo redirection", "echo x > f", false},
		{"echo append redirection", "echo x >> f", false},
		{"stderr redirection", "ls 2> err", false},
		{"backgrounds a command", "echo x & rm y", false},
		{"process substitution", "echo <(rm x)", false},
		{"subshell", "(rm x)", false},
		{"newline chains commands", "echo hi\nrm x", false},
		{"timeout wraps a mutating command", "timeout 5 rm x", false},
		{"timeout wraps a read-only command", "timeout 5 ls", false},
		{"nice wraps a command", "nice ls", false},
		{"nohup wraps a command", "nohup ls", false},
		{"time wraps a command", "time ls", false},
		{"env runs another command", "env FOO=bar ls", false},
		{"set changes shell state", "set -x", false},
		{"unset changes shell state", "unset FOO", false},
		{"kill terminates a process", "kill 1234", false},
		{"killall terminates processes", "killall frob", false},
		{"git branch", "git branch", false},
		{"git branch create", "git branch feature", false},
		{"git tag", "git tag v1.0", false},
		{"git remote", "git remote add origin url", false},
		{"unknown command", "frobnicate", false},
		{"empty string", "", false},
		{"prefix with attached text", "git statusx", false},
		{"id with attached text", "idontexist", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := IsReadOnlyCommand(tt.input)
			assert.Equal(t, tt.expected, got, "IsReadOnlyCommand(%q)", tt.input)
		})
	}
}
