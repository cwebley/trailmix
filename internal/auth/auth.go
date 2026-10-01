// Package auth stores the user's hosted-provider credential in the XDG config
// directory. It does not use shell variables, checkout files, or an OS keychain.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/cwebley/trailmix/internal/cliout"
)

type credentials struct {
	OpenAIAPIKey string `json:"openai_api_key"`
}

func credentialsPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	// The XDG specification treats relative environment paths as invalid.
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(home) {
			return "", errors.New("cannot locate an absolute home directory for credentials")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "trailmix", "credentials.json"), nil
}

// APIKey returns an empty string when logged out. Both recording and retry use
// this lookup; logout cannot silently reveal a second key from .env or the shell.
func APIKey() (string, error) {
	path, err := credentialsPath()
	if err != nil {
		return "", err
	}
	return read(path)
}

func read(path string) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read credentials: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 16*1024+1))
	if err != nil {
		return "", errors.New("cannot read saved credentials; run trailmix auth to replace them")
	}
	var saved credentials
	// Never include JSON parser errors: they can contain credential text.
	if len(data) > 16*1024 || json.Unmarshal(data, &saved) != nil || !validKey(saved.OpenAIAPIKey) {
		return "", fmt.Errorf("invalid credentials in %s; run trailmix auth to replace them", path)
	}
	return saved.OpenAIAPIKey, nil
}

func validKey(key string) bool {
	if len(key) == 0 || len(key) > 4096 {
		return false
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func save(path, key string) error {
	directory := filepath.Dir(path)
	var created []string
	for parent := directory; ; parent = filepath.Dir(parent) {
		_, err := os.Stat(parent)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		created = append(created, parent)
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return err
	}
	// Persist newly created directory entries, including a new ~/.config.
	for i := len(created) - 1; i >= 0; i-- {
		if err := syncDirectory(filepath.Dir(created[i])); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp(directory, ".credentials-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(credentials{OpenAIAPIKey: key}); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Poll in the calling goroutine so cancellation never abandons a password
// reader on stdin. Nonblocking reads cover readiness changes after Poll returns.
type promptReader struct {
	ctx context.Context
	fd  int
}

func (r promptReader) Read(data []byte) (int, error) {
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		n, err = unix.Read(r.fd, data)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		return n, nil
	}
}

func prompt() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("run trailmix auth in an interactive terminal to enter the key without echo")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer cancel()
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	defer term.Restore(fd, state)
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return "", err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		return "", err
	}
	defer unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags)
	terminal := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{promptReader{ctx: ctx, fd: fd}, os.Stderr}, "")
	key, err := terminal.ReadPassword("OpenAI API key: ")
	if err != nil || ctx.Err() != nil {
		fmt.Fprint(os.Stderr, "\r\n")
		return "", errors.New("authentication canceled; saved credentials were not changed")
	}
	key = strings.TrimSpace(key)
	if !validKey(key) {
		return "", errors.New("enter a nonempty, single-line API key; saved credentials were not changed")
	}
	return key, nil
}

const usage = `Usage:
  trailmix auth          Enter or replace the saved OpenAI API key
  trailmix auth status   Show whether a key is saved, without displaying it
  trailmix auth logout   Remove the saved key

Credentials: $XDG_CONFIG_HOME/trailmix/credentials.json
Default:     ~/.config/trailmix/credentials.json on macOS and Linux
`

// Run implements the terminal auth workflow. Saving a key does not verify
// provider access; the first transcription request exercises that separately.
func Run(args []string) error {
	if len(args) > 1 {
		return errors.New(usage)
	}
	if len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		fmt.Print(usage)
		return nil
	}
	if len(args) == 1 && args[0] != "status" && args[0] != "logout" {
		return errors.New(usage)
	}
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		key, err := prompt()
		if err != nil {
			return err
		}
		if err := save(path, key); err != nil {
			return fmt.Errorf("save credentials: %w", err)
		}
		fmt.Printf("OpenAI API key saved to %s\n", path)
		return nil
	}
	if args[0] == "logout" {
		err := os.Remove(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove credentials: %w", err)
		}
		if err == nil {
			if err := syncDirectory(filepath.Dir(path)); err != nil {
				return fmt.Errorf("persist credential removal: %w", err)
			}
		}
		fmt.Println("Saved OpenAI API key removed.")
		return nil
	}
	key, err := read(path)
	if err != nil {
		return err
	}
	return cliout.JSON(struct {
		Configured bool   `json:"configured"`
		Path       string `json:"path"`
	}{Configured: key != "", Path: path}, true)
}
