-- Run with nvim --headless -u NONE --noplugin -i NONE -l tests/nvim/timestamp.lua.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
local plugin = require("trailmix")
local directory = vim.fn.tempname()
vim.fn.mkdir(directory, "p")
local cli = directory .. "/fake cli"
vim.fn.writefile({
  "#!/bin/sh",
  'test "$1" = status || exit 2',
  'sleep "$TRAILMIX_TEST_DELAY"',
  'printf "%s\\n" "$TRAILMIX_TEST_STATUS"',
  'printf "%s\\n" "informational stderr" >&2',
  'exit "$TRAILMIX_TEST_EXIT"',
}, cli)
vim.fn.setfperm(cli, "rwx------")
plugin.setup({ cli = cli, mapping = false })
local id = "0123456789abcdef0123456789abcdef"
local marker = "<!-- trailmix: meeting " .. id .. " -->"
local notices = {}
vim.notify = function(message) notices[#notices + 1] = message end

local function prepare(overrides)
  vim.cmd("enew!")
  vim.api.nvim_buf_set_lines(0, 0, -1, false, { marker, "original thought", "more notes" })
  vim.api.nvim_win_set_cursor(0, { 2, 3 })
  vim.wait(30, function() return false end)
  notices = {}
  vim.env.TRAILMIX_TEST_DELAY = "0.08"
  vim.env.TRAILMIX_TEST_EXIT = "0"
  vim.env.TRAILMIX_TEST_STATUS = vim.json.encode(vim.tbl_extend("force", {
    meeting_id = id, state = "recording", live = true, elapsed_seconds = 62.9, clock_age_seconds = 0.1,
  }, overrides or {}))
end

local function wait_for_result()
  assert(vim.wait(2000, function()
    return #notices > 0 or vim.api.nvim_buf_line_count(0) == 4
  end, 10), "timestamp response timed out")
end

local function unchanged()
  assert(vim.api.nvim_buf_line_count(0) == 3, "unsafe timestamp insertion")
  assert(#notices > 0, "rejection was silent")
end

local ok, err = xpcall(function()
  prepare()
  vim.cmd("TrailMixAddTimestamp")
  wait_for_result()
  assert(vim.api.nvim_buf_get_lines(0, 1, 3, false)[1] == "@01:02")
  assert(vim.api.nvim_buf_get_lines(0, 1, 3, false)[2] == "original thought")
  assert(vim.deep_equal(vim.api.nvim_win_get_cursor(0), { 3, 3 }), "cursor moved off original thought")

  prepare({ elapsed_seconds = 3661.8 })
  plugin.add_timestamp()
  wait_for_result()
  assert(vim.api.nvim_buf_get_lines(0, 1, 2, false)[1] == "@01:01:01")

  for _, state in ipairs({
    { live = false }, { state = "stopped" }, { state = "starting" },
    { meeting_id = "another-meeting" }, { elapsed_seconds = -1 }, { error = "capture failed" },
    { clock_age_seconds = 8 },
  }) do
    prepare(state)
    plugin.add_timestamp()
    wait_for_result()
    unchanged()
  end

  prepare()
  vim.env.TRAILMIX_TEST_EXIT = "1"
  plugin.add_timestamp()
  wait_for_result()
  unchanged()

  prepare()
  plugin.add_timestamp()
  vim.api.nvim_buf_set_lines(0, 2, 3, false, { "edited while waiting" })
  wait_for_result()
  unchanged()

  prepare()
  plugin.add_timestamp()
  vim.api.nvim_win_set_cursor(0, { 3, 0 })
  wait_for_result()
  unchanged()

  prepare()
  local original = vim.api.nvim_get_current_buf()
  plugin.add_timestamp()
  vim.cmd("enew!")
  vim.api.nvim_buf_set_lines(0, 0, -1, false, { "unrelated buffer" })
  assert(vim.wait(2000, function() return #notices > 0 end, 10))
  assert(vim.api.nvim_buf_line_count(0) == 1)
  assert(vim.api.nvim_buf_line_count(original) == 3)

  -- Returning to the same buffer does not revive a canceled insertion.
  prepare()
  original = vim.api.nvim_get_current_buf()
  plugin.add_timestamp()
  vim.cmd("enew!")
  vim.api.nvim_set_current_buf(original)
  wait_for_result()
  unchanged()

  prepare()
  plugin.add_timestamp()
  vim.api.nvim_buf_set_lines(0, 0, 1, false, { "<!-- trailmix: meeting ffffffffffffffffffffffffffffffff -->" })
  wait_for_result()
  unchanged()

  -- Reopening a note resolves the ID from its content, with no editor cache.
  prepare()
  local note = directory .. "/note.md"
  vim.cmd("write! " .. vim.fn.fnameescape(note))
  vim.cmd("bwipeout!")
  vim.cmd("edit " .. vim.fn.fnameescape(note))
  vim.api.nvim_win_set_cursor(0, { 2, 0 })
  vim.wait(30, function() return false end)
  plugin.add_timestamp()
  wait_for_result()
  assert(vim.api.nvim_buf_get_lines(0, 1, 2, false)[1] == "@01:02")
end, debug.traceback)

vim.fn.delete(directory, "rf")
if not ok then error(err) end
print("TrailMix timestamp checks passed")
vim.cmd("qa!")
