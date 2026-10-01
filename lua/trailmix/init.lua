local M = {}
local options = {
  cli = vim.env.TRAILMIX_CLI or "trailmix", mapping = "<leader>mt", ask_mapping = "<leader>ma", transcript_mapping = "<leader>mT",
  transcript_jump_mapping = "<leader>ml", note_jump_mapping = "<leader>mh", note_jump_back_mapping = "<leader>mH",
}
local pending = false
local installed = {}

local function notify(message)
  vim.notify("TrailMix: " .. message, vim.log.levels.WARN)
end

local meeting_id = require("trailmix.note").id

function M.add_timestamp()
  if pending then
    notify("a timestamp request is already pending")
    return
  end
  local buffer, window = vim.api.nvim_get_current_buf(), vim.api.nvim_get_current_win()
  -- From the transcript, stamp the passage under the cursor into the note.
  if vim.b[buffer].trailmix_kind == "transcript" then
    require("trailmix.transcript").stamp()
    return
  end
  local id = meeting_id(buffer)
  if not id then
    notify("this note needs exactly one valid meeting ID")
    return
  end
  if not vim.bo[buffer].modifiable then
    notify("this buffer is not modifiable")
    return
  end
  local tick = vim.api.nvim_buf_get_changedtick(buffer)
  local cursor = vim.api.nvim_win_get_cursor(window)
  local canceled = false
  -- Even switching away and back cancels the insertion. No extmark relocation
  -- or changed-text guesswork: the user can request another timestamp.
  local autocmd = vim.api.nvim_create_autocmd({ "BufLeave", "WinLeave", "CursorMoved", "CursorMovedI", "TextChanged", "TextChangedI" }, {
    callback = function() canceled = true end,
  })
  pending = true
  local function finish(result)
    vim.schedule(function()
      pending = false
      vim.api.nvim_del_autocmd(autocmd)
      if canceled or not vim.api.nvim_buf_is_valid(buffer) or not vim.api.nvim_win_is_valid(window)
        or vim.api.nvim_get_current_buf() ~= buffer or vim.api.nvim_get_current_win() ~= window
        or vim.api.nvim_buf_get_changedtick(buffer) ~= tick or meeting_id(buffer) ~= id
        or not vim.deep_equal(vim.api.nvim_win_get_cursor(window), cursor) or not vim.bo[buffer].modifiable then
        notify("timestamp canceled because the editing target changed")
        return
      end
      local state, problem = require("trailmix.cli").decode(result.stdout, "status")
      if result.code == 0 and not state then
        notify(problem)
        return
      end
      if result.code ~= 0 or state.live ~= true
        or state.state ~= "recording" or state.meeting_id ~= id or (state.error and state.error ~= "") then
        notify("the same meeting must be actively recording through a reachable recorder")
        return
      end
      local elapsed = state.elapsed_seconds
      local age = state.clock_age_seconds
      if type(age) ~= "number" or age < 0 or age ~= age or age > 5 then
        notify("recorder's capture clock is stale")
        return
      end
      if type(elapsed) ~= "number" or elapsed < 0 or elapsed ~= elapsed or elapsed == math.huge then
        notify("recorder returned an invalid capture timestamp")
        return
      end
      local anchor = require("trailmix.anchors").format(elapsed)
      vim.api.nvim_buf_set_lines(buffer, cursor[1] - 1, cursor[1] - 1, false, { anchor })
      vim.api.nvim_win_set_cursor(window, { cursor[1] + 1, cursor[2] })
    end)
  end
  local ok = pcall(vim.system, { options.cli, "status" }, { text = true, timeout = 4000 }, finish)
  if not ok then
    pending = false
    vim.api.nvim_del_autocmd(autocmd)
    notify("cannot launch " .. options.cli)
  end
end

function M.setup(opts)
  for _, mapping in ipairs(installed) do pcall(vim.keymap.del, mapping[1], mapping[2]) end
  installed = {}
  options = vim.tbl_extend("force", options, opts or {})
  vim.api.nvim_create_user_command("TrailMixAddTimestamp", M.add_timestamp, { desc = "Insert the active meeting's capture timestamp", force = true })
  vim.api.nvim_create_user_command("TrailMixTranscript", function(command)
    require("trailmix.transcript").open(meeting_id(vim.api.nvim_get_current_buf()), options, command.args ~= "" and command.args or nil)
  end, { desc = "Open this meeting's transcript, optionally at a passage ID", nargs = "?", force = true })
  vim.api.nvim_create_user_command("TrailMixTranscriptJump", function()
    require("trailmix.transcript").jump(meeting_id(vim.api.nvim_get_current_buf()), options)
  end, { desc = "Jump to the current or preceding timestamp's transcript", force = true })
  local function toggle_transcript()
    require("trailmix.transcript").toggle(meeting_id(vim.api.nvim_get_current_buf()), options)
  end
  vim.api.nvim_create_user_command("TrailMixTranscriptToggle", toggle_transcript, { desc = "Toggle this meeting's transcript", force = true })
  vim.api.nvim_create_user_command("TrailMixNoteJump", function() require("trailmix.transcript").note_jump(1) end,
    { desc = "Jump from a transcript passage to the closest note timestamp, or to the next-closest", force = true })
  vim.api.nvim_create_user_command("TrailMixNoteJumpBack", function() require("trailmix.transcript").note_jump(-1) end,
    { desc = "Step back to the previous note timestamp candidate", force = true })
  vim.api.nvim_create_user_command("TrailMixAsk", function(command)
    local selection
    if command.range > 0 then selection = table.concat(vim.api.nvim_buf_get_lines(0, command.line1 - 1, command.line2, false), "\n") end
    require("trailmix.ask").ask(options, selection)
  end, { desc = "Ask about this meeting", range = true, force = true })
  vim.api.nvim_create_user_command("TrailMixAnswers", function() require("trailmix.ask").answers(options) end, { desc = "Open saved meeting answers", force = true })
  vim.api.nvim_create_user_command("TrailMixAskCancel", function() require("trailmix.ask").cancel() end, { desc = "Cancel this meeting's assistant request", force = true })
  vim.api.nvim_create_user_command("TrailMixFollowCitation", function() require("trailmix.ask").follow(options) end, { desc = "Show cited transcript evidence", force = true })
  vim.api.nvim_create_user_command("TrailMixBack", function() require("trailmix.panel").back() end, { desc = "Return to the answer view", force = true })
  local group = vim.api.nvim_create_augroup("TrailMixAnswerFiles", { clear = true })
  vim.api.nvim_create_autocmd({ "BufReadPost", "FileType" }, { group = group, callback = function(event)
    require("trailmix.ask").attach(event.buf, options)
  end })
  for _, buffer in ipairs(vim.api.nvim_list_bufs()) do require("trailmix.ask").attach(buffer, options) end
  -- false disables a mapping. Remember what was set so setup can run again.
  local function map(modes, lhs, rhs, desc)
    if not lhs then return end
    vim.keymap.set(modes, lhs, rhs, { desc = desc })
    for _, mode in ipairs(modes) do installed[#installed + 1] = { mode, lhs } end
  end
  map({ "n", "i" }, options.mapping, M.add_timestamp, "TrailMix timestamp")
  map({ "n" }, options.ask_mapping, function() require("trailmix.ask").ask(options) end, "Ask TrailMix")
  map({ "x" }, options.ask_mapping, function() require("trailmix.ask").visual(options) end, "Ask TrailMix about selection")
  map({ "n" }, options.transcript_mapping, toggle_transcript, "Toggle TrailMix transcript")
  map({ "n" }, options.transcript_jump_mapping, function() vim.cmd("TrailMixTranscriptJump") end, "Jump from note to transcript")
  map({ "n" }, options.note_jump_mapping, function() require("trailmix.transcript").note_jump(1) end, "Jump from transcript to note")
  map({ "n" }, options.note_jump_back_mapping, function() require("trailmix.transcript").note_jump(-1) end, "Previous note timestamp candidate")
end

return M
