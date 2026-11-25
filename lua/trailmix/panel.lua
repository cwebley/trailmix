-- Transcript above answers in the right column; either view can occupy it alone.
local M = {}
local history = {}
local saved, restores = {}, {}
local row_keys = {}
local answer_views = {}
local deferred_answers = {}

local function managed(buffer)
  return vim.api.nvim_buf_is_valid(buffer) and vim.b[buffer].trailmix_kind ~= nil
end

local function answer_position(window, state)
  if not vim.api.nvim_win_is_valid(window) or vim.api.nvim_win_get_buf(window) ~= state.buffer then
    answer_views[window] = nil
    return
  end
  return vim.api.nvim_win_call(window, vim.fn.winsaveview)
end

local function answer_geometry(window)
  return { width = vim.api.nvim_win_get_width(window), height = vim.api.nvim_win_get_height(window) }
end

local function remember_answer(window, state)
  state.position = answer_position(window, state)
  if state.position then state.geometry = answer_geometry(window) end
end

local function answer_moved(window, state)
  local position = answer_position(window, state)
  if not position then return end
  local geometry = answer_geometry(window)
  local resized = not vim.deep_equal(geometry, state.geometry)
  if not vim.deep_equal(position, state.position) and (not resized
    or position.lnum ~= state.position.lnum or position.col ~= state.position.col) then state.following = false end
  state.position, state.geometry = position, geometry
end

local function reading_answer(buffer)
  if vim.api.nvim_get_current_buf() ~= buffer then return false end
  local mode, command = vim.fn.mode(1):sub(1, 1), vim.fn.getcmdtype()
  return mode == "v" or mode == "V" or mode == "\022" or mode == "s" or mode == "S" or mode == "\019"
    or command == "/" or command == "?"
end

local function scroll_answer(window, state)
  if not answer_position(window, state) then return end
  if state.following then
    vim.api.nvim_win_call(window, function() vim.cmd("normal! G$zb") end)
  end
  remember_answer(window, state)
end

local function scroll_answers(buffer)
  for window, state in pairs(answer_views) do if state.buffer == buffer then scroll_answer(window, state) end end
end

function M.follow_answers(window, enabled)
  local buffer = vim.api.nvim_win_get_buf(window)
  if vim.b[buffer].trailmix_kind ~= "answers" then return end
  -- Reopening a pending request should retain an existing manual pause.
  if enabled == nil and answer_views[window] and answer_views[window].buffer == buffer then return end
  -- An explicit new question or G overrides a saved reading position.
  restores[window] = nil
  answer_views[window] = {
    buffer = buffer, following = enabled ~= false, position = vim.api.nvim_win_call(window, vim.fn.winsaveview), geometry = answer_geometry(window),
  }
  scroll_answer(window, answer_views[window])
end

function M.wrap_answers(buffer)
  if not vim.api.nvim_buf_is_valid(buffer) or vim.b[buffer].trailmix_kind ~= "answers" then return end
  for _, window in ipairs(vim.fn.win_findbuf(buffer)) do
    vim.wo[window].wrap = true
    vim.wo[window].linebreak = true
  end
end

function M.remember(window, origin)
  history[window] = history[window] or {}
  table.insert(history[window], origin)
end

function M.show(buffer, focus, origin)
  local current = vim.api.nvim_get_current_win()
  local kind = vim.b[buffer].trailmix_kind
  local target, fallback, opposite, opposite_fallback
  local positions = {}
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do
    if answer_views[window] then answer_moved(window, answer_views[window]) end
    local shown = vim.api.nvim_win_get_buf(window)
    positions[window] = { buffer = shown, view = vim.api.nvim_win_call(window, vim.fn.winsaveview) }
    -- The focused window holding an ordinary buffer is where the user is
    -- editing, whatever tag it kept. Answers must not replace that note.
    if vim.w[window].trailmix_panel and window == current and not managed(shown) then
      vim.w[window].trailmix_panel = nil
      vim.w[window].trailmix_panel_kind = nil
    end
    local shown_kind = vim.b[shown].trailmix_kind or vim.w[window].trailmix_panel_kind
    if shown == buffer then
      target = window
    elseif kind and shown_kind == kind then
      if vim.w[window].trailmix_panel then fallback = window else fallback = fallback or window end
    elseif shown_kind then
      if vim.w[window].trailmix_panel then opposite = window else opposite_fallback = opposite_fallback or window end
    end
  end
  target = target or fallback
  opposite = opposite or opposite_fallback
  local changing = not target or vim.api.nvim_win_get_buf(target) ~= buffer
  if not target then
    target = vim.api.nvim_open_win(buffer, false, {
      split = opposite and (kind == "answers" and "below" or "above") or "right", win = opposite or current,
    })
  elseif vim.api.nvim_win_get_buf(target) ~= buffer then
    -- Keep unrelated unsaved buffers if the user used :edit or :enew in this
    -- slot. Reusing a panel must not abandon their text or create a third pane.
    vim.api.nvim_win_call(target, function() vim.cmd("hide buffer " .. buffer) end)
  end
  if origin then M.remember(target, origin) end
  vim.w[target].trailmix_panel = true
  vim.w[target].trailmix_panel_kind = kind
  if changing then
    local tab, id = vim.api.nvim_get_current_tabpage(), vim.b[buffer].trailmix_meeting
    local prior = saved[tab] and saved[tab][id] and saved[tab][id][kind]
    restores[target] = prior and {
      buffer = buffer, position = prior.position, follow = prior.follow, cursor_key = prior.cursor_key, top_key = prior.top_key,
    } or nil
    answer_views[target] = kind == "answers" and prior and prior.answer_follow ~= nil and {
      buffer = buffer, following = prior.answer_follow, position = vim.api.nvim_win_call(target, vim.fn.winsaveview), geometry = answer_geometry(target),
    } or nil
  end
  for window, at in pairs(positions) do
    if vim.api.nvim_win_get_buf(window) == at.buffer then
      vim.api.nvim_win_call(window, function() vim.fn.winrestview(at.view) end)
    end
  end
  M.wrap_answers(buffer)
  if kind == "answers" and vim.api.nvim_buf_get_lines(buffer, 0, 1, false)[1] ~= "" then M.restore(target) end
  -- Resizing the right column is layout work, not a manual scroll.
  for window, state in pairs(answer_views) do remember_answer(window, state) end
  if focus then vim.api.nvim_set_current_win(target) end
  return target, restores[target]
end

-- Transcript row identities survive late arrivals and buffer recreation.
function M.track_rows(buffer, keys)
  row_keys[buffer] = keys
end

-- A reopened transcript is initially empty. Restore once its first snapshot
-- arrives, rather than clamping the saved cursor to that empty buffer.
function M.restore(window, following)
  local prior = restores[window]
  if not prior then return end
  restores[window] = nil
  if not following and vim.api.nvim_win_get_buf(window) == prior.buffer then
    local at = vim.deepcopy(prior.position)
    for row, key in ipairs(row_keys[prior.buffer] or {}) do
      if key == prior.cursor_key then at.lnum = row end
      if key == prior.top_key then at.topline = row end
    end
    vim.api.nvim_win_call(window, function() vim.fn.winrestview(at) end)
    if answer_views[window] then remember_answer(window, answer_views[window]) end
  end
end

local function restore_buffer(buffer)
  for _, window in ipairs(vim.fn.win_findbuf(buffer)) do M.restore(window) end
end

function M.back()
  local window = vim.api.nvim_get_current_win()
  local stack = history[window] or {}
  while #stack > 0 do
    local prior = table.remove(stack)
    if vim.api.nvim_buf_is_valid(prior.buffer) then
      local target = prior.window
      if not target or not vim.api.nvim_win_is_valid(target) or vim.api.nvim_win_get_buf(target) ~= prior.buffer
        or vim.api.nvim_win_get_tabpage(target) ~= vim.api.nvim_get_current_tabpage() then
        if managed(prior.buffer) then
          target = M.show(prior.buffer, true)
        else
          -- A citation may also have been clicked in an ordinary note. If its
          -- window closed, restore it in a new split without replacing another note.
          target = vim.api.nvim_open_win(prior.buffer, true, { split = "left", win = window })
        end
      else
        vim.api.nvim_set_current_win(target)
      end
      M.wrap_answers(prior.buffer)
      M.restore(target)
      vim.fn.winrestview(prior.position)
      return
    end
  end
  vim.notify("TrailMix: no previous view to return to", vim.log.levels.WARN)
end

function M.set_text(buffer, text)
  if not vim.api.nvim_buf_is_valid(buffer) then return end
  local lines = vim.split(text, "\n", { plain = true })
  local old = vim.api.nvim_buf_get_lines(buffer, 0, -1, false)
  local first, last_old, last_new = 1, #old, #lines
  while first <= last_old and first <= last_new and old[first] == lines[first] do first = first + 1 end
  while last_old >= first and last_new >= first and old[last_old] == lines[last_new] do
    last_old, last_new = last_old - 1, last_new - 1
  end
  if vim.b[buffer].trailmix_kind == "answers" and reading_answer(buffer) then
    local state = answer_views[vim.api.nvim_get_current_win()]
    if state then state.following = false end
    local protected = math.max(vim.fn.line("w$"), vim.api.nvim_win_get_cursor(0)[1])
    if vim.fn.mode(1):sub(1, 1) ~= "c" then protected = math.max(protected, vim.fn.getpos("v")[2]) end
    -- An update beyond the reader's selection/viewport is safe, including in
    -- another window sharing this buffer. Defer edits to the text being read.
    if first <= protected and (first <= last_old or first <= last_new) then
      deferred_answers[buffer] = text
      return
    end
  end
  deferred_answers[buffer] = nil
  -- CursorMoved may not have fired yet when a subprocess completes.
  local paused = {}
  for window, state in pairs(answer_views) do
    if state.buffer == buffer then
      answer_moved(window, state)
      if not state.following then paused[window] = state.position end
    end
  end
  if first > last_old and first > last_new then restore_buffer(buffer); scroll_answers(buffer); return end
  vim.bo[buffer].readonly = false
  vim.bo[buffer].modifiable = true
  vim.api.nvim_buf_set_lines(buffer, first - 1, last_old, false, vim.list_slice(lines, first, last_new))
  vim.bo[buffer].modified = false
  vim.bo[buffer].modifiable = false
  vim.bo[buffer].readonly = true
  for window, position in pairs(paused) do
    if vim.api.nvim_win_is_valid(window) and vim.api.nvim_win_get_buf(window) == buffer then
      vim.api.nvim_win_call(window, function() vim.fn.winrestview(position) end)
    end
  end
  restore_buffer(buffer)
  scroll_answers(buffer)
end

function M.keys(buffer)
  if vim.b[buffer].trailmix_kind == "answers" then
    vim.keymap.set("n", "G", function()
      local window = vim.api.nvim_get_current_win()
      M.follow_answers(window, vim.v.count == 0)
      if vim.v.count > 0 then
        vim.cmd("normal! " .. vim.v.count .. "G")
        remember_answer(window, answer_views[window])
      end
    end, { buffer = buffer, desc = "Follow latest TrailMix answer" })
  end
  vim.keymap.set("n", "<BS>", M.back, { buffer = buffer, desc = "Back to TrailMix answer" })
  vim.keymap.set("n", "q", function()
    local ok, err = pcall(vim.api.nvim_win_close, vim.api.nvim_get_current_win(), false)
    if not ok then vim.notify("TrailMix: " .. tostring(err), vim.log.levels.WARN) end
  end, { buffer = buffer, desc = "Close TrailMix panel" })
end

vim.api.nvim_create_autocmd("WinClosed", { callback = function(event)
  local window = tonumber(event.match)
  if vim.api.nvim_win_is_valid(window) then
    if answer_views[window] then answer_moved(window, answer_views[window]) end
    local buffer, tab = vim.api.nvim_win_get_buf(window), vim.api.nvim_win_get_tabpage(window)
    local kind, id = vim.b[buffer].trailmix_kind, vim.b[buffer].trailmix_meeting
    if kind and id then
      saved[tab] = saved[tab] or {}
      saved[tab][id] = saved[tab][id] or {}
      local position, keys = vim.api.nvim_win_call(window, vim.fn.winsaveview), row_keys[buffer] or {}
      saved[tab][id][kind] = {
        position = position, follow = vim.b[buffer].trailmix_follow, cursor_key = keys[position.lnum], top_key = keys[position.topline],
        answer_follow = answer_views[window] and answer_views[window].following,
      }
    end
  end
  history[window], restores[window], answer_views[window] = nil, nil, nil
end })
vim.api.nvim_create_autocmd("BufWipeout", { callback = function(event)
  row_keys[event.buf] = nil
  deferred_answers[event.buf] = nil
  for window, state in pairs(answer_views) do if state.buffer == event.buf then answer_views[window] = nil end end
end })
vim.api.nvim_create_autocmd({ "CursorMoved", "WinScrolled" }, { callback = function()
  for window, state in pairs(answer_views) do answer_moved(window, state) end
end })
vim.api.nvim_create_autocmd({ "ModeChanged", "CmdlineEnter" }, { callback = function()
  local window = vim.api.nvim_get_current_win()
  local state = answer_views[window]
  if not state then return end
  if reading_answer(state.buffer) then state.following = false end
end })
vim.api.nvim_create_autocmd({ "ModeChanged", "CmdlineLeave" }, { callback = function()
  if next(deferred_answers) == nil then return end
  vim.schedule(function()
    for buffer, text in pairs(deferred_answers) do
      if not reading_answer(buffer) then M.set_text(buffer, text) end
    end
  end)
end })
vim.api.nvim_create_autocmd("BufWinEnter", { callback = function(event) M.wrap_answers(event.buf) end })
return M
