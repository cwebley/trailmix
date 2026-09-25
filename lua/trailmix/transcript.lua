local M = {}
local views = {}
local display = require("trailmix.transcript_view")
local namespace = vim.api.nvim_create_namespace("trailmix-transcript")

local function notify(message)
  vim.notify("TrailMix: " .. message, vim.log.levels.WARN)
end

local function valid(view)
  if view.closed or not vim.api.nvim_buf_is_valid(view.buffer) then return false end
  local windows = vim.fn.win_findbuf(view.buffer)
  if #windows == 0 then return false end
  local previous = view.window
  if vim.api.nvim_get_current_buf() == view.buffer then
    view.window = vim.api.nvim_get_current_win()
  elseif not vim.api.nvim_win_is_valid(view.window) or vim.api.nvim_win_get_buf(view.window) ~= view.buffer then
    view.window = windows[1]
  end
  for _, window in ipairs(windows) do
    if vim.api.nvim_win_get_tabpage(view.window) ~= vim.api.nvim_get_current_tabpage()
      and vim.api.nvim_win_get_tabpage(window) == vim.api.nvim_get_current_tabpage() then
      view.window = window
    end
  end
  if previous ~= view.window then
    view.navigation = nil
  end
  view.tab = vim.api.nvim_win_get_tabpage(view.window)
  return true
end

local function close(view)
  view.closed = true
  views[view.buffer] = nil
  if view.process then view.process:kill(15) end
  if view.timer then view.timer:stop(); view.timer:close(); view.timer = nil end
  if view.group then vim.api.nvim_del_augroup_by_id(view.group) end
end

local function position(view, window)
  return vim.api.nvim_win_call(window or view.window, vim.fn.winsaveview)
end

local function remember_positions(view)
  view.positions = {}
  for _, window in ipairs(vim.fn.win_findbuf(view.buffer)) do view.positions[window] = position(view, window) end
end

local function moved(view)
  for window, previous in pairs(view.positions or {}) do
    if vim.api.nvim_win_is_valid(window) and vim.api.nvim_win_get_buf(window) == view.buffer
      and not vim.deep_equal(position(view, window), previous) then
      view.follow = false
      view.navigation = nil
    end
  end
end

local function navigate(view)
  local target = view.navigation
  if not target or not view.snapshot then return end
  view.navigation = nil
  -- A delayed fetch cannot take focus back or move another editing target.
  if vim.api.nvim_get_current_win() ~= view.window or vim.api.nvim_get_current_buf() ~= view.buffer then return end
  local selected
  if target.passage then
    for _, passage in ipairs(view.passages) do
      if passage.id == target.passage then selected = passage; break end
    end
    if not selected then notify("passage not found in this meeting: " .. target.passage); return end
  else
    local duration = view.snapshot.duration_seconds
    if duration and target.seconds > duration then
      notify("timestamp exceeds this meeting's duration")
      return
    end
    for _, passage in ipairs(view.passages) do
      if passage.start <= target.seconds then
        selected = passage
        if passage.finish >= target.seconds then break end
      elseif not selected then
        selected = passage
        break
      end
    end
    if not selected then
      if target.seconds == 0 then
        vim.api.nvim_win_set_cursor(view.window, { 1, 0 })
        remember_positions(view)
      else
        notify("no finalized passages near this timestamp yet")
      end
      return
    end
  end
  view.follow = false
  local topline = selected.row
  -- Include preceding conversation without claiming word-level alignment.
  local start = target.seconds or selected.start
  for _, passage in ipairs(view.passages) do
    if passage.finish >= start - 10 then
      topline = math.min(topline, passage.row)
      break
    end
  end
  vim.api.nvim_win_set_cursor(view.window, { selected.row, 0 })
  vim.api.nvim_win_call(view.window, function() vim.fn.winrestview({ lnum = selected.row, col = 0, topline = topline }) end)
  remember_positions(view)
end

local function render(view)
  if not valid(view) or view.searching or view.selecting then return end
  moved(view)
  local result = display.render(view.id, view.snapshot, view.follow, view.failure)
  if not vim.deep_equal(view.lines, result.lines) then
    local saved = {}
    for _, window in ipairs(vim.fn.win_findbuf(view.buffer)) do saved[window] = position(view, window) end
    local rows = {}
    for row, key in ipairs(result.keys) do rows[key] = row end
    local function relocate(row)
      return rows[(view.keys or {})[row]] or math.min(row, #result.lines)
    end
    for _, at in pairs(saved) do at.lnum, at.topline = relocate(at.lnum), relocate(at.topline) end
    vim.bo[view.buffer].readonly = false
    vim.bo[view.buffer].modifiable = true
    vim.api.nvim_buf_set_lines(view.buffer, 0, -1, false, result.lines)
    vim.bo[view.buffer].modifiable = false
    vim.bo[view.buffer].modified = false
    vim.bo[view.buffer].readonly = true
    vim.api.nvim_buf_clear_namespace(view.buffer, namespace, 0, -1)
    for _, highlight in ipairs(result.highlights) do
      vim.api.nvim_buf_set_extmark(view.buffer, namespace, highlight.row - 1, 0, { end_row = highlight.row, hl_group = highlight.group })
    end
    for window, at in pairs(saved) do
      vim.api.nvim_win_call(window, function() vim.fn.winrestview(at) end)
    end
    view.lines, view.keys = result.lines, result.keys
  end
  view.passages = result.passages
  if view.follow then
    vim.api.nvim_win_call(view.window, function() vim.cmd("normal! G") end)
  end
  remember_positions(view)
  navigate(view)
end

local refresh
local function schedule(view)
  if not valid(view) then return end
  view.timer = vim.defer_fn(function()
    view.timer = nil
    refresh(view)
  end, view.interval)
end

refresh = function(view)
  if not valid(view) or view.pending then return end
  if view.timer then view.timer:stop(); view.timer:close(); view.timer = nil end
  if view.tab ~= vim.api.nvim_get_current_tabpage() then schedule(view); return end
  view.pending = true
  local ok, process = pcall(vim.system, { view.cli, "transcript", "--meeting", view.id, "--json" }, { text = true, timeout = 4000 }, function(result)
    vim.schedule(function()
      view.pending, view.process = false, nil
      if not valid(view) then return end
      local decoded, snapshot = pcall(vim.json.decode, result.stdout or "")
      local checked, problem = false, "invalid transcript response"
      if decoded then checked, problem = pcall(display.validate, snapshot, view.id) end
      if result.code ~= 0 then
        view.failure = result.code == 124 and "request timed out" or vim.trim(result.stderr or "")
        if view.failure == "" then view.failure = "CLI exited with code " .. result.code end
      elseif not decoded or not checked then
        view.failure = tostring(problem)
      else
        view.failure, view.snapshot = nil, snapshot
        if view.follow == nil then view.follow = snapshot.transcription.state == "streaming" end
      end
      if view.failure then view.failure = view.failure:gsub("[\r\n]", " "):sub(1, 300) end
      render(view)
      schedule(view)
    end)
  end)
  if ok then
    view.process = process
  else
    view.pending = false
    view.failure = "cannot launch " .. view.cli
    render(view)
    schedule(view)
  end
end

local function request_navigation(view, target)
  view.follow = false
  vim.api.nvim_set_current_win(view.window)
  remember_positions(view)
  view.navigation = target
  render(view)
end

function M.open(id, opts, passage, origin)
  local tab = vim.api.nvim_get_current_tabpage()
  for _, view in pairs(views) do
    if valid(view) and view.tab == tab and (view.id == id or view.buffer == vim.api.nvim_get_current_buf()) then
      if origin then require("trailmix.panel").remember(view.window, origin) end
      if view.buffer == vim.api.nvim_get_current_buf() then view.window = vim.api.nvim_get_current_win() end
      if passage then request_navigation(view, { passage = passage }) end
      refresh(view)
      return view
    end
  end
  if not id then
    notify("this note needs exactly one valid meeting ID")
    return
  end
  local buffer = vim.api.nvim_create_buf(false, true)
  vim.b[buffer].trailmix_kind = "transcript"
  vim.b[buffer].trailmix_meeting = id
  local window = require("trailmix.panel").show(buffer, false, origin)
  local view = { id = id, buffer = buffer, window = window, tab = tab, cli = opts.cli, interval = math.max(100, opts.transcript_refresh_ms or 1000) }
  views[buffer] = view
  vim.api.nvim_buf_set_name(buffer, "trailmix://transcript/" .. id .. "/" .. buffer)
  vim.bo[buffer].bufhidden = "wipe"
  vim.bo[buffer].swapfile = false
  vim.bo[buffer].filetype = "trailmix-transcript"
  vim.bo[buffer].readonly = true
  vim.bo[buffer].modifiable = false
  vim.wo[window].wrap = true
  vim.wo[window].number = false
  vim.wo[window].relativenumber = false
  vim.wo[window].signcolumn = "no"
  view.group = vim.api.nvim_create_augroup("TrailMixTranscript" .. buffer, { clear = true })
  vim.api.nvim_create_autocmd("BufWipeout", { group = view.group, buffer = buffer, once = true, callback = function() close(view) end })
  vim.api.nvim_create_autocmd({ "CursorMoved", "WinScrolled" }, { group = view.group, callback = function()
    if valid(view) then moved(view) end
  end })
  vim.api.nvim_create_autocmd("WinLeave", { group = view.group, buffer = buffer, callback = function() view.navigation = nil end })
  vim.api.nvim_create_autocmd("WinClosed", { group = view.group, callback = function()
    vim.schedule(function()
      if not view.closed and not valid(view) then close(view) end
    end)
  end })
  vim.api.nvim_create_autocmd("ModeChanged", { group = view.group, callback = function()
    local mode = vim.fn.mode(1):sub(1, 1)
    local selecting = vim.api.nvim_get_current_buf() == buffer and (mode == "v" or mode == "V" or mode == "\022" or mode == "s" or mode == "S" or mode == "\019")
    if selecting then
      view.follow, view.navigation, view.selecting = false, nil, true
    elseif view.selecting then
      view.selecting = false
      vim.schedule(function() if valid(view) then render(view) end end)
    end
  end })
  vim.api.nvim_create_autocmd("CmdlineEnter", { group = view.group, callback = function()
    if vim.api.nvim_get_current_buf() == buffer and (vim.fn.getcmdtype() == "/" or vim.fn.getcmdtype() == "?") then
      view.follow, view.navigation, view.searching = false, nil, true
    end
  end })
  vim.api.nvim_create_autocmd("CmdlineLeave", { group = view.group, callback = function()
    if view.searching then
      view.searching = false
      vim.schedule(function() if valid(view) then render(view) end end)
    end
  end })
  vim.keymap.set("n", "G", function()
    view.window = vim.api.nvim_get_current_win()
    view.follow = vim.v.count == 0
    vim.cmd("normal! " .. (vim.v.count > 0 and tostring(vim.v.count) or "") .. "G")
    remember_positions(view)
    render(view)
  end, { buffer = buffer, desc = "Follow latest transcript" })
  require("trailmix.panel").keys(buffer)
  render(view)
  if passage then request_navigation(view, { passage = passage }) end
  refresh(view)
  return view
end

function M.toggle(id, opts)
  id = vim.b[vim.api.nvim_get_current_buf()].trailmix_meeting or id
  if not id then notify("this note needs exactly one valid meeting ID"); return end
  for _, view in pairs(views) do
    if valid(view) and view.tab == vim.api.nvim_get_current_tabpage() and view.id == id then
      local ok, err = pcall(vim.api.nvim_win_close, view.window, false)
      if not ok then notify(tostring(err)) end
      return
    end
  end
  M.open(id, opts)
end

function M.jump(id, opts)
  if not id then notify("this note needs exactly one valid meeting ID"); return end
  local seconds, problem = require("trailmix.anchors").at_cursor(vim.api.nvim_get_current_buf(), vim.api.nvim_win_get_cursor(0)[1])
  if not seconds then notify(problem); return end
  local view = M.open(id, opts)
  if view then request_navigation(view, { seconds = seconds }) end
end

return M
