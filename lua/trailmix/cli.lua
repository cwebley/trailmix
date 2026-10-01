-- The CLI's JSON output is versioned. This plugin reads exactly one version.
local M = {}

M.schema_version = 1

-- Decodes one CLI document. A version mismatch names its cause, since an
-- independently updated CLI or plugin otherwise looks like corrupt output.
function M.decode(stdout, kind)
  local ok, value = pcall(vim.json.decode, stdout or "")
  if not ok or type(value) ~= "table" then return nil, "invalid " .. kind .. " response" end
  if value.schema_version ~= M.schema_version then
    return nil, string.format("CLI output schema %s does not match this plugin's %d; update the trailmix CLI and plugin together",
      tostring(value.schema_version), M.schema_version)
  end
  return value
end

return M
