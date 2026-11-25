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

function M.is_note(buffer, id)
  local path = vim.api.nvim_buf_get_name(buffer)
  return not vim.b[buffer].trailmix_kind and not path:match("%.answers%.md$")
    and not path:match("%.transcript%.md$") and M.id(buffer) == id
end

-- Resolve without replacing a loaded buffer's unsaved contents. Saved exports
-- normally sit beside the note; live panels use the stable meeting association.
function M.resolve(id, transcript_buffer)
  local loaded, problem = M.find(id)
  if loaded or problem then return loaded, problem end
  local paths = {}
  local source = vim.api.nvim_buf_get_name(transcript_buffer)
  if source:match("%.transcript%.md$") then
    paths[#paths + 1] = source:gsub("%.transcript%.md$", ".md")
  end
  local home = vim.env.TRAILMIX_HOME
  if not home or home == "" then home = vim.fn.expand("~/Library/Application Support/TrailMix") end
  local ok, association = pcall(function()
    return vim.json.decode(table.concat(vim.fn.readfile(home .. "/meetings/" .. id .. ".json"), "\n"))
  end)
  if ok and type(association) == "table" and association.id == id
    and type(association.path) == "string" and association.path:sub(1, 1) == "/" then
    paths[#paths + 1] = association.path
  end
  for _, path in ipairs(paths) do
    if vim.fn.filereadable(path) == 1 then
      local buffer = vim.fn.bufadd(path)
      local read = pcall(vim.fn.bufload, buffer)
      if read and M.is_note(buffer, id) then return buffer end
    end
  end
  return nil, "cannot find a matching meeting note for this transcript"
end

-- Include loaded notes in other tabs or hidden buffers, without mistaking a
-- generated sidecar for the user's note. The CLI reads disk when none is loaded.
function M.find(id)
  local found
  for _, buffer in ipairs(vim.api.nvim_list_bufs()) do
    local path = vim.api.nvim_buf_get_name(buffer)
    if vim.api.nvim_buf_is_loaded(buffer) and vim.bo[buffer].buftype == ""
      and not vim.b[buffer].trailmix_kind and not path:match("%.answers%.md$")
      and not path:match("%.transcript%.md$") and M.id(buffer) == id then
      if found then return nil, "more than one loaded note has this meeting ID" end
      found = buffer
    end
  end
  return found
end

return M
