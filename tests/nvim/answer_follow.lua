-- Answers follow new content in the background, until the reader navigates.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
local directory = vim.fn.tempname()
vim.fn.mkdir(directory, "p")
local id = "0123456789abcdef0123456789abcdef"
local marker = "<!-- trailmix: meeting " .. id .. " -->"
local cli = directory .. "/fake cli"
vim.fn.writefile({
  "#!/bin/sh",
  'case "$1" in',
  'ask) cat > "' .. directory .. '/request.json"; sleep 0.2; cat "' .. directory .. '/response.json" ;;',
  'answers) cat "' .. directory .. '/response.json" ;;',
  'transcript) cat "' .. directory .. '/transcript.json" ;;',
  '*) exit 2 ;;',
  'esac',
}, cli)
vim.fn.setfperm(cli, "rwx------")
local history = { marker, "# AI answers" }
for i = 1, 90 do history[#history + 1] = "Prior answer " .. i end
local previous = table.concat(history, "\n") .. "\n"
local function response(markdown)
  vim.fn.writefile({ vim.json.encode({ schema_version = 1, meeting_id = id, path = directory .. "/meeting.answers.md", markdown = markdown }) }, directory .. "/response.json")
end
vim.fn.writefile({ vim.json.encode({ schema_version = 1, meeting_id = id, passages = {
  { id = "system-1", meeting_id = id, track = "system", start_seconds = 1, end_seconds = 2, text = "Conversation context." },
}, transcription = { meeting_id = id, state = "complete", tracks = { microphone = { state = "complete" }, system = { state = "complete" } } } }) }, directory .. "/transcript.json")
require("trailmix").setup({ cli = cli, mapping = false, transcript_refresh_ms = 100 })
vim.ui.input = function(_, callback) callback("What did we decide?") end
local panel = require("trailmix.panel")
local function text(buffer) return table.concat(vim.api.nvim_buf_get_lines(buffer, 0, -1, false), "\n") end
local function view(window) return vim.api.nvim_win_call(window, vim.fn.winsaveview) end
local function wait_for(predicate) assert(vim.wait(3000, predicate, 10), "answer did not arrive") end
local function keys(sequence) vim.api.nvim_feedkeys(vim.api.nvim_replace_termcodes(sequence, true, false, true), "xt", false) end
local function window_for(kind)
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do if vim.b[vim.api.nvim_win_get_buf(window)].trailmix_kind == kind then return window end end
end
local function at_end(window, buffer)
  assert(vim.api.nvim_win_get_cursor(window)[1] == vim.api.nvim_buf_line_count(buffer), "answer panel did not follow latest content")
end
local ok, err = xpcall(function()
  vim.cmd("edit " .. vim.fn.fnameescape(directory .. "/meeting.md"))
  vim.api.nvim_buf_set_lines(0, 0, -1, false, { marker, "My notes" })
  vim.cmd("silent write")
  local note_window = vim.api.nvim_get_current_win()
  response(previous)
  vim.cmd("TrailMixTranscript")
  local upper = window_for("transcript")
  wait_for(function() return text(vim.api.nvim_win_get_buf(upper)):find("Conversation context.", 1, true) end)
  local transcript_position = view(upper)
  vim.cmd("TrailMixAnswers")
  local lower = window_for("answers")
  local answer = vim.api.nvim_win_get_buf(lower)
  wait_for(function() return text(answer):find("Prior answer 90", 1, true) end)
  vim.api.nvim_win_set_cursor(lower, { 6, 0 })
  response(previous .. "Disk-only refresh\n")
  vim.cmd("TrailMixAnswers")
  wait_for(function() return text(answer):find("Disk-only refresh", 1, true) end)
  assert(view(lower).lnum == 6, "reading saved answers forced following")
  local completed = previous .. "\n## First response\n\n" .. string.rep("New answer text. ", 180) .. "\n"
  response(completed)
  vim.cmd("TrailMixAsk")
  wait_for(function() return text(answer):find("Working…", 1, true) end)
  at_end(lower, answer)
  assert(vim.api.nvim_get_current_win() == note_window, "following took note focus")
  -- Closing the transcript resizes the answer pane, but is not a manual scroll.
  vim.cmd("TrailMixTranscriptToggle")
  wait_for(function() return text(answer):find("First response", 1, true) and not text(answer):find("Working…", 1, true) end)
  at_end(lower, answer)
  vim.cmd("TrailMixTranscriptToggle")
  upper = window_for("transcript")
  wait_for(function() return text(vim.api.nvim_win_get_buf(upper)):find("Conversation context.", 1, true) end)
  assert(vim.api.nvim_get_current_win() == note_window and vim.deep_equal(view(upper), transcript_position), "completion moved another view")
  -- Navigation can happen before CursorMoved fires. Completing a request must
  -- still preserve the older answer the user deliberately scrolled back to.
  completed = completed .. "\n## Second response\n\nMore text.\n"
  response(completed)
  vim.cmd("TrailMixAsk")
  wait_for(function() return text(answer):find("Working…", 1, true) end)
  vim.api.nvim_set_current_win(lower)
  vim.api.nvim_win_call(lower, function() vim.fn.winrestview({ lnum = 6, col = 0, topline = 4 }) end)
  local paused = view(lower)
  vim.cmd("TrailMixAnswers")
  assert(view(lower).lnum == paused.lnum, "reopening a pending question overrode a manual pause")
  wait_for(function() return text(answer):find("Second response", 1, true) and not text(answer):find("Working…", 1, true) end)
  assert(view(lower).lnum == paused.lnum and view(lower).topline == paused.topline, "completion pulled the reader away from older answers")
  -- G resumes following; further content updates keep the newest text visible.
  keys("G")
  at_end(lower, answer)
  vim.api.nvim_set_current_win(note_window)
  completed = completed .. "\nAdditional content\n"
  panel.set_text(answer, completed)
  at_end(lower, answer)
  assert(vim.api.nvim_get_current_win() == note_window)
  -- A counted G is navigation, not a request to follow the tail.
  vim.api.nvim_set_current_win(lower)
  keys("10G")
  panel.set_text(answer, completed .. "Counted motion stays paused\n")
  assert(view(lower).lnum == 10, "counted G resumed following")
  -- Selection and search pause even if they start at the bottom of the answer.
  keys("GV")
  local selected_row = view(lower).lnum
  panel.set_text(answer, completed .. "Selection stays paused\nMore text\n")
  assert(view(lower).lnum == selected_row and vim.fn.mode() == "V", "update disrupted visual selection")
  assert(not text(answer):find("Selection stays paused", 1, true), "rendering changed selected answer text")
  keys("<Esc>")
  wait_for(function() return text(answer):find("Selection stays paused", 1, true) end)
  assert(view(lower).lnum == selected_row, "finishing a selection resumed following")
  -- The last selected row is inclusive, even when it is a trailing blank line.
  keys("GV")
  panel.set_text(answer, text(answer) .. "Replacement of selected text")
  assert(not text(answer):find("Replacement of selected text", 1, true), "an edit changed the last selected row")
  keys("<Esc>")
  wait_for(function() return text(answer):find("Replacement of selected text", 1, true) end)
  keys("G/Prior answer 10<CR>")
  paused = view(lower)
  panel.set_text(answer, completed .. "Search stays paused\n")
  assert(view(lower).lnum == paused.lnum, "update resumed following after search")
  -- A shared answer buffer can be read in two windows independently.
  keys("G")
  local duplicate = vim.api.nvim_open_win(answer, false, { split = "right", win = lower })
  panel.follow_answers(duplicate, false)
  vim.api.nvim_win_set_cursor(duplicate, { 3, 0 })
  vim.api.nvim_set_current_win(note_window)
  panel.set_text(answer, completed .. "Independent views\nAnother line\nAnd another\n")
  at_end(lower, answer)
  assert(view(duplicate).lnum == 3, "following one answer window scrolled another")
  vim.api.nvim_set_current_win(duplicate)
  keys("10GV")
  panel.set_text(answer, completed .. "Independent views\nAnother line\nAnd another\nSelection in another view\n")
  assert(text(answer):find("Selection in another view", 1, true), "an older selection blocked the following window's update")
  at_end(lower, answer)
  assert(view(duplicate).lnum == 10 and vim.fn.mode() == "V", "a shared-buffer update disrupted the older selection")
  keys("<Esc>")
  vim.api.nvim_win_close(duplicate, false)
  vim.api.nvim_set_current_win(note_window)
  -- Closing during a request must not let follow-latest reopen the panel.
  response(completed .. "\n## Closed view response\n")
  vim.cmd("TrailMixAsk")
  wait_for(function() return text(answer):find("Working…", 1, true) end)
  vim.api.nvim_win_close(lower, false)
  wait_for(function() return text(answer):find("Closed view response", 1, true) and not text(answer):find("Working…", 1, true) end)
  assert(not window_for("answers") and #vim.api.nvim_tabpage_list_wins(0) == 2, "late response reopened its closed window")
end, debug.traceback)
vim.fn.delete(directory, "rf")
if not ok then error(err) end
print("TrailMix answer following checks passed")
vim.cmd("qa!")
