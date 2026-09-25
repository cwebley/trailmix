local M = {}

-- Only standalone anchors outside Markdown fences apply to following notes.
-- An invalid anchor ends the preceding section too; never silently jump using
-- an older valid anchor when the current section's timestamp is malformed.
function M.at_cursor(buffer, row)
  local selected, problem, fence_char, fence_length
  for _, line in ipairs(vim.api.nvim_buf_get_lines(buffer, 0, row, false)) do
    local indent, fence, rest = line:match("^( *)(`+)(.*)$")
    if not fence then indent, fence, rest = line:match("^( *)(~+)(.*)$") end
    local is_fence = fence and #indent <= 3 and #fence >= 3
    if fence_char then
      if is_fence and fence:sub(1, 1) == fence_char and #fence >= fence_length and rest:match("^%s*$") then
        fence_char, fence_length = nil, nil
      end
    elseif is_fence and (fence:sub(1, 1) == "~" or not rest:find("`", 1, true)) then
      fence_char, fence_length = fence:sub(1, 1), #fence
    else
      local spaces, anchor = line:match("^( *)(@%S+)%s*$")
      if anchor and #spaces <= 3 and (anchor:find(":", 1, true) or anchor:match("^@[%d+%-]")) then
        local hours, minutes, seconds = anchor:match("^@(%d%d+):(%d%d):(%d%d)$")
        if not hours then
          minutes, seconds = anchor:match("^@(%d%d):(%d%d)$")
          hours = "0"
        end
        hours, minutes, seconds = tonumber(hours), tonumber(minutes), tonumber(seconds)
        if hours and hours * 3600 < math.huge and minutes and minutes < 60 and seconds and seconds < 60 then
          selected, problem = hours * 3600 + minutes * 60 + seconds, nil
        else
          selected, problem = nil, "invalid timestamp anchor: " .. anchor
        end
      end
    end
  end
  if problem then return nil, problem end
  return selected or 0
end

return M
