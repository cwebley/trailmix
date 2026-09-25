package auth_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/cwebley/trailmix/internal/auth"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "auth-test-cli" {
		if err := auth.Run(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func command(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exec.Command(executable, append([]string{"auth-test-cli"}, args...)...)
}

// Run the real prompt through a PTY. Check both visible output and terminal
// restoration, rather than substituting a fake password reader.
func enterKey(t *testing.T, input string, interrupt os.Signal) (string, error) {
	t.Helper()
	rawMaster, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rawMaster.Close()
	defer terminal.Close()
	// os.NewFile recognizes the nonblocking duplicate and registers it with
	// Go's poller. Deadlines now interrupt reads, including a missing prompt.
	fd, err := unix.Dup(int(rawMaster.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "auth-test-pty")
	defer master.Close()
	rawMaster.Close()
	if err := master.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	original, err := term.GetState(int(terminal.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	cmd := command(t)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = terminal, terminal, terminal
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	defer func() { cmd.Process.Kill(); <-done }()
	var output bytes.Buffer
	buf := make([]byte, 4096)
	for !strings.Contains(output.String(), "OpenAI API key: ") {
		n, err := master.Read(buf)
		output.Write(buf[:n])
		if err != nil {
			t.Fatalf("auth prompt did not appear: %v %q", err, output.String())
		}
	}
	if _, err := io.WriteString(master, input); err != nil {
		t.Fatal(err)
	}
	if interrupt != nil {
		if err := cmd.Process.Signal(interrupt); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatal("auth command did not finish")
	}
	restored, stateErr := term.GetState(int(terminal.Fd()))
	if stateErr != nil || !reflect.DeepEqual(original, restored) {
		t.Fatalf("auth did not restore terminal state: %v", stateErr)
	}
	terminal.Close()
	if err := master.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	remaining, readErr := io.ReadAll(master)
	if os.IsTimeout(readErr) {
		t.Fatal("PTY did not close after auth exited")
	}
	output.Write(remaining)
	return output.String(), waitErr
}

func TestAuthSavesReplacesAndRemovesPrivateXDGCredentials(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("OPENAI_API_KEY", "unrelated-terminal-key")
	path := filepath.Join(root, "trailmix", "credentials.json")
	for _, key := range []string{"first-secret-key", "replacement-secret-key"} {
		output, err := enterKey(t, key+"\r", nil)
		if err != nil || strings.Contains(output, key) {
			t.Fatalf("prompt failed or echoed the key: %v %q", err, output)
		}
		got, err := auth.APIKey()
		if err != nil || got != key {
			t.Fatalf("saved credential is not available to recording/retry: %v", err)
		}
		for file, mode := range map[string]os.FileMode{filepath.Dir(path): 0700, path: 0600} {
			info, err := os.Stat(file)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("wrong private permissions for %s: %v", file, err)
			}
		}
		status, err := command(t, "status").CombinedOutput()
		if err != nil || !bytes.Contains(status, []byte(`"configured": true`)) || bytes.Contains(status, []byte(key)) {
			t.Fatalf("unsafe auth status: %v %s", err, status)
		}
		if os.Getenv("OPENAI_API_KEY") != "unrelated-terminal-key" {
			t.Fatal("auth changed the terminal environment")
		}
	}
	for i := 0; i < 2; i++ {
		if output, err := command(t, "logout").CombinedOutput(); err != nil {
			t.Fatalf("logout: %v %s", err, output)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("logout retained credential: %v", err)
	}
	if key, err := auth.APIKey(); err != nil || key != "" {
		t.Fatalf("logout fell back to terminal credentials: %v", err)
	}
	status, err := command(t, "status").CombinedOutput()
	if err != nil || !bytes.Contains(status, []byte(`"configured": false`)) {
		t.Fatalf("wrong logged-out status: %v %s", err, status)
	}
}

func TestAuthUsesHomeConfigForUnsetOrRelativeXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, xdg := range []string{"", "relative-config"} {
		t.Setenv("XDG_CONFIG_HOME", xdg)
		status, err := command(t, "status").CombinedOutput()
		path := filepath.Join(home, ".config", "trailmix", "credentials.json")
		if err != nil || !bytes.Contains(status, []byte(path)) || !bytes.Contains(status, []byte(`"configured": false`)) {
			t.Fatalf("wrong XDG fallback: %v %s", err, status)
		}
		if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Fatalf("status created credentials directory: %v", err)
		}
	}
	if output, err := enterKey(t, "home-secret\r", nil); err != nil || strings.Contains(output, "home-secret") {
		t.Fatalf("default-path auth: %v %q", err, output)
	}
	if key, err := auth.APIKey(); err != nil || key != "home-secret" {
		t.Fatalf("default-path credential unavailable: %v", err)
	}
}

func TestCanceledOrInvalidAuthPreservesExistingCredential(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if output, err := enterKey(t, "original-secret\r", nil); err != nil {
		t.Fatalf("initial auth: %v %q", err, output)
	}
	for _, tt := range []struct {
		name, input string
		interrupt   os.Signal
	}{
		{"ctrl-c", "unfinished-secret\x03", nil},
		{"empty", "\r", nil},
		{"invalid", "contains secret spaces\r", nil},
		{"interrupt", "unfinished-secret", os.Interrupt},
		{"terminate", "unfinished-secret", syscall.SIGTERM},
		{"hangup", "unfinished-secret", syscall.SIGHUP},
		{"quit", "unfinished-secret", syscall.SIGQUIT},
	} {
		t.Run(tt.name, func(t *testing.T) {
			output, err := enterKey(t, tt.input, tt.interrupt)
			if err == nil || strings.Contains(output, "unfinished-secret") || strings.Contains(output, "contains secret spaces") {
				t.Fatalf("cancellation accepted or exposed input: %v %q", err, output)
			}
			if key, err := auth.APIKey(); err != nil || key != "original-secret" {
				t.Fatalf("unsuccessful auth changed existing credential: %v", err)
			}
		})
	}
}

func TestMalformedCredentialsAreRedactedAndLogoutCanRemoveThem(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	directory := filepath.Join(root, "trailmix")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "credentials.json")
	for _, data := range []string{`{"openai_api_key":"secret-value`, `{"openai_api_key":""}`, strings.Repeat("secret-value", 2000)} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		output, err := command(t, "status").CombinedOutput()
		if err == nil || bytes.Contains(output, []byte("secret-value")) {
			t.Fatalf("invalid credential status exposed data: %v %q", err, output)
		}
		if key, err := auth.APIKey(); err == nil || key != "" {
			t.Fatal("invalid credentials were accepted")
		}
	}
	if output, err := command(t, "logout").CombinedOutput(); err != nil {
		t.Fatalf("cannot remove corrupt credentials: %v %s", err, output)
	}
}

func TestAuthRequiresInteractiveInputAndDoesNotPrintArguments(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	cmd := command(t)
	cmd.Stdin = strings.NewReader("secret-value\n")
	output, err := cmd.CombinedOutput()
	if err == nil || bytes.Contains(output, []byte("secret-value")) {
		t.Fatalf("noninteractive auth accepted or leaked input: %v %q", err, output)
	}
	output, err = command(t, "secret-value").CombinedOutput()
	if err == nil || bytes.Contains(output, []byte("secret-value")) {
		t.Fatalf("unexpected argument leaked: %v %q", err, output)
	}
	if _, err := os.Stat(filepath.Join(root, "trailmix")); !os.IsNotExist(err) {
		t.Fatalf("failed auth created credential storage: %v", err)
	}
}
