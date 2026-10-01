local M = {}

-- Standalone anchors outside Markdown fences, in buffer order through last_row
-- (all lines when nil). Malformed anchors carry a problem instead of seconds.
local function scan(buffer, last_row)
  local found, fence_char, fence_length = {}, nil, nil
  for row, line in ipairs(vim.api.nvim_buf_get_lines(buffer, 0, last_row or -1, false)) do
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
          found[#found + 1] = { row = row, seconds = hours * 3600 + minutes * 60 + seconds }
        else
          found[#found + 1] = { row = row, problem = "invalid timestamp anchor: " .. anchor }
        end
      end
    end
  end
  return found
end

-- The anchor governing a row is the last one above it. An invalid anchor ends
-- the preceding section too; never silently jump using an older valid anchor
-- when the current section's timestamp is malformed.
function M.at_cursor(buffer, row)
  local found = scan(buffer, row)
  local last = found[#found]
  if not last then return 0 end
  if last.problem then return nil, last.problem end
  return last.seconds
end

-- Every valid anchor with its row, in buffer order. Malformed ones are skipped.
function M.list(buffer)
  return vim.tbl_filter(function(anchor) return anchor.seconds ~= nil end, scan(buffer))
end

function M.format(seconds)
  seconds = math.floor(seconds)
  if seconds >= 3600 then
    return string.format("@%02d:%02d:%02d", math.floor(seconds / 3600), math.floor(seconds / 60) % 60, seconds % 60)
  end
  return string.format("@%02d:%02d", math.floor(seconds / 60), seconds % 60)
end

return M
