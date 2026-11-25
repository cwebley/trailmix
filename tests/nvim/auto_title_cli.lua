-- The Go parent provides isolated notes, CLI, and a fake assistant.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
local notices = {}
vim.notify = function(message) notices[#notices + 1] = message end
local path = assert(vim.env.TRAILMIX_TEST_NOTE)
vim.cmd("edit " .. vim.fn.fnameescape(path))
local buffer = vim.api.nvim_get_current_buf()
local original = vim.fn.readfile(path)
vim.api.nvim_buf_set_lines(buffer, -1, -1, false, { "Unsaved detail before the recap request." })
vim.api.nvim_win_set_cursor(0, { vim.api.nvim_buf_line_count(buffer), 0 })
require("trailmix").setup({ cli = vim.env.TRAILMIX_CLI, recap_refresh_ms = 100 })
local function text() return table.concat(vim.api.nvim_buf_get_lines(buffer, 0, -1, false), "\n") end
local function await(predicate, message)
  assert(vim.wait(20000, predicate, 10), message .. ": " .. table.concat(notices, "\n"))
end
-- setup registers the live editor before the parent queues the background job.
vim.fn.writefile({ "ready" }, assert(vim.env.TRAILMIX_TEST_EDITOR_READY))
await(function() return text():find("## Summary", 1, true) end, "background recap was not inserted")
await(function() return vim.fn.filereadable(vim.env.TRAILMIX_TEST_TITLE_INTENT) == 1 end, "automatic name was not selected")
assert(vim.deep_equal(vim.fn.readfile(path), original), "generation or naming wrote the live note")
assert(vim.uv.fs_realpath(vim.api.nvim_buf_get_name(buffer)) == vim.uv.fs_realpath(path), "naming changed an open buffer's filename")
assert(vim.bo[buffer].modified, "automatic recap saved the live buffer")
assert(text():find("Unsaved detail before the recap request.", 1, true), "recap lost unsaved input notes")
local intent = vim.json.decode(table.concat(vim.fn.readfile(vim.env.TRAILMIX_TEST_TITLE_INTENT), "\n"))
assert(intent.state == "pending", "name should remain pending while the editor is open")
-- A later edit is outside the assistant's snapshot. Naming must preserve it too.
vim.api.nvim_buf_set_lines(buffer, -1, -1, false, { "Fresh notes added after the recap was ready." })
vim.cmd("write") -- Only this explicit user-equivalent action saves the note.
vim.fn.writefile(vim.api.nvim_buf_get_lines(buffer, 0, -1, false), vim.env.TRAILMIX_TEST_SAVED_NOTE)
vim.cmd("qa") -- VimLeavePre must launch a detached retry that survives exit.
