-- Real window lifecycle checks for stacked views, reopening, and Back.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
local directory = vim.fn.tempname()
vim.fn.mkdir(directory, "p")
local id = "0123456789abcdef0123456789abcdef"
local marker = "<!-- trailmix: meeting " .. id .. " -->"
local cli = directory .. "/fake cli"
vim.fn.writefile({ "#!/bin/sh", 'cat "' .. directory .. '/$1.json"' }, cli)
vim.fn.setfperm(cli, "rwx------")
local speech, answer_lines = {}, { marker, "# AI answers" }
for i = 1, 80 do speech[i], answer_lines[#answer_lines + 1] = "Statement " .. i, "Answer line " .. i end
local snapshot = { schema_version = 1, meeting_id = id, passages = {
  { id = "system-1", meeting_id = id, track = "system", start_seconds = 1, end_seconds = 80, text = table.concat(speech, "\n") },
}, transcription = { meeting_id = id, state = "complete", tracks = { microphone = { state = "complete" }, system = { state = "complete" } } } }
vim.fn.writefile({ vim.json.encode(snapshot) }, directory .. "/transcript.json")
vim.fn.writefile({ vim.json.encode({ schema_version = 1, meeting_id = id, path = directory .. "/meeting.answers.md", markdown = table.concat(answer_lines, "\n") }) }, directory .. "/answers.json")
require("trailmix").setup({ cli = cli, mapping = false, transcript_refresh_ms = 100 })
local function window_for(kind)
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do if vim.b[vim.api.nvim_win_get_buf(window)].trailmix_kind == kind then return window end end
end
local function wait_for(predicate) assert(vim.wait(3000, predicate, 10), "panel content timed out") end
local function view(window) return vim.api.nvim_win_call(window, vim.fn.winsaveview) end
local function place(window, row, top)
  vim.api.nvim_win_call(window, function() vim.fn.winrestview({ lnum = row, col = 0, topline = top }) end)
  return view(window)
end
local function same_position(window, saved)
  local current = view(window)
  assert(current.lnum == saved.lnum and current.topline == saved.topline,
    "reading position changed: " .. vim.inspect({ current = current, saved = saved }))
end
local ok, err = xpcall(function()
  vim.api.nvim_buf_set_lines(0, 0, -1, false, { marker, "My note" })
  local note_window = vim.api.nvim_get_current_win()
  vim.cmd("TrailMixTranscript")
  local upper = window_for("transcript")
  wait_for(function() return vim.api.nvim_buf_line_count(vim.api.nvim_win_get_buf(upper)) > 80 end)
  local saved = place(upper, 40, 30)
  vim.api.nvim_win_close(upper, false)
  vim.cmd("TrailMixTranscript")
  upper = window_for("transcript")
  wait_for(function() return vim.api.nvim_buf_line_count(vim.api.nvim_win_get_buf(upper)) > 80 end)
  same_position(upper, saved)
  -- Earlier finalized speech arriving while closed must not change which
  -- passage/text row the saved cursor and viewport refer to.
  local buffer = vim.api.nvim_win_get_buf(upper)
  local cursor_text = vim.api.nvim_buf_get_lines(buffer, saved.lnum - 1, saved.lnum, false)[1]
  local top_text = vim.api.nvim_buf_get_lines(buffer, saved.topline - 1, saved.topline, false)[1]
  vim.api.nvim_win_close(upper, false)
  table.insert(snapshot.passages, 1, { id = "late-final", meeting_id = id, track = "microphone", start_seconds = 0, end_seconds = 0.5, text = "An earlier final" })
  vim.fn.writefile({ vim.json.encode(snapshot) }, directory .. "/transcript.json")
  vim.cmd("TrailMixTranscript")
  upper = window_for("transcript")
  wait_for(function() return vim.api.nvim_buf_line_count(vim.api.nvim_win_get_buf(upper)) > 80 end)
  buffer = vim.api.nvim_win_get_buf(upper)
  local resumed = view(upper)
  assert(vim.api.nvim_buf_get_lines(buffer, resumed.lnum - 1, resumed.lnum, false)[1] == cursor_text, "late final changed the reopened cursor's passage")
  assert(vim.api.nvim_buf_get_lines(buffer, resumed.topline - 1, resumed.topline, false)[1] == top_text, "late final changed the reopened viewport's passage")
  vim.cmd("TrailMixAnswers")
  local lower = window_for("answers")
  wait_for(function() return vim.api.nvim_buf_line_count(vim.api.nvim_win_get_buf(lower)) > 80 end)
  saved = place(lower, 50, 47)
  vim.api.nvim_win_close(lower, false)
  vim.cmd("TrailMixAnswers")
  lower = window_for("answers")
  wait_for(function() return view(lower).lnum == saved.lnum and view(lower).topline == saved.topline end)
  same_position(lower, saved)
  assert(vim.api.nvim_get_current_win() == note_window, "reopening took note focus")
  -- Back from a note citation whose original window closed must not replace
  -- any other ordinary note the user now has open.
  local origin = vim.api.nvim_create_buf(true, false)
  vim.api.nvim_buf_set_lines(origin, 0, -1, false, { "Original note" })
  local origin_window = vim.api.nvim_open_win(origin, true, { split = "left", win = note_window })
  local panel = require("trailmix.panel")
  panel.remember(upper, { buffer = origin, window = origin_window, position = view(origin_window) })
  vim.api.nvim_set_current_win(upper)
  vim.api.nvim_win_close(origin_window, false)
  local other_note = vim.api.nvim_win_get_buf(note_window)
  panel.back()
  assert(vim.api.nvim_get_current_buf() == origin, "Back did not restore the source note")
  assert(vim.api.nvim_win_get_buf(note_window) == other_note, "Back replaced another note window")
  assert(vim.b[vim.api.nvim_win_get_buf(upper)].trailmix_kind == "transcript", "Back replaced the evidence")
end, debug.traceback)
vim.fn.delete(directory, "rf")
if not ok then error(err) end
print("TrailMix panel position checks passed")
vim.cmd("qa!")
