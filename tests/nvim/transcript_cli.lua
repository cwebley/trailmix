-- Invoked by the Go CLI test with isolated meeting storage and a freshly built CLI.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
require("trailmix").setup({ cli = vim.env.TRAILMIX_TEST_CLI, mapping = false })
local id = "0123456789abcdef0123456789abcdef"
vim.api.nvim_buf_set_lines(0, 0, -1, false, { "<!-- trailmix: meeting " .. id .. " -->", "@01:03", "- tentative" })
local note = vim.api.nvim_get_current_buf()
vim.cmd("TrailMixTranscript system-later")
assert(vim.wait(3000, function() return vim.api.nvim_get_current_line():find("01:02–01:05 system", 1, true) end, 10), "real CLI passage resolution failed")
assert(vim.bo.readonly and not vim.bo.modifiable)
local text = table.concat(vim.api.nvim_buf_get_lines(0, 0, -1, false), "\n")
assert(text:find("Friday is tentative.", 1, true))
assert(vim.api.nvim_buf_get_lines(note, 2, 3, false)[1] == "- tentative")
vim.cmd("qa!")
