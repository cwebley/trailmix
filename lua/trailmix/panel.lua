-- Answers and evidence share one managed right-hand window in each tab.
local M = {}
local history = {}

local function managed(buffer)
  return vim.api.nvim_buf_is_valid(buffer) and vim.b[buffer].trailmix_kind ~= nil
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
  local target, fallback
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do
    local shown = vim.api.nvim_win_get_buf(window)
    if shown == buffer then target = window; break end
    -- The focused window holding an ordinary buffer is where the user is
    -- editing, whatever tag it kept. Answers must not replace that note.
    if vim.w[window].trailmix_panel and window == current and not managed(shown) then
      vim.w[window].trailmix_panel = nil
    end
    if vim.w[window].trailmix_panel then
      target = window
    elseif managed(shown) then
      fallback = fallback or window
    end
  end
  target = target or fallback
  if not target then
    target = vim.api.nvim_open_win(buffer, false, { split = "right", win = current })
  elseif vim.api.nvim_win_get_buf(target) ~= buffer then
    local previous = vim.api.nvim_win_get_buf(target)
    if not origin and vim.b[previous].trailmix_kind == "answers" then
      origin = { buffer = previous, position = vim.api.nvim_win_call(target, vim.fn.winsaveview) }
    end
    -- Keep unrelated unsaved buffers if the user used :edit or :enew in this
    -- slot. Reusing a panel must not abandon their text or create a third pane.
    vim.api.nvim_win_call(target, function() vim.cmd("hide buffer " .. buffer) end)
  end
  if origin then M.remember(target, origin) end
  vim.w[target].trailmix_panel = true
  M.wrap_answers(buffer)
  if focus then vim.api.nvim_set_current_win(target) end
  return target
end

function M.back()
  local window = vim.api.nvim_get_current_win()
  local stack = history[window] or {}
  while #stack > 0 do
    local prior = table.remove(stack)
    if vim.api.nvim_buf_is_valid(prior.buffer) then
      vim.api.nvim_win_set_buf(window, prior.buffer)
      M.wrap_answers(prior.buffer)
      vim.fn.winrestview(prior.position)
      return
    end
  end
  vim.notify("TrailMix: no answer view to return to", vim.log.levels.WARN)
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
  if first > last_old and first > last_new then return end
  vim.bo[buffer].readonly = false
  vim.bo[buffer].modifiable = true
  vim.api.nvim_buf_set_lines(buffer, first - 1, last_old, false, vim.list_slice(lines, first, last_new))
  vim.bo[buffer].modified = false
  vim.bo[buffer].modifiable = false
  vim.bo[buffer].readonly = true
end

function M.keys(buffer)
  vim.keymap.set("n", "<BS>", M.back, { buffer = buffer, desc = "Back to TrailMix answer" })
  vim.keymap.set("n", "q", function()
    local ok, err = pcall(vim.api.nvim_win_close, vim.api.nvim_get_current_win(), false)
    if not ok then vim.notify("TrailMix: " .. tostring(err), vim.log.levels.WARN) end
  end, { buffer = buffer, desc = "Close TrailMix panel" })
end

vim.api.nvim_create_autocmd("WinClosed", { callback = function(event) history[tonumber(event.match)] = nil end })
vim.api.nvim_create_autocmd("BufWinEnter", { callback = function(event) M.wrap_answers(event.buf) end })
return M
