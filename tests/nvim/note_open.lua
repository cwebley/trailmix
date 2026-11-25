-- Saved exports must work without a live transcript view or recording.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
vim.g.mapleader = " "
require("trailmix").setup()
local root = vim.fn.resolve(vim.fn.tempname()) .. " notes with spaces"
vim.fn.mkdir(root .. "/meetings", "p")
local previous_home = vim.env.TRAILMIX_HOME
vim.env.TRAILMIX_HOME = root
local id = "0123456789abcdef0123456789abcdef"
local other = "abcdef0123456789abcdef0123456789"
local marker = "<!-- trailmix: meeting " .. id .. " -->"
local notices = {}
vim.notify = function(message) notices[#notices + 1] = message end
local note = root .. "/call.md"
local export = root .. "/call.transcript.md"
vim.fn.writefile({ marker, "@00:20", "notes", "@01:00", "later" }, note)
vim.fn.writefile({ "<!-- trailmix: transcript " .. id .. " -->", marker, "# Transcript", "",
  "## 00:58–01:03 · microphone", "", "Passage: `microphone-first`", "", "> hello" }, export)
local function jump()
  vim.api.nvim_win_set_cursor(0, { 9, 0 })
  vim.api.nvim_feedkeys(" mh", "xt", false)
end
local ok, err = xpcall(function()
  vim.cmd("edit " .. vim.fn.fnameescape(export))
  local transcript_window, transcript = vim.api.nvim_get_current_win(), vim.api.nvim_get_current_buf()
  jump()
  local note_window, buffer = vim.api.nvim_get_current_win(), vim.api.nvim_get_current_buf()
  assert(note_window ~= transcript_window and vim.api.nvim_buf_get_name(buffer) == note, "did not open adjacent note: " .. vim.api.nvim_buf_get_name(buffer) .. " " .. vim.inspect(notices))
  assert(vim.api.nvim_win_get_cursor(0)[1] == 4, "did not jump to closest anchor")
  assert(vim.api.nvim_win_get_position(note_window)[2] < vim.api.nvim_win_get_position(transcript_window)[2], "note not left of transcript")
  vim.api.nvim_buf_set_lines(buffer, -1, -1, false, { "unsaved" })
  vim.api.nvim_set_current_win(transcript_window)
  jump()
  assert(vim.api.nvim_get_current_win() == note_window and #vim.api.nvim_tabpage_list_wins(0) == 2, "duplicated visible note")
  -- Another tab's note buffer is reused, including its unsaved text.
  vim.cmd("tabnew")
  vim.api.nvim_win_set_buf(0, transcript)
  jump()
  assert(vim.api.nvim_get_current_buf() == buffer and vim.api.nvim_buf_get_lines(buffer, -2, -1, false)[1] == "unsaved", "lost other-tab edits")
  vim.cmd("tabclose!")
  vim.api.nvim_set_current_win(transcript_window)
  vim.api.nvim_win_close(note_window, true)
  vim.api.nvim_buf_delete(buffer, { force = true })
  -- Association lookup supports renamed notes, rather than guessing latest meeting.
  local renamed = root .. "/renamed.md"
  vim.fn.rename(note, renamed)
  vim.fn.writefile({ vim.json.encode({ id = id, path = renamed }) }, root .. "/meetings/" .. id .. ".json")
  jump()
  assert(vim.api.nvim_buf_get_name(0) == renamed, "association lookup failed")
  buffer = vim.api.nvim_get_current_buf()
  vim.api.nvim_buf_set_lines(buffer, 0, -1, false, { marker, "no timestamps" })
  vim.api.nvim_set_current_win(transcript_window)
  jump()
  assert(vim.api.nvim_get_current_buf() == buffer and notices[#notices]:find("no timestamps", 1, true), "anchorless note not focused")
  vim.api.nvim_win_close(vim.api.nvim_get_current_win(), true)
  vim.api.nvim_buf_delete(buffer, { force = true })
  vim.fn.writefile({ "<!-- trailmix: meeting " .. other .. " -->" }, renamed)
  local count = #vim.api.nvim_tabpage_list_wins(0)
  jump()
  assert(vim.api.nvim_get_current_win() == transcript_window and #vim.api.nvim_tabpage_list_wins(0) == count, "opened mismatched note")
  assert(notices[#notices]:find("cannot find a matching", 1, true), "missing mismatch warning")
  vim.fn.delete(renamed)
  jump()
  assert(#vim.api.nvim_tabpage_list_wins(0) == count, "created missing note")
end, debug.traceback)
vim.env.TRAILMIX_HOME = previous_home
vim.fn.delete(root, "rf")
if not ok then error(err) end
print("TrailMix note opening checks passed")
vim.cmd("qa!")
