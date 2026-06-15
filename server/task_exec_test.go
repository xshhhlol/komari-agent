package server

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestRunTaskCommandMultilineUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell script execution test")
	}

	result, exitCode := runTaskCommand("printf '%s\\n' first\nprintf '%s\\n' second\n")

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d with result %q", exitCode, result)
	}
	if result != "first\nsecond\n" {
		t.Fatalf("unexpected result %q", result)
	}
}

func TestRunTaskCommandEscapingAndWildcardsUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell script execution test")
	}

	tempDir := t.TempDir()
	command := strings.Join([]string{
		"cd " + shellSingleQuote(tempDir),
		"touch alpha.txt beta.log gamma.txt",
		"printf '%s\\n' \"quoted value with spaces\"",
		"printf '%s\\n' '*.txt'",
		"printf '%s ' *.txt",
		"printf '\\n'",
	}, "\n")

	result, exitCode := runTaskCommand(command)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d with result %q", exitCode, result)
	}
	lines := strings.Split(strings.TrimSuffix(result, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 output lines, got %d in %q", len(lines), result)
	}
	if lines[0] != "quoted value with spaces" {
		t.Fatalf("quoted argument was not preserved: %q", lines[0])
	}
	if lines[1] != "*.txt" {
		t.Fatalf("quoted wildcard should remain literal, got %q", lines[1])
	}
	expanded := strings.Fields(lines[2])
	sort.Strings(expanded)
	if len(expanded) != 2 || expanded[0] != "alpha.txt" || expanded[1] != "gamma.txt" {
		t.Fatalf("wildcard expansion mismatch: %q", lines[2])
	}
}

// 永不退出的命令（如 sleep/ping）应在 TaskExecTimeout 到时被终止，
// 而不是把任务永久挂死；已产生的输出需保留，退出码为约定的 124。
func TestRunTaskCommandTimeoutUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix timeout test")
	}
	old := flags.TaskExecTimeout
	flags.TaskExecTimeout = 1
	defer func() { flags.TaskExecTimeout = old }()

	start := time.Now()
	result, exitCode := runTaskCommand("echo KOMARI_START; sleep 30; echo KOMARI_DONE")
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Fatalf("command was not killed promptly, took %s", elapsed)
	}
	if exitCode != 124 {
		t.Fatalf("expected timeout exit code 124, got %d (result %q)", exitCode, result)
	}
	if !strings.Contains(result, "KOMARI_START") {
		t.Fatalf("expected partial output to contain KOMARI_START, got %q", result)
	}
	if strings.Contains(result, "KOMARI_DONE") {
		t.Fatalf("command should have been killed before printing KOMARI_DONE, got %q", result)
	}
}

func TestBuildTaskCommandUsesShellStdinUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell script execution test")
	}

	cmd, cleanup, err := buildTaskCommand(context.Background(), "printf done")
	if err != nil {
		t.Fatalf("buildTaskCommand returned error: %v", err)
	}
	defer cleanup()

	if filepath.Base(cmd.Path) != "sh" {
		t.Fatalf("expected sh command, got %q", cmd.Path)
	}
	if strings.Join(cmd.Args, " ") != "sh -s" {
		t.Fatalf("expected sh -s args, got %#v", cmd.Args)
	}
	if cmd.Stdin == nil {
		t.Fatal("expected command script to be provided on stdin")
	}
}

func TestBuildTaskCommandWritesUtf8BomWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell script execution test")
	}

	cmd, cleanup, err := buildTaskCommand(context.Background(), "Write-Output '你好'")
	if err != nil {
		t.Fatalf("buildTaskCommand returned error: %v", err)
	}
	defer cleanup()

	if filepath.Base(cmd.Path) != "powershell.exe" && filepath.Base(cmd.Path) != "powershell" {
		t.Fatalf("expected powershell command, got %q", cmd.Path)
	}
	if len(cmd.Args) == 0 {
		t.Fatal("expected PowerShell script path in command args")
	}

	scriptFile := cmd.Args[len(cmd.Args)-1]
	file, err := filepath.Abs(scriptFile)
	if err != nil {
		t.Fatalf("failed to resolve script path: %v", err)
	}

	f, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatalf("failed to evaluate script path: %v", err)
	}
	contents, err := os.ReadFile(f)
	if err != nil {
		t.Fatalf("failed to read script file: %v", err)
	}
	if len(contents) < 3 || contents[0] != 0xEF || contents[1] != 0xBB || contents[2] != 0xBF {
		t.Fatalf("expected UTF-8 BOM at start of script, got %#v", contents[:min(len(contents), 3)])
	}
}

func TestAppendErrorResultAvoidsLeadingNewline(t *testing.T) {
	if got := appendErrorResult("", "stderr"); got != "stderr" {
		t.Fatalf("expected stderr without leading newline, got %q", got)
	}
	if got := appendErrorResult("stdout", "stderr"); got != "stdout\nstderr" {
		t.Fatalf("expected stdout and stderr separated by newline, got %q", got)
	}
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
