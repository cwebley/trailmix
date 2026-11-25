-- Real CLI + fixture assistant; no capture or network.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
local notices = {}
vim.notify = function(message) notices[#notices + 1] = message end
require("trailmix").setup({ cli = vim.env.TRAILMIX_CLI, recap_on_stop = false })
vim.cmd("edit " .. vim.fn.fnameescape(vim.env.TRAILMIX_TEST_NOTE))
local buffer, window = vim.api.nvim_get_current_buf(), vim.api.nvim_get_current_win()
local function text()
  return table.concat(vim.api.nvim_buf_get_lines(buffer, 0, -1, false), "\n")
end
local function await(predicate, label)
  assert(vim.wait(10000, predicate, 10), label .. ": " .. table.concat(notices, "\n"))
end
vim.api.nvim_buf_set_lines(buffer, -1, -1, false, { "Unsaved editor question" })
vim.cmd("TrailMixRecap")
-- Make further edits while the subprocess reads its earlier snapshot.
vim.api.nvim_buf_set_lines(buffer, -1, -1, false, { "More rough notes while generating." })
local row = vim.api.nvim_buf_line_count(buffer)
vim.api.nvim_win_set_cursor(window, { row, 4 })
await(function() return text():find("## Summary", 1, true) end, "recap did not arrive")
assert(text():find("[01:02](trailmix://meetings/0123456789abcdef0123456789abcdef/passages/system-1)", 1, true), "citation did not cross CLI boundary")
assert(text():find("Original saved note.\nUnsaved editor question\nMore rough notes while generating.", 1, true), "current rough notes lost")
assert(vim.api.nvim_get_current_win() == window and vim.api.nvim_get_current_buf() == buffer, "recap changed focus")
assert(vim.api.nvim_get_current_line() == "More rough notes while generating.", "recap moved rough-note cursor")
assert(vim.bo[buffer].modified, "recap saved the buffer")
assert(text():find("# Meeting\n", 1, true) and not text():find("# Friday planning\n", 1, true), "suggested title was applied")
local _, boundaries = text():gsub("## Rough notes", "")
assert(boundaries == 1, "rough boundary duplicated")
-- Citations in the main note resolve through the real transcript CLI.
for index, line in ipairs(vim.api.nvim_buf_get_lines(buffer, 0, -1, false)) do
  local column = line:find("[01:02]", 1, true)
  if column then vim.api.nvim_win_set_cursor(window, { index, column - 1 }); break end
end
vim.cmd("TrailMixFollowCitation")
await(function() return vim.api.nvim_get_current_line():find("01:02–01:05 system", 1, true) end, "recap citation did not resolve")
vim.cmd("TrailMixBack")
assert(vim.api.nvim_get_current_buf() == buffer and text():find("More rough notes while generating.", 1, true), "citation navigation lost note edits")
-- Regeneration replaces the owned region, not user text below it.
local lines = vim.api.nvim_buf_get_lines(buffer, 0, -1, false)
for index, line in ipairs(lines) do
  if line == "## Summary" then
    vim.api.nvim_buf_set_lines(buffer, index, index, false, { "Discard this generated-section edit." })
    break
  end
end
-- A cursor left in the generated section deliberately defers replacement.
-- Continue from Rough notes for this regeneration-preservation check.
vim.api.nvim_win_set_cursor(window, { vim.api.nvim_buf_line_count(buffer), 0 })
vim.cmd("TrailMixRecap")
await(function() return not text():find("Discard this generated-section edit.", 1, true) end, "regeneration did not replace generated edits")
assert(text():find("More rough notes while generating.", 1, true), "regeneration lost rough notes")
vim.cmd("TrailMixRecapRestore")
vim.wait(300, function() return false end)
assert(text():find("More rough notes while generating.", 1, true), "restore lost rough notes")
vim.cmd("qa!")
