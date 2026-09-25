local M = {}
local note = require("trailmix.note")
local panel = require("trailmix.panel")
local requests, buffers = {}, {}
local prompting = false

local function notify(message)
  vim.notify("TrailMix: " .. message, vim.log.levels.WARN)
end

local function buffer_for(id, opts)
  local buffer = buffers[id]
  if buffer and vim.api.nvim_buf_is_valid(buffer) then return buffer end
  buffer = vim.api.nvim_create_buf(false, true)
  buffers[id] = buffer
  vim.api.nvim_buf_set_name(buffer, "trailmix://answers/" .. id)
  vim.b[buffer].trailmix_kind = "answers"
  vim.b[buffer].trailmix_meeting = id
  vim.bo[buffer].bufhidden = "hide"
  vim.bo[buffer].swapfile = false
  vim.bo[buffer].filetype = "markdown"
  vim.bo[buffer].readonly = true
  vim.bo[buffer].modifiable = false
  panel.keys(buffer)
  vim.keymap.set("n", "gf", function() M.follow(opts, true) end, { buffer = buffer, desc = "Follow TrailMix citation" })
  return buffer
end

function M.attach(buffer, opts)
  if not vim.api.nvim_buf_is_loaded(buffer) or vim.bo[buffer].buftype ~= ""
    or not vim.api.nvim_buf_get_name(buffer):match("%.answers%.md$") then return end
  local first = vim.api.nvim_buf_get_lines(buffer, 0, 1, false)[1] or ""
  local id = first:match("^<!%-%- trailmix: answers ([%x]+) %-%->$")
  if not id or #id ~= 32 or note.id(buffer) ~= id then return end
  vim.b[buffer].trailmix_kind, vim.b[buffer].trailmix_meeting = "answers", id
  panel.wrap_answers(buffer)
  panel.keys(buffer)
  vim.keymap.set("n", "gf", function() M.follow(opts, true) end, { buffer = buffer, desc = "Follow TrailMix citation" })
end

local function identity()
  local buffer = vim.api.nvim_get_current_buf()
  return vim.b[buffer].trailmix_meeting or note.id(buffer)
end

function M.follow(opts, fallback)
  local line, column = vim.api.nvim_get_current_line(), vim.api.nvim_win_get_cursor(0)[2] + 1
  local start = 1
  while true do
    local first, last, id, passage = line:find("%[[^%]]*%]%(%s*trailmix://meetings/([%x]+)/passages/([%w_-]+)%)", start)
    if not first then break end
    if column >= first and column <= last and #id == 32 then
      if identity() and identity() ~= id then notify("citation belongs to another meeting"); return end
      local origin = { buffer = vim.api.nvim_get_current_buf(), position = vim.fn.winsaveview() }
      require("trailmix.transcript").open(id, opts, passage, origin)
      return
    end
    start = last + 1
  end
  if fallback then vim.cmd("normal! gf") else notify("place the cursor on a TrailMix citation") end
end

local function decode(result, id)
  local ok, value = pcall(vim.json.decode, result.stdout or "")
  if result.code ~= 0 then return nil, vim.trim(result.stderr or "") ~= "" and vim.trim(result.stderr) or ("command exited with code " .. result.code) end
  if not ok or type(value) ~= "table" or value.meeting_id ~= id or type(value.markdown) ~= "string" or type(value.path) ~= "string" then
    return nil, "invalid or mismatched answers response"
  end
  return value
end

local function display(buffer, value)
  if not buffer or not vim.api.nvim_buf_is_valid(buffer) then return end
  vim.b[buffer].trailmix_answers_path = value.path
  panel.set_text(buffer, value.markdown)
end

function M.answers(opts)
  local id = identity()
  if not id then notify("this note needs exactly one valid meeting ID"); return end
  local buffer = buffer_for(id, opts)
  panel.show(buffer, false)
  if requests[id] then
    local pending = requests[id]
    if pending.asking then
      panel.set_text(buffer, pending.previous .. "\n\n## Working…\n\n> " .. pending.question:gsub("\n", "\n> ") .. "\n")
    end
    return
  end
  local token = {}
  requests[id] = token
  local ok, process = pcall(vim.system, { opts.cli, "answers", "--meeting", id }, { text = true, timeout = 4000 }, function(result)
    vim.schedule(function()
      if requests[id] ~= token then return end
      requests[id] = nil
      local value, err = decode(result, id)
      if value then display(buffers[id], value) else notify(err) end
    end)
  end)
  if ok then token.process = process else requests[id] = nil; notify("cannot launch " .. opts.cli) end
end

local function submit(opts, id, source, question, selection)
  if requests[id] then notify("a request for this meeting is already pending"); return end
  local buffer = buffer_for(id, opts)
  panel.show(buffer, false)
  local previous = table.concat(vim.api.nvim_buf_get_lines(buffer, 0, -1, false), "\n")
  if previous == "" then previous = "<!-- trailmix: meeting " .. id .. " -->\n# AI answers\n" end
  panel.set_text(buffer, previous .. "\n\n## Working…\n\n> " .. question:gsub("\n", "\n> ") .. "\n")
  local request = { question = question, selection = selection, notes = source.text, note_path = source.path }
  local token = { asking = true, previous = previous, question = question }
  requests[id] = token
  local ok, process = pcall(vim.system, { opts.cli, "ask", "--meeting", id }, {
    text = true, stdin = vim.json.encode(request),
  }, function(result)
    vim.schedule(function()
      if requests[id] ~= token then return end
      requests[id] = nil
      local value, err = decode(result, id)
      if token.cancelled then value, err = nil, "request cancelled" end
      local target = buffers[id]
      if value then
        display(target, value)
        if not target or not vim.api.nvim_buf_is_valid(target) or #vim.fn.win_findbuf(target) == 0 then notify("answer saved to " .. value.path) end
      else
        if target then panel.set_text(target, previous .. "\n\n## Ask failed\n\n" .. err .. "\n") end
        notify(err)
      end
    end)
  end)
  if ok then
    token.process = process
  else
    requests[id] = nil
    panel.set_text(buffer, previous .. "\n\nCannot launch " .. opts.cli .. "\n")
    notify("cannot launch " .. opts.cli)
  end
end

function M.ask(opts, selection)
  if prompting then notify("an ask prompt is already open"); return end
  local buffer, window = vim.api.nvim_get_current_buf(), vim.api.nvim_get_current_win()
  local id = note.id(buffer)
  if not id or vim.bo[buffer].buftype ~= "" or vim.b[buffer].trailmix_kind then notify("ask from the associated meeting note"); return end
  if requests[id] then notify("a request for this meeting is already pending"); return end
  local path = vim.api.nvim_buf_get_name(buffer)
  prompting = true
  vim.ui.input({ prompt = "Ask this meeting: ", default = (selection or ""):gsub("\n", " ") }, function(question)
    vim.schedule(function()
      prompting = false
      if not question or vim.trim(question) == "" then return end
      -- A prompt may be asynchronous. Revalidate before snapshotting unsaved text
      -- or opening a panel in a tab the user has since left.
      if not vim.api.nvim_buf_is_valid(buffer) or not vim.api.nvim_win_is_valid(window)
        or vim.api.nvim_get_current_win() ~= window or vim.api.nvim_get_current_buf() ~= buffer
        or note.id(buffer) ~= id or vim.api.nvim_buf_get_name(buffer) ~= path then
        notify("ask cancelled because the source note changed"); return
      end
      submit(opts, id, { path = path, text = table.concat(vim.api.nvim_buf_get_lines(buffer, 0, -1, false), "\n") .. "\n" }, vim.trim(question), selection or "")
    end)
  end)
end

function M.visual(opts)
  local mode = vim.fn.mode()
  local selection = table.concat(vim.fn.getregion(vim.fn.getpos("v"), vim.fn.getpos("."), { type = mode, exclusive = vim.o.selection == "exclusive" }), "\n")
  local escape = vim.api.nvim_replace_termcodes("<Esc>", true, false, true)
  vim.api.nvim_feedkeys(escape, "nx", false)
  M.ask(opts, selection)
end

function M.cancel()
  local id = identity()
  local request = id and requests[id]
  if not request or not request.asking then notify("no assistant request is pending for this meeting"); return end
  request.cancelled = true
  if request.process then request.process:kill(15) end
end

vim.api.nvim_create_autocmd("VimLeavePre", { callback = function()
  for _, request in pairs(requests) do if request.process then request.process:kill(15) end end
end })

return M
