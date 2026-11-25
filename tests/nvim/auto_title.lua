-- Closure fixtures only. No CLI, provider, recorder or detached process runs.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
local directory = vim.fn.tempname() .. " title fixtures ; $HOME"
vim.fn.mkdir(directory, "p")
local previous_home = vim.env.TRAILMIX_HOME
vim.env.TRAILMIX_HOME = directory .. "/private home"
local cli = directory .. "/bin/trailmix ; $(never-run)"
local calls, jobs, notices = {}, {}, {}
local original_system, original_jobstart, original_jobstop, original_swapname = vim.system, vim.fn.jobstart, vim.fn.jobstop, vim.fn.swapname
local registry_stopped = false
package.loaded["trailmix.recap_editors"] = {
  update = function(loaded) registry_stopped = not loaded; return true end,
  stop = function() registry_stopped = true end,
}
vim.notify = function(message) notices[#notices + 1] = message end
vim.system = function(argv, opts, callback)
  local call = { argv = argv, opts = opts, callback = callback }
  calls[#calls + 1] = call
  return { kill = function(_, signal) call.killed = signal end }
end
vim.fn.jobstart = function(argv, opts)
  jobs[#jobs + 1] = { argv = argv, opts = opts, registry_stopped = registry_stopped,
    home = vim.env.TRAILMIX_HOME }
  return #jobs
end
vim.fn.jobstop = function() error("exit cleanup stopped a detached title worker") end
local recap = require("trailmix.recap")
local function setup(auto)
  recap.setup({ cli = cli, recap_on_stop = auto == true, recap_refresh_ms = 100 })
end
local function drain() vim.wait(20, function() return false end, 1) end
local function meeting(n) return string.format("%032x", n) end
local function marker(id) return "<!-- trailmix: meeting " .. id .. " -->" end
local function note(id, name, kind)
  local path = directory .. "/" .. name
  vim.fn.writefile({ marker(id), "# Meeting 2026-09-24 09:30:00", "", "## Rough notes", "" }, path)
  local buffer = vim.api.nvim_create_buf(true, false)
  vim.api.nvim_buf_set_name(buffer, path)
  vim.api.nvim_buf_set_lines(buffer, 0, -1, false, vim.fn.readfile(path))
  vim.bo[buffer].bufhidden = "hide"
  if kind then vim.b[buffer].trailmix_kind = kind end
  vim.api.nvim_set_current_buf(buffer)
  return buffer, path
end
local function sidecar(path, kind)
  path = path:gsub("%.[^./]+$", "") .. "." .. kind .. ".md"
  vim.fn.writefile({ "<!-- trailmix: " .. kind .. " fixture -->", "Sidecar content" }, path)
  vim.cmd("edit " .. vim.fn.fnameescape(path))
  return vim.api.nvim_get_current_buf(), path
end
local function status(buffer, path, state)
  local id = vim.api.nvim_buf_get_lines(buffer, 0, 1, false)[1]:match("meeting (%x+)")
  setup(true)
  local found
  assert(vim.wait(1000, function()
    for index = #calls, 1, -1 do
      local call = calls[index]
      if call.argv[4] == id and call.argv[5] == "--status" and not call.finished and not call.killed then
        found = call; return true
      end
    end
    return false
  end, 5), "missing status for " .. id)
  found.finished = true
  found.callback({ code = 0, stdout = vim.json.encode({ schema_version = 1, meeting_id = id,
    note_path = path or vim.api.nvim_buf_get_name(buffer), state = state or "none", editor_conflict = false }) })
  drain()
  setup(false)
  return found
end
local function finish_sidecar_status(id, main_path)
  local found
  assert(vim.wait(1000, function()
    for index = #calls, 1, -1 do
      local call = calls[index]
      if call.argv[4] == id and call.argv[5] == "--status" and not call.finished and not call.killed then
        found = call; return true
      end
    end
    return false
  end, 5), "missing standalone sidecar status")
  found.finished = true
  assert(found.opts.timeout == 8000 and found.opts.stdin == nil, "sidecar check was not bounded/read-only")
  found.callback({ code = 0, stdout = vim.json.encode({ schema_version = 1, meeting_id = id,
    note_path = main_path, state = "none", editor_conflict = false }) })
  drain()
  return found
end
local function close(buffer)
  local before = #jobs
  vim.api.nvim_buf_delete(buffer, { force = true })
  assert(#jobs == before, "close launched synchronously before unload completed")
  drain()
end
local function count_jobs(id, first)
  local n = 0
  for index = first or 1, #jobs do if jobs[index].argv[4] == id then n = n + 1 end end
  return n
end
local function assert_job(job, id)
  assert(vim.deep_equal(job.argv, { cli, "_title-worker", "--meeting", id }), "worker did not use safe CLI argv")
  assert(job.opts.detach == true and job.opts.stdout_buffered == true and job.opts.stderr_buffered == true,
    "worker was not detached with buffered output")
  assert(job.opts.env == nil and job.opts.cwd == nil, "worker changed the recap environment")
  assert(job.home == vim.env.TRAILMIX_HOME, "worker used a different home than the registry")
end
local ok, err = xpcall(function()
  setup(false)
  local id = meeting(1)
  local buffer, path = note(id, "main note ; $(not-a-shell).md")
  assert(#jobs == 0 and #calls == 0, "opening an old note launched title work")
  status(buffer, path)
  assert(#jobs == 0, "status with no title intent launched title work")

  -- Hide is not closure. Even a synthetic early BufUnload cannot launch while
  -- this associated main note is still loaded.
  vim.api.nvim_set_current_buf(vim.api.nvim_create_buf(true, true))
  vim.api.nvim_exec_autocmds("BufHidden", { buffer = buffer })
  vim.api.nvim_exec_autocmds("BufUnload", { buffer = buffer })
  drain()
  assert(#jobs == 0, "a loaded note launched title work")
  vim.cmd("bunload! " .. buffer)
  assert(#jobs == 0, "BufUnload did not defer its loaded check")
  drain()
  assert(count_jobs(id) == 1)
  assert_job(jobs[#jobs], id)
  vim.api.nvim_buf_delete(buffer, { force = true })
  drain()
  assert(count_jobs(id) == 1, "unload/delete/wipeout repeated a close retry")

  -- Cached canonical association survives setup, unload and a fresh load.
  -- A reopen starts another cycle, even with automatic recap reads disabled.
  vim.cmd("edit " .. vim.fn.fnameescape(path))
  buffer = vim.api.nvim_get_current_buf()
  setup(false); setup(false)
  close(buffer)
  assert(count_jobs(id) == 2, "reopened note did not get its own close retry")

  -- Known IDs alone are not enough. A copied file has no verified association.
  buffer = note(id, "copy of main.md")
  close(buffer)
  assert(count_jobs(id) == 2, "closing a copy launched a title worker")
  local rejected = meeting(2)
  local foreign = directory .. "/actual-main.md"
  vim.fn.writefile({ marker(rejected), "## Rough notes" }, foreign)
  buffer = note(rejected, "unverified copy.md")
  status(buffer, foreign)
  local before = #jobs
  close(buffer)
  assert(#jobs == before, "mismatched status verified a copied note")

  -- Sidecars and managed panels never count as main notes, even with a marker.
  for index, name in ipairs({ "sidecar.answers.md", "sidecar.transcript.md", "managed.md", "unknown.md" }) do
    buffer = note(meeting(10 + index), name, name == "managed.md" and "answers" or nil)
    close(buffer)
    assert(#jobs == before, "unverified sidecar/panel/old note launched title work")
  end

  -- A bounded main-close worker can expire while sidecars remain loaded.
  -- Each corresponding sidecar close must retry without establishing any new
  -- meeting association or requiring the user to exit the whole editor.
  local with_sidecars = meeting(20)
  local main, main_path = note(with_sidecars, "main with sidecars.md")
  local answers, answers_path = sidecar(main_path, "answers")
  local transcript, transcript_path = sidecar(main_path, "transcript")
  status(main, main_path)
  close(main)
  assert(count_jobs(with_sidecars) == 1, "main close did not retry with sidecars loaded")
  assert(not registry_stopped, "loaded associated sidecars lost their editor registration")
  close(answers)
  assert(count_jobs(with_sidecars) == 2, "answers close did not retry after main unload")
  assert(not registry_stopped, "remaining transcript lost its editor registration")
  before = #jobs
  vim.cmd("bunload! " .. transcript)
  assert(#jobs == before, "sidecar unload launched before the buffer unloaded")
  drain()
  assert(count_jobs(with_sidecars) == 3, "last sidecar close did not retry")
  vim.api.nvim_buf_delete(transcript, { force = true })
  drain()
  assert(count_jobs(with_sidecars) == 3, "sidecar unload/delete/wipeout repeated a retry")
  assert(registry_stopped, "last sidecar closure left an editor registration")
  vim.cmd("edit " .. vim.fn.fnameescape(transcript_path))
  close(vim.api.nvim_get_current_buf())
  assert(count_jobs(with_sidecars) == 4, "reopened sidecar lost its retry cycle")
  local sidecar_alias = directory .. "/linked answers.md"
  assert((vim.uv or vim.loop).fs_symlink(answers_path, sidecar_alias))
  vim.cmd("edit " .. vim.fn.fnameescape(sidecar_alias))
  close(vim.api.nvim_get_current_buf())
  assert(count_jobs(with_sidecars) == 5, "canonical sidecar alias was not associated")
  buffer = note(with_sidecars, "copied sidecar.answers.md")
  close(buffer)
  assert(count_jobs(with_sidecars) == 5, "copied sidecar established an association")

  -- An ordinary editor can open only an actual exported sidecar. Its valid
  -- marker registers the endpoint before ownership is known; a read-only
  -- backend status must then prove the canonical main/sidecar relationship.
  local standalone = meeting(30)
  local standalone_main = directory .. "/standalone actual main.md"
  vim.fn.writefile({ marker(standalone), "## Rough notes" }, standalone_main)
  local standalone_buffer = sidecar(standalone_main, "answers")
  local standalone_lines = { "<!-- trailmix: answers " .. standalone .. " -->", "Saved answers" }
  vim.api.nvim_buf_set_lines(standalone_buffer, 0, -1, false, standalone_lines)
  setup(false)
  assert(not registry_stopped, "standalone potential sidecar did not register its editor")
  local standalone_check = finish_sidecar_status(standalone, standalone_main)
  assert(vim.deep_equal(vim.api.nvim_buf_get_lines(standalone_buffer, 0, -1, false), standalone_lines),
    "standalone ownership discovery edited its sidecar")
  local checked_calls = #calls
  vim.api.nvim_exec_autocmds("CursorMoved", { buffer = standalone_buffer })
  vim.api.nvim_exec_autocmds("TextChanged", { buffer = standalone_buffer })
  drain()
  assert(#calls == checked_calls, "standalone association kept polling")
  assert(not standalone_check.opts.stdin, "standalone sidecar discovery generated a recap")
  close(standalone_buffer)
  assert(count_jobs(standalone) == 1, "verified standalone sidecar close did not retry")

  local copied_standalone = meeting(31)
  local copied_main = directory .. "/never opened real main.md"
  vim.fn.writefile({ marker(copied_standalone), "## Rough notes" }, copied_main)
  local copied_buffer = sidecar(directory .. "/copied elsewhere.md", "transcript")
  vim.api.nvim_buf_set_lines(copied_buffer, 0, -1, false,
    { "<!-- trailmix: transcript " .. copied_standalone .. " -->", "Copied transcript" })
  setup(false)
  assert(not registry_stopped, "potential copied sidecar was invisible to backend inspection")
  finish_sidecar_status(copied_standalone, copied_main)
  checked_calls = #calls
  for _ = 1, 3 do vim.api.nvim_exec_autocmds("CursorMoved", { buffer = copied_buffer }); drain() end
  assert(#calls == checked_calls, "rejected sidecar ownership was repeatedly queried")
  close(copied_buffer)
  assert(count_jobs(copied_standalone) == 0, "artifact marker alone established a copied sidecar association")
  assert(registry_stopped, "closed standalone sidecar leaked its registry entry")

  -- A result can arrive later without another BufEnter or status read. The
  -- first read-only status cached the main-note association, not result state.
  local later = meeting(3)
  buffer, path = note(later, "late result.md")
  status(buffer, path, "queued")
  close(buffer)
  assert(count_jobs(later) == 1, "close depended on visiting a ready recap")

  -- Renaming or changing the in-buffer identity invalidates the close target.
  local renamed = meeting(4)
  buffer, path = note(renamed, "before rename.md")
  status(buffer, path)
  vim.api.nvim_buf_set_name(buffer, directory .. "/renamed-copy.md")
  before = #jobs
  close(buffer)
  assert(#jobs == before, "renamed copy kept the old close association")
  local changed = meeting(5)
  buffer, path = note(changed, "changed identity.md")
  status(buffer, path)
  vim.api.nvim_buf_set_lines(buffer, 0, 1, false, { marker(meeting(99)) })
  close(buffer)
  assert(#jobs == before, "changed identity kept the old close association")

  -- Canonical aliases are genuine associations, not byte-for-byte path tests.
  local aliased = meeting(6)
  buffer, path = note(aliased, "alias target.md")
  local alias = directory .. "/symlink.md"
  assert((vim.uv or vim.loop).fs_symlink(path, alias))
  status(buffer, alias)
  close(buffer)
  assert(count_jobs(aliased) == 1, "canonical alias was not verified")

  -- Remember recovery's original swap path. Restore clears only the pending
  -- recovery flag, since the original file may still exist after recovery.
  local exiting = meeting(7)
  buffer, path = note(exiting, "still open.md")
  status(buffer, path)
  local original_v = vim.v
  vim.v = setmetatable({ swapname = directory .. "/original-recovery.swp" }, { __index = original_v })
  vim.api.nvim_exec_autocmds("SwapExists", { buffer = buffer, pattern = nil })
  vim.v = original_v
  local original_swap = vim.b[buffer].trailmix_recap_original_swap
  assert(original_swap == directory .. "/original-recovery.swp", "SwapExists lost the original swap path")
  recap.restore({ cli = cli })
  assert(vim.b[buffer].trailmix_recap_swap_pending == nil, "Restore did not clear its pending recovery flag")
  assert(vim.b[buffer].trailmix_recap_original_swap == original_swap, "Restore discarded the recovery source")

  -- Generation persists known swap paths before any provider result exists.
  -- Exact duplicates are removed and a missing current swap is harmless.
  setup(false)
  vim.fn.swapname = function(actual)
    assert(actual == buffer, "swap lookup did not use the actual note buffer ID")
    return original_swap
  end
  recap.generate({ cli = cli }, buffer)
  assert(vim.deep_equal(vim.json.decode(calls[#calls].opts.stdin).editor_swap_paths, { original_swap }),
    "generation repeated a known swap path")
  setup(false)
  vim.fn.swapname = function() return "" end
  recap.generate({ cli = cli }, buffer)
  assert(vim.deep_equal(vim.json.decode(calls[#calls].opts.stdin).editor_swap_paths, { original_swap }),
    "generation lost the original path when no current swap existed")
  setup(false)
  vim.fn.swapname = function() return "./fixture-relative.swp" end
  recap.generate({ cli = cli }, buffer)
  assert(vim.deep_equal(vim.json.decode(calls[#calls].opts.stdin).editor_swap_paths,
    { vim.fn.fnamemodify("./fixture-relative.swp", ":p"), original_swap }), "generation sent a relative swap path")
  setup(false)
  vim.fn.swapname = original_swapname

  -- Leave both a manual request and a discovery in flight. Exit starts workers
  -- before removing the registry, but kills only the normal vim.system reads.
  local manual
  local discovering = meeting(8)
  local discovery_buffer = note(discovering, "unverified discovery.md")
  setup(true)
  local current_swap = directory .. "/current-note.swp"
  vim.fn.swapname = function(actual)
    assert(actual == buffer and actual ~= vim.api.nvim_get_current_buf(), "generation inspected the current buffer instead of its target")
    return current_swap
  end
  recap.generate({ cli = cli }, buffer)
  vim.fn.swapname = original_swapname
  manual = calls[#calls]
  assert(manual.opts.stdin, "fixture did not start a manual recap")
  local sent = vim.json.decode(manual.opts.stdin)
  assert(sent.swap_paths == nil, "generation used the wrong swap metadata field")
  assert(vim.deep_equal(sent.editor_swap_paths, { current_swap, original_swap }),
    "generation did not persist both current and recovery swap paths")
  local discovery
  assert(vim.wait(1000, function()
    for index = #calls, 1, -1 do
      local call = calls[index]
      if call.argv[4] == discovering and not call.finished and not call.killed then discovery = call; return true end
    end
    return false
  end, 5))
  assert(vim.api.nvim_buf_is_loaded(discovery_buffer))
  local exit_start = #jobs + 1
  registry_stopped = false
  vim.api.nvim_exec_autocmds("VimLeavePre", {})
  assert(registry_stopped, "exit did not stop editor registration")
  assert(manual.killed == 15 and discovery.killed == 15, "exit did not clean up normal requests")
  for _, known in ipairs({ id, later, renamed, changed, aliased, exiting, with_sidecars, standalone }) do
    assert(count_jobs(known, exit_start) == 1, "exit lost or repeated a previously verified meeting " .. known)
  end
  assert(count_jobs(rejected, exit_start) == 0 and count_jobs(discovering, exit_start) == 0
    and count_jobs(copied_standalone, exit_start) == 0,
    "exit launched workers for unverified identities")
  for index = exit_start, #jobs do
    assert_job(jobs[index], jobs[index].argv[4])
    assert(not jobs[index].registry_stopped, "worker launched after registry removal")
  end
  local after_exit = #jobs
  drain()
  assert(#jobs == after_exit, "exit allowed queued close retries to run again")
  for _, call in ipairs(calls) do
    assert(call.argv[2] == "recap", "title worker was launched with vim.system")
    assert(not vim.tbl_contains(call.argv, "--auto"), "fixture created an automatic recap job")
  end
end, debug.traceback)
setup(false)
vim.api.nvim_del_augroup_by_name("TrailMixRecap")
vim.system, vim.fn.jobstart, vim.fn.jobstop, vim.fn.swapname = original_system, original_jobstart, original_jobstop, original_swapname
vim.env.TRAILMIX_HOME = previous_home
vim.fn.delete(directory, "rf")
if not ok then error(err) end
print("TrailMix automatic title closure checks passed")
vim.cmd("qa!")
