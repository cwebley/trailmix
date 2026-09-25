local M = {}

function M.id(buffer)
  local found
  for _, line in ipairs(vim.api.nvim_buf_get_lines(buffer, 0, -1, false)) do
    for id in line:gmatch("<!%-%- trailmix: meeting ([%x]+) %-%->") do
      if found or #id ~= 32 then return nil end
      found = id
    end
  end
  return found
end

return M
