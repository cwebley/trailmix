-- Invoked by Go with a built CLI and a fake assistant executable in isolated storage.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
require("trailmix").setup({ cli = vim.env.TRAILMIX_TEST_CLI, mapping = false, ask_mapping = false })
vim.cmd("edit " .. vim.fn.fnameescape(vim.env.TRAILMIX_TEST_NOTE))
local note = vim.api.nvim_get_current_buf()
vim.api.nvim_buf_set_lines(note, 1, -1, false, { "- unsaved editor question" })
vim.ui.input = function(_, callback) callback("What is tentative?") end
vim.cmd("TrailMixAsk")
local right
assert(vim.wait(5000, function()
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do
    if vim.api.nvim_win_get_buf(window) ~= note then
      right = window
      for _, line in ipairs(vim.api.nvim_buf_get_lines(vim.api.nvim_win_get_buf(window), 0, -1, false)) do
        if line:find("[01:02](trailmix://", 1, true) then return true end
      end
    end
  end
end, 10), "real CLI answer did not arrive")
assert(vim.api.nvim_get_current_buf() == note and vim.bo[note].modified)
vim.api.nvim_set_current_win(right)
local answer = vim.api.nvim_get_current_buf()
for row, line in ipairs(vim.api.nvim_buf_get_lines(answer, 0, -1, false)) do
  local column = line:find("[01:02](trailmix://", 1, true)
  if column then vim.api.nvim_win_set_cursor(right, { row, column - 1 }); break end
end
vim.cmd("TrailMixFollowCitation")
assert(vim.wait(3000, function() return vim.api.nvim_get_current_line():find("01:02–01:05 system", 1, true) end, 10), "real citation did not resolve")
assert(#vim.api.nvim_tabpage_list_wins(0) == 3)
assert(vim.api.nvim_win_get_buf(right) == answer, "citation replaced the answer")
vim.cmd("TrailMixBack")
assert(vim.api.nvim_get_current_buf() == answer)
assert(vim.api.nvim_buf_get_lines(note, 1, 2, false)[1] == "- unsaved editor question")
-- A transcript-origin request uses the unsaved note and saves the quote with
-- source links through the real CLI, not just the editor's fake command path.
local transcript_window
for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do
  if vim.b[vim.api.nvim_win_get_buf(window)].trailmix_kind == "transcript" then transcript_window = window end
end
vim.api.nvim_set_current_win(transcript_window)
local transcript = vim.api.nvim_get_current_buf()
for row, line in ipairs(vim.api.nvim_buf_get_lines(transcript, 0, -1, false)) do
  if line == "Friday is tentative." then vim.api.nvim_win_set_cursor(transcript_window, { row, 0 }); break end
end
vim.cmd("TrailMixAsk")
assert(vim.wait(5000, function()
  local text = table.concat(vim.api.nvim_buf_get_lines(answer, 0, -1, false), "\n")
  return text:find("### Selected transcript\n\n> Friday is tentative.", 1, true) and not text:find("Working…", 1, true)
end, 10), "real CLI did not retain the transcript quote")
assert(vim.api.nvim_get_current_win() == transcript_window and vim.bo[note].modified)
-- With no loaded note buffer, the CLI supplies the associated saved note.
vim.api.nvim_buf_delete(note, { force = true })
vim.cmd("TrailMixAsk")
assert(vim.wait(5000, function()
  local text = table.concat(vim.api.nvim_buf_get_lines(answer, 0, -1, false), "\n")
  local _, count = text:gsub("### Selected transcript", "")
  return count == 2 and not text:find("Working…", 1, true)
end, 10), "asking without a loaded note did not use saved notes")
-- Selecting the whole note includes its identity marker. Quoting that marker
-- must not make the saved answer file lose its meeting identity or mappings.
vim.cmd("edit " .. vim.fn.fnameescape(vim.env.TRAILMIX_TEST_NOTE))
vim.cmd("%TrailMixAsk")
assert(vim.wait(5000, function()
  local text = table.concat(vim.api.nvim_buf_get_lines(answer, 0, -1, false), "\n")
  return text:find("### Selected note text\n\n> <!-- trailmix: meeting", 1, true) and not text:find("Working…", 1, true)
end, 10), "whole-note quote was not saved")
local saved_path = vim.b[answer].trailmix_answers_path
vim.api.nvim_buf_delete(answer, { force = true })
vim.cmd("edit " .. vim.fn.fnameescape(saved_path))
assert(vim.b.trailmix_kind == "answers" and vim.fn.maparg("gf", "n", false, true).buffer == 1, "quoted meeting marker disabled saved-answer navigation")
vim.cmd("qa!")
