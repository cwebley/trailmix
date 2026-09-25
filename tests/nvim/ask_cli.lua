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
assert(#vim.api.nvim_tabpage_list_wins(0) == 2)
vim.cmd("TrailMixBack")
assert(vim.api.nvim_get_current_buf() == answer)
assert(vim.api.nvim_buf_get_lines(note, 1, 2, false)[1] == "- unsaved editor question")
vim.cmd("qa!")
