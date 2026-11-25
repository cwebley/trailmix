-- Advertise only the RPC endpoint. The recorder reads buffers over Neovim RPC;
-- neither the registry nor its filenames contain note paths or note contents.
local M = {}
local uv = vim.uv or vim.loop
local registration, registered_socket, private_socket

local function home()
  local path = vim.env.TRAILMIX_HOME
  path = path and path ~= "" and path or vim.fn.expand("~/Library/Application Support/TrailMix")
  return vim.fn.fnamemodify(path, ":p"):gsub("/+$", "")
end

function M.stop()
  if registration then pcall(uv.fs_unlink, registration); registration, registered_socket = nil, nil end
  if private_socket then pcall(vim.fn.serverstop, private_socket); private_socket = nil end
end

function M.update(loaded)
  if not loaded then M.stop(); return true end
  local root = home()
  local directory = root .. "/recap-editors"
  local target = directory .. "/" .. vim.fn.getpid() .. ".json"
  local stat = registration and uv.fs_lstat(registration)
  local parent = registration and uv.fs_lstat(directory)
  if registration == target and stat and stat.type == "file" and stat.uid == uv.getuid()
    and stat.mode % 512 == 384 and parent and parent.type == "directory" and parent.uid == uv.getuid()
    and parent.mode % 512 == 448
    and registered_socket == (vim.v.servername:sub(1, 1) == "/" and vim.v.servername or private_socket)
    and vim.tbl_contains(vim.fn.serverlist(), registered_socket) then return true end
  M.stop()
  local function fail(reason)
    M.stop()
    return false, reason
  end
  local ok = pcall(vim.fn.mkdir, directory, "p", 448)
  if not ok then return fail("cannot create the editor registry directory") end
  stat = uv.fs_lstat(directory)
  if not stat or stat.type ~= "directory" or stat.uid ~= uv.getuid() then
    return fail("the editor registry must be a directory owned by this user")
  end
  if not uv.fs_chmod(directory, 448) then return fail("cannot make the editor registry owner-only") end
  local socket = vim.v.servername
  if socket:sub(1, 1) ~= "/" then
    local path = root .. "/e-" .. vim.fn.getpid() .. ".sock"
    local started, address = pcall(vim.fn.serverstart, path)
    if not started or type(address) ~= "string" or address:sub(1, 1) ~= "/" then
      return fail("cannot start a private Unix editor socket")
    end
    private_socket, socket = address, address
    -- The endpoint is private even when the state home already existed.
    if not uv.fs_chmod(socket, 384) then return fail("cannot make the private editor socket owner-only") end
  end
  local temporary = target .. "." .. tostring(uv.hrtime()) .. ".tmp"
  local fd = uv.fs_open(temporary, "wx", 384)
  if not fd then return fail("cannot create an owner-only editor registration file") end
  local data = vim.json.encode({ pid = vim.fn.getpid(), socket = socket }) .. "\n"
  local secured = uv.fs_fchmod(fd, 384)
  local wrote = secured and uv.fs_write(fd, data, 0)
  local synced = wrote == #data and uv.fs_fsync(fd)
  local closed = uv.fs_close(fd)
  if not secured or not synced or not closed then
    uv.fs_unlink(temporary)
    return fail("cannot write and sync the owner-only editor registration")
  end
  if not uv.fs_rename(temporary, target) then
    uv.fs_unlink(temporary)
    return fail("cannot atomically publish the editor registration")
  end
  registration, registered_socket = target, socket
  return true
end

return M
