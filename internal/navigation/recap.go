package navigation

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RecapEditorState describes known editor copies without changing any buffer.
// Notes is present only for one unambiguous, associated main-note buffer.
// Arbitrary external editors cannot be discovered reliably. Recap generation
// never writes the main note. Automatic naming also uses CheckTitleSafety.
type RecapEditorState struct {
	Notes    *string
	Loaded   int
	Conflict bool
	Reason   string
	// SwapPaths includes current and known recovery files for the main note,
	// its sidecars, and copies with the same meeting identity.
	SwapPaths []string
}

type recapEditorRegistration struct {
	PID    int    `json:"pid"`
	Socket string `json:"socket"`
}

// recapEditorSockets returns managed and registered endpoints. A nonzero PID
// makes the endpoint required and lets callers verify its process identity.
func recapEditorSockets(home string) (map[string]int, error) {
	sockets := map[string]int{}
	// Enumerate literal directories: TRAILMIX_HOME may itself contain glob
	// characters, which must not broaden or hide editor discovery.
	entries, err := os.ReadDir(home)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "e-") && strings.HasSuffix(entry.Name(), ".sock") {
			sockets[filepath.Join(home, entry.Name())] = 0
		}
	}
	root := filepath.Join(home, "recap-editors")
	registrations, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range registrations {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read editor registration: %w", err)
		}
		var editor recapEditorRegistration
		if json.Unmarshal(data, &editor) != nil || editor.PID < 2 || !filepath.IsAbs(editor.Socket) || filepath.Base(path) != strconv.Itoa(editor.PID)+".json" {
			return nil, fmt.Errorf("invalid recap editor registration: %s", path)
		}
		if err := syscall.Kill(editor.PID, 0); errors.Is(err, syscall.ESRCH) {
			continue // A crashed editor can leave a registration behind.
		} else if err != nil {
			return nil, fmt.Errorf("inspect registered editor: %w", err)
		}
		sockets[editor.Socket] = editor.PID
	}
	return sockets, nil
}

func RecapEditors(home string, note Target) (RecapEditorState, error) {
	var state RecapEditorState
	if !filepath.IsAbs(note.Path) || note.ID == "" {
		return state, errors.New("invalid meeting editor association")
	}
	sockets, err := recapEditorSockets(home)
	if err != nil {
		return state, err
	}
	if len(sockets) == 0 {
		return state, nil
	}
	nvim, err := executable("nvim")
	if err != nil {
		return state, err
	}
	seen := map[int]bool{}
	for socket, expectedPID := range sockets {
		connection, err := net.DialTimeout("unix", socket, 150*time.Millisecond)
		if err != nil {
			if expectedPID == 0 && (errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist)) {
				continue
			}
			return state, fmt.Errorf("cannot inspect editor %s: %w", socket, err)
		}
		connection.Close()
		quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
		code := `(function()
  local matches, swaps = {}, {}
  local marker = "<!-- trailmix: meeting " .. _A[2] .. " -->"
  local canonical = function(path) return vim.uv.fs_realpath(path) or path end
  local want = canonical(_A[1])
  local base = _A[1]:gsub("%.[^./]+$", "")
  local paths = { [want] = true, [canonical(base .. ".answers.md")] = true, [canonical(base .. ".transcript.md")] = true }
  local add_swap = function(path) if type(path) == "string" and path ~= "" then swaps[#swaps + 1] = vim.fn.fnamemodify(path, ":p") end end
  for _, buf in ipairs(vim.api.nvim_list_bufs()) do
    local path = vim.api.nvim_buf_get_name(buf)
    if vim.api.nvim_buf_is_loaded(buf) then
      local related = paths[canonical(path)]
      if not related then
        for _, line in ipairs(vim.api.nvim_buf_get_lines(buf, 0, -1, false)) do
          if line:find(marker, 1, true) then related = true; break end
        end
      end
      if related then
        local ok, swap = pcall(vim.fn.swapname, buf)
        if ok then add_swap(swap) end
        add_swap(vim.b[buf].trailmix_recap_original_swap)
      end
    end
    if vim.api.nvim_buf_is_loaded(buf) and vim.bo[buf].buftype == "" and not vim.b[buf].trailmix_kind
      and not path:match("%.answers%.md$") and not path:match("%.transcript%.md$") then
      local lines = vim.api.nvim_buf_get_lines(buf, 0, -1, false)
      local found, identities = 0, 0
      for _, line in ipairs(lines) do
        for _ in line:gmatch("<!%-%- trailmix: meeting ") do identities = identities + 1 end
        if line:find(marker, 1, true) then found = found + 1 end
      end
      if canonical(path) == want or found > 0 then
        matches[#matches + 1] = {
          associated = canonical(path) == want and found == 1 and identities == 1 and not vim.b[buf].trailmix_recap_swap_pending,
          reason = vim.b[buf].trailmix_recap_swap_pending and "meeting note has unresolved swap recovery" or nil,
          notes = table.concat(lines, "\n") .. (vim.bo[buf].endofline and "\n" or ""),
        }
      end
    end
  end
  return vim.json.encode({pid = vim.fn.getpid(), matches = matches, swaps = swaps})
end)()`
		expression := "luaeval(" + quote(code) + ", [" + quote(note.Path) + ", " + quote(note.ID) + "])"
		out, err := run(nvim, "--server", socket, "--remote-expr", expression)
		if err != nil {
			return state, fmt.Errorf("inspect recap editor: %w", err)
		}
		var response struct {
			PID     int      `json:"pid"`
			Swaps   []string `json:"swaps"`
			Matches []struct {
				Associated bool   `json:"associated"`
				Notes      string `json:"notes"`
				Reason     string `json:"reason"`
			} `json:"matches"`
		}
		if json.Unmarshal([]byte(out), &response) != nil || response.PID < 2 || (expectedPID != 0 && response.PID != expectedPID) {
			return state, errors.New("recap editor identity changed or returned invalid state")
		}
		if seen[response.PID] {
			continue
		}
		seen[response.PID] = true
		for _, path := range response.Swaps {
			if !filepath.IsAbs(path) {
				return state, errors.New("editor returned a non-absolute swap path")
			}
			state.SwapPaths = append(state.SwapPaths, path)
		}
		for _, match := range response.Matches {
			state.Loaded++
			if !match.Associated {
				state.Conflict, state.Reason = true, "a loaded meeting copy does not match the associated main note"
				if match.Reason != "" {
					state.Reason = match.Reason
				}
			} else {
				text := match.Notes
				state.Notes = &text
			}
		}
	}
	if state.Loaded > 1 {
		state.Conflict, state.Reason = true, "more than one editor buffer has this meeting identity"
	}
	if state.Conflict {
		state.Notes = nil
	}
	return state, nil
}
