-- Cross the detached worker -> private result -> real editor discovery path.
-- The Go parent configured a local fake assistant, never a real provider.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
local notices = {}
vim.notify = function(message) notices[#notices + 1] = message end
vim.cmd("edit " .. vim.fn.fnameescape(vim.env.TRAILMIX_TEST_NOTE))
local buffer, window = vim.api.nvim_get_current_buf(), vim.api.nvim_get_current_win()
vim.api.nvim_buf_set_lines(buffer, -1, -1, false, { "Unsaved notes typed after background generation." })
vim.api.nvim_win_set_cursor(window, { vim.api.nvim_buf_line_count(buffer), 5 })
require("trailmix").setup({ cli = vim.env.TRAILMIX_CLI, recap_refresh_ms = 100 })
local function text()
  return table.concat(vim.api.nvim_buf_get_lines(buffer, 0, -1, false), "\n")
end
local function await(predicate, label)
  assert(vim.wait(15000, predicate, 10), label .. ": " .. table.concat(notices, "\n"))
end
await(function() return text():find("## Summary", 1, true) end, "ready background result was not discovered")
assert(text():find("Unsaved notes typed after background generation.", 1, true), "automatic pickup lost current rough notes")
assert(vim.api.nvim_get_current_win() == window and vim.api.nvim_get_current_line() == "Unsaved notes typed after background generation.", "automatic pickup changed cursor or focus")
assert(vim.bo[buffer].modified, "automatic pickup saved the note")
assert(text():find("<!-- trailmix: recap result ", 1, true), "applied result identity missing")
assert(text():find("trailmix://meetings/", 1, true), "background result lost citations")
local applied = text()
vim.wait(400, function() return false end)
assert(text() == applied, "polling repeatedly replaced the same result")
vim.cmd("undo")
assert(not text():find("## Summary", 1, true) and text():find("Unsaved notes typed after background generation.", 1, true), "undo lost rough edits")
vim.wait(500, function() return false end)
assert(not text():find("## Summary", 1, true), "automatic pickup immediately undid the user's undo")
vim.cmd("TrailMixRecapRestore")
await(function() return text():find("## Summary", 1, true) end, "explicit restore did not override auto-apply suppression")
-- Discard this fixture buffer's unsaved changes, then load the disk note again.
-- Its privately saved recap must be discoverable without a new provider call.
vim.api.nvim_buf_delete(buffer, { force = true })
vim.cmd("edit " .. vim.fn.fnameescape(vim.env.TRAILMIX_TEST_NOTE))
buffer = vim.api.nvim_get_current_buf()
await(function() return text():find("## Summary", 1, true) end, "reopened note did not pick up its pending result")
vim.cmd("qa!")
