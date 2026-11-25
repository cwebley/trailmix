package navigation

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestTitleRegisteredEditorAndRecoveryPaths(t *testing.T) {
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		t.Skip("Neovim unavailable")
	}
	home, err := os.MkdirTemp("/tmp", "tm-title-[work]-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	note := Target{ID: "0123456789abcdef0123456789abcdef", Path: filepath.Join(home, "meeting.md")}
	body := "<!-- trailmix: meeting " + note.ID + " -->\n# Meeting\n"
	if err := os.WriteFile(note.Path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(home, "meeting.answers.md")
	if err := os.WriteFile(sidecar, []byte("# Saved answers\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// This endpoint is discoverable only through registration, not e-*.sock.
	socket := filepath.Join(home, "ordinary.sock")
	cmd := exec.Command(nvim, "--headless", "-u", "NONE", "-i", "NONE", "--cmd",
		fmt.Sprintf("lua vim.opt.directory = {%q}", home+"//"), "--listen", socket, "--", note.Path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	n := &Navigator{home: home, nvim: nvim}
	if err := n.waitEditor(socket); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(home, "recap-editors")
	if err := os.MkdirAll(registry, 0700); err != nil {
		t.Fatal(err)
	}
	register := func(pid int) string {
		t.Helper()
		data, _ := json.Marshal(recapEditorRegistration{PID: pid, Socket: socket})
		path := filepath.Join(registry, strconv.Itoa(pid)+".json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	registration := register(cmd.Process.Pid)
	if err := n.checkManagedTitleSockets(note, nil); err == nil || !strings.Contains(err.Error(), "hidden or clean") {
		t.Fatalf("registered main note allowed: %v", err)
	}
	recovery := filepath.Join(home, ".original-recovery.swp")
	expr := fmt.Sprintf(`luaeval('(function() vim.b.trailmix_recap_original_swap = _A; vim.bo.swapfile = true; vim.cmd("preserve"); return vim.fn.swapname(vim.api.nvim_get_current_buf()) end)()', '%s')`, recovery)
	activeSwap, err := run(nvim, "--server", socket, "--remote-expr", expr)
	if err != nil || activeSwap == "" {
		t.Fatalf("create fixture swap: %q %v", activeSwap, err)
	}
	state, err := RecapEditors(home, note)
	if err != nil || !hasSwapPath(state.SwapPaths, activeSwap) || !hasSwapPath(state.SwapPaths, recovery) {
		t.Fatalf("main/recovery swap paths missing: %+v %v", state, err)
	}
	// A sidecar without a meeting marker is still protected by its associated
	// filename, including when the main note is no longer loaded.
	expr = fmt.Sprintf(`luaeval('(function() local old = vim.api.nvim_get_current_buf(); vim.cmd("hide edit " .. vim.fn.fnameescape(_A)); vim.api.nvim_buf_delete(old, {force=true}); vim.bo.swapfile = true; vim.cmd("preserve"); return vim.fn.swapname(vim.api.nvim_get_current_buf()) end)()', '%s')`, sidecar)
	sidecarSwap, err := run(nvim, "--server", socket, "--remote-expr", expr)
	if err != nil || sidecarSwap == "" {
		t.Fatalf("create sidecar fixture swap: %q %v", sidecarSwap, err)
	}
	state, err = RecapEditors(home, note)
	if err != nil || state.Loaded != 0 || !hasSwapPath(state.SwapPaths, sidecarSwap) {
		t.Fatalf("sidecar swap not protected independently: %+v %v", state, err)
	}
	if err := n.checkManagedTitleSockets(note, nil); err == nil {
		t.Fatal("registered sidecar accepted for rename")
	}
	if _, err := run(nvim, "--server", socket, "--remote-expr", `execute('silent! bwipeout!')`); err != nil {
		t.Fatal(err)
	}
	if err := n.checkManagedTitleSockets(note, nil); err != nil {
		t.Fatalf("unrelated registered editor blocked: %v", err)
	}
	if err := os.Remove(registration); err != nil {
		t.Fatal(err)
	}
	register(os.Getpid()) // Valid live PID, but not the endpoint's process.
	if err := n.checkManagedTitleSockets(note, nil); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("changed registered editor identity accepted: %v", err)
	}
}

func hasSwapPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}
