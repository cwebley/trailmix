local M = {}
local BEGIN = "<!-- trailmix: recap begin -->"
local END = "<!-- trailmix: recap end -->"
local sections = { "Summary", "Observations", "Questions to follow up", "My commitments" }
local requests, discoveries, pending, applied, canceled = {}, {}, {}, {}, {}
local swap_buffers, swap_paths, cancel_notified = {}, {}, {}
local states = { none = true, queued = true, generating = true, ready = true, failed = true, cancelled = true }
local options = {}
local timer, epoch, refresh_queued = nil, 0, false
local uv = vim.uv or vim.loop
local editors = require("trailmix.recap_editors")
local registration_warnings = {}
-- Association checks are read-only. Keep verified identities after unload so
-- final editor exit can retry even if an earlier bounded worker timed out.
local title_meetings, title_notes, title_close_queued = {}, {}, {}
local title_sidecar_checks = {}
local title_exiting = false
local refresh

local function notify(message)
  vim.notify("TrailMix: " .. message, vim.log.levels.WARN)
end

local function registration_ready()
  local ready, reason = editors.update(true)
  if ready == true then return true end
  reason = type(reason) == "string" and reason or "cannot register this editor"
  if not registration_warnings[reason] then
    registration_warnings[reason] = true
    notify(reason .. "; recap insertion is paused until editor registration works")
  end
  return false, "editor registration is unavailable"
end

-- Only Markdown outside fenced code can own a note boundary or marker.
local function outside(lines, visit)
  local fence, width
  for row, line in ipairs(lines) do
    local run, rest = line:match("^ ? ? ?(`+)(.*)$")
    if not run then run, rest = line:match("^ ? ? ?(~+)(.*)$") end
    if fence then
      if run and run:sub(1, 1) == fence and not run:find("[^" .. fence .. "]")
        and #run >= width and rest:match("^%s*$") then fence = nil end
    elseif run and #run >= 3 and not run:find("[^" .. run:sub(1, 1) .. "]")
      and (run:sub(1, 1) ~= "`" or not rest:find("`", 1, true)) then
      fence, width = run:sub(1, 1), #run
    else
      visit(row, line)
    end
  end
  return fence == nil
end

local function heading(line)
  local hashes, title = line:match("^ ? ? ?(#+)%s+(.+)$")
  if hashes and #hashes <= 6 then return #hashes, vim.trim(title:gsub("%s+#+%s*$", "")) end
end

local function identity(lines)
  local found, invalid
  outside(lines, function(_, line)
    if line:find("trailmix: meeting", 1, true) then
      local id = line:match("^%s*<!%-%- trailmix: meeting ([%x]+) %-%->%s*$")
      if found or not id or #id ~= 32 then invalid = true end
      found = id or found
    end
  end)
  return not invalid and found or nil
end

local function note_id(buffer)
  if not vim.api.nvim_buf_is_loaded(buffer) or vim.bo[buffer].buftype ~= "" or vim.b[buffer].trailmix_kind then return end
  local path = vim.api.nvim_buf_get_name(buffer)
  if path:match("%.answers%.md$") or path:match("%.transcript%.md$") then return end
  return identity(vim.api.nvim_buf_get_lines(buffer, 0, -1, false))
end

local function sidecar_id(buffer)
  if not vim.api.nvim_buf_is_loaded(buffer) or vim.bo[buffer].buftype ~= "" then return end
  local kind = vim.api.nvim_buf_get_name(buffer):match("%.([^.]+)%.md$")
  if kind ~= "answers" and kind ~= "transcript" then return end
  local found, invalid
  outside(vim.api.nvim_buf_get_lines(buffer, 0, -1, false), function(_, line)
    if line:find("trailmix: " .. kind, 1, true) then
      local id = line:match("^%s*<!%-%- trailmix: " .. kind .. " ([%x]+) %-%->%s*$")
      if found or not id or #id ~= 32 then invalid = true end
      found = id or found
    end
  end)
  if not invalid then return found, kind end
end

local function text(lines, eol)
  return table.concat(lines, "\n") .. (eol and "\n" or "")
end

local function split(value)
  local eol = value:sub(-1) == "\n"
  if eol then value = value:sub(1, -2) end
  return vim.split(value, "\n", { plain = true }), eol
end

-- Return the exact rough lines, not a trimmed or reformatted interpretation.
local function layout(lines, eol)
  local rough, first, last, titles, marker, problem = nil, nil, nil, {}, nil, nil
  local result_id, result_row
  local headings = {}
  local closed = outside(lines, function(row, line)
    if line:find("trailmix: recap", 1, true) then
      local result = line:match("^<!%-%- trailmix: recap result (%x+) %-%->$")
      if line == BEGIN and not first then first = row
      elseif line == END and not last then last = row
      elseif result and #result == 32 and not result_id then result_id, result_row = result, row
      else problem = "duplicate or malformed recap markers" end
    end
    local id = line:match("^%s*<!%-%- trailmix: meeting ([%x]+) %-%->%s*$")
    if id then marker = row end
    local level, title = heading(line)
    if level then
      headings[#headings + 1] = { row = row, level = level, title = title }
      if title == "Rough notes" then
        if level ~= 2 or rough then problem = "the note needs exactly one ## Rough notes boundary" end
        rough = row
      end
      if level == 1 then titles[#titles + 1] = row end
    end
  end)
  if not closed then return nil, "the note has an unclosed code fence" end
  if problem then return nil, problem end
  if (first and not last) or (last and not first) or (first and (first >= last or not rough or last >= rough)) then
    return nil, "recap markers must form one complete region above ## Rough notes"
  end
  if result_row and (not first or not last or result_row <= first or result_row >= last) then
    return nil, "the recap result identity must be inside the generated region"
  end
  for _, h in ipairs(headings) do
    local owned = first and h.row > first and h.row < last
    for _, title in ipairs(sections) do
      if h.title == title and not owned and (not rough or h.row < rough) then
        return nil, "unowned recap headings found; keep your recap or add explicit recap region markers"
      end
    end
    if first and h.row > first and h.row < last and h.level == 1 then
      return nil, "the recap region contains the note title"
    end
  end
  if marker and first and marker >= first and marker <= last then return nil, "the recap region contains the meeting identity" end
  if rough then
    if marker and marker > rough then return nil, "the meeting identity must be above ## Rough notes" end
    return { rough = rough, first = first, last = last, result_id = result_id,
      rough_text = text(vim.list_slice(lines, rough + 1), eol) }
  end
  -- Legacy flat notes have only a title and identity header. Custom sectioned
  -- layouts need an explicit Rough notes boundary rather than a guessed move.
  if #titles > 1 or not marker then return nil, "ambiguous note header; add ## Rough notes explicitly" end
  local title = titles[1]
  for row = 1, math.max(title or 0, marker) do
    if row ~= marker and row ~= title and lines[row]:match("%S") then
      return nil, "ambiguous note header; add ## Rough notes explicitly"
    end
  end
  local rough_lines, header = {}, {}
  for row, line in ipairs(lines) do
    if row == title or row == marker then header[#header + 1] = line else rough_lines[#rough_lines + 1] = line end
  end
  return { header = header, title = title, marker = marker, rough_lines = rough_lines, rough_text = text(rough_lines, eol) }
end

local function decode(result, id, snapshot)
  if result.code ~= 0 then
    local message = vim.trim(result.stderr or "")
    return nil, message ~= "" and message or "recap command failed with code " .. tostring(result.code)
  end
  local value, problem = require("trailmix.cli").decode(result.stdout, "recap")
  if not value then return nil, problem end
  if value.meeting_id ~= id or type(value.recap_id) ~= "string" or #value.recap_id ~= 32
    or not value.recap_id:match("^%x+$") or type(value.markdown) ~= "string"
    or type(value.suggested_title) ~= "string" or type(value.notes_snapshot) ~= "string"
    or type(value.path) ~= "string" or value.path == "" or type(value.note_path) ~= "string"
    or value.note_path == "" or (snapshot and value.notes_snapshot ~= snapshot) then
    return nil, "invalid or mismatched recap response"
  end
  local lines = split(value.markdown)
  local count, invalid = 0, false
  local closed = outside(lines, function(_, line)
    local level, title = heading(line)
    if level then
      count = count + 1
      if level ~= 2 or title ~= sections[count] then invalid = true end
    elseif count == 0 and line:match("%S") then invalid = true end
    if line:find("<!-- trailmix:", 1, true) then invalid = true end
  end)
  if invalid or not closed or count ~= #sections then return nil, "recap must contain exactly the four expected sections" end
  value.lines = lines
  return value
end

local function apply(buffer, current, plan, value)
  local owned = { BEGIN, "<!-- trailmix: recap result " .. value.recap_id .. " -->" }
  vim.list_extend(owned, value.lines)
  owned[#owned + 1] = END
  local replacement, from, to, map
  if plan.rough then
    from, to = (plan.first or plan.rough) - 1, plan.last or (plan.rough - 1)
    replacement = owned
    if not plan.first then replacement[#replacement + 1] = "" end
    local delta = #replacement - (to - from)
    map = function(row)
      if row <= from then return row end
      if row > to then return row + delta end
      return from + math.min(row - from, #replacement)
    end
  else
    replacement = vim.deepcopy(plan.header)
    replacement[#replacement + 1] = ""
    vim.list_extend(replacement, owned)
    replacement[#replacement + 1] = ""
    replacement[#replacement + 1] = "## Rough notes"
    local rough_start = #replacement
    vim.list_extend(replacement, plan.rough_lines)
    local positions, header_row, rough_row = {}, 0, 0
    for row in ipairs(current) do
      if row == plan.title or row == plan.marker then
        header_row = header_row + 1
        positions[row] = header_row
      else
        rough_row = rough_row + 1
        positions[row] = rough_start + rough_row
      end
    end
    map = function(row) return positions[row] or row end
    from, to = 0, #current
    -- Preserve unchanged suffixes and their extmarks wherever possible.
    while to > from and #replacement > 0 and current[to] == replacement[#replacement] do
      to = to - 1
      table.remove(replacement)
    end
    while from < to and #replacement > 0 and current[from + 1] == replacement[1] do
      from = from + 1
      table.remove(replacement, 1)
    end
  end
  local views = {}
  for _, window in ipairs(vim.fn.win_findbuf(buffer)) do
    views[window] = vim.api.nvim_win_call(window, vim.fn.winsaveview)
  end
  vim.api.nvim_buf_call(buffer, function()
    -- Close any preceding insert/script undo block without changing its limit.
    vim.cmd("let &g:undolevels = &g:undolevels")
    vim.api.nvim_buf_set_lines(buffer, from, to, false, replacement)
    vim.cmd("let &g:undolevels = &g:undolevels")
  end)
  for window, view in pairs(views) do
    if vim.api.nvim_win_is_valid(window) and vim.api.nvim_win_get_buf(window) == buffer then
      view.lnum, view.topline = map(view.lnum), map(view.topline)
      vim.api.nvim_win_call(window, function() vim.fn.winrestview(view) end)
    end
  end
end

local function unique_note(id, buffer)
  local found
  for _, candidate in ipairs(vim.api.nvim_list_bufs()) do
    if note_id(candidate) == id then
      if found then return false end
      found = candidate
    end
  end
  return found == buffer
end

local function canonical(path)
  if type(path) ~= "string" or path == "" then return nil end
  return uv.fs_realpath(path)
end

local function token_for(buffer, id)
  local path = vim.api.nvim_buf_get_name(buffer)
  return { buffer = buffer, id = id, path = path, realpath = canonical(path), epoch = epoch }
end

local function valid(token)
  return not token.invalid and token.epoch == epoch and vim.api.nvim_buf_is_loaded(token.buffer)
    and note_id(token.buffer) == token.id and unique_note(token.id, token.buffer)
    and vim.api.nvim_buf_get_name(token.buffer) == token.path
    and token.realpath ~= nil and canonical(token.path) == token.realpath
end

local function associated(token, path)
  return token.realpath ~= nil and canonical(path) == token.realpath
end

local function title_note(buffer)
  if not vim.api.nvim_buf_is_loaded(buffer) then return end
  local path = vim.api.nvim_buf_get_name(buffer)
  local realpath = canonical(path)
  local id = note_id(buffer)
  local meeting = id and title_meetings[id]
  if not meeting or realpath ~= meeting.realpath then
    id, meeting = nil, nil
    -- Sidecars cannot establish an association. Only paths derived from an
    -- already verified main note can retry that meeting after their closure.
    for known_id, known in pairs(title_meetings) do
      for sidecar, cached_realpath in pairs(known.sidecars) do
        if path == sidecar or (realpath and (realpath == canonical(sidecar) or realpath == cached_realpath)) then
          id, meeting = known_id, known
          break
        end
      end
      if meeting then break end
    end
  end
  if not meeting then title_notes[buffer] = nil; return end
  local record = title_notes[buffer]
  if not record or record.id ~= id or record.path ~= path or record.realpath ~= realpath then
    record = { id = id, path = path, realpath = realpath, cli = meeting.cli }
    title_notes[buffer] = record
  end
  record.cli = meeting.cli
  return record
end

local function cache_title(id, path, realpath, cli, alias)
  local meeting = title_meetings[id]
  if not meeting or meeting.realpath ~= realpath then meeting = { sidecars = {} } end
  meeting.realpath, meeting.cli = realpath, cli or options.cli or "trailmix"
  for _, main in ipairs({ path, alias or path, realpath }) do
    local base = main:gsub("%.[^./]+$", "")
    for _, kind in ipairs({ "answers", "transcript" }) do
      local sidecar = base .. "." .. kind .. ".md"
      meeting.sidecars[sidecar] = canonical(sidecar) or false
    end
  end
  title_meetings[id] = meeting
end

local function remember_title(token, path)
  if not valid(token) or not associated(token, path) then return end
  cache_title(token.id, path, token.realpath, token.cli, token.path)
  title_note(token.buffer)
end

local function launch_title(id, cli)
  -- A vim.system handle is killed during exit cleanup. This job must outlive
  -- Neovim and wait for the real editor/tagged pane to close. The backend owns
  -- its bounded wait, private error log, intent and title eligibility checks.
  pcall(vim.fn.jobstart, { cli, "_title-worker", "--meeting", id }, {
    detach = true, stdout_buffered = true, stderr_buffered = true,
  })
end

local function queue_title_close(buffer)
  local record
  if vim.api.nvim_buf_is_loaded(buffer) then record = title_note(buffer)
  else record = title_notes[buffer] end
  if not record or record.sent or title_exiting then return end
  record.closing = true
  local id = record.id
  if title_close_queued[id] then return end
  title_close_queued[id] = true
  -- BufUnload fires before the buffer actually unloads. Delete and wipeout
  -- can follow in the same cycle; batch them into one retry per meeting.
  vim.schedule(function()
    title_close_queued[id] = nil
    if title_exiting then return end
    local meeting = title_meetings[id]
    for _, candidate in ipairs(vim.api.nvim_list_bufs()) do
      if note_id(candidate) == id and meeting
        and canonical(vim.api.nvim_buf_get_name(candidate)) == meeting.realpath then return end
    end
    local cli
    for candidate, closing in pairs(title_notes) do
      if closing.id == id and closing.closing and not closing.sent and not vim.api.nvim_buf_is_loaded(candidate) then
        closing.sent, cli = true, closing.cli
      end
    end
    if cli then launch_title(id, cli) end
  end)
end

local function cursor_in_region(buffer, plan)
  if not plan.first then return false end
  local tab = vim.api.nvim_get_current_tabpage()
  for _, window in ipairs(vim.fn.win_findbuf(buffer)) do
    if vim.api.nvim_win_get_tabpage(window) == tab then
      local row = vim.api.nvim_win_get_cursor(window)[1]
      if row >= plan.first and row <= plan.last then return true end
    end
  end
  return false
end

-- Pending results never own a buffer. Recheck its identity, path, layout and
-- every window each time insertion is attempted, including after async reads.
local function insert(token, value, explicit)
  local buffer = token.buffer
  if not valid(token) then return false, "the original note buffer was closed or replaced" end
  if not associated(token, value.note_path) then return false, "recap belongs to a different note path" end
  local registered, registration_error = registration_ready()
  if not registered then return false, registration_error end
  if not vim.bo[buffer].modifiable or vim.bo[buffer].readonly then return false, "the note is no longer writable" end
  local current = vim.api.nvim_buf_get_lines(buffer, 0, -1, false)
  local plan, problem = layout(current, vim.bo[buffer].endofline)
  if not plan then return false, problem end
  if token.owned and not plan.first then return false, "the original recap markers were removed" end
  local previous = applied[buffer]
  if not explicit and (plan.result_id == value.recap_id or (previous and previous.id == token.id
    and previous.path == token.path and previous.result_id == value.recap_id)) then return true end
  if not explicit then
    if swap_buffers[buffer] or swap_paths[token.path] then return false, "swap recovery must be resolved before :TrailMixRecapRestore" end
    if token.conflict then return false, token.reason or "another editor has this note open" end
    if cursor_in_region(buffer, plan) then return false, "a cursor is in the generated recap" end
  end
  apply(buffer, current, plan, value)
  applied[buffer] = { id = token.id, path = token.path, result_id = value.recap_id }
  local saved_lines, saved_eol = split(value.notes_snapshot)
  local saved = layout(saved_lines, saved_eol)
  local rough = token.rough or (saved and saved.rough_text)
  if rough ~= plan.rough_text then notify("rough notes changed since this recap's snapshot; regenerate when ready") end
  if vim.trim(value.suggested_title) ~= "" then
    notify("generated title available: " .. value.suggested_title
      .. "; eligible unnamed notes are named automatically after editor closure. Existing names are preserved")
  end
  return true
end

local function kill(token)
  token.invalid, token.checking = true, false
  if token.process then pcall(token.process.kill, token.process, 15) end
end

local function launch(token, command, config, callback)
  local ok, process = pcall(vim.system, command, config, function(result)
    vim.schedule(function()
      if token.invalid or token.epoch ~= epoch then return end
      callback(result)
    end)
  end)
  if ok then token.process = process end
  return ok
end

local function check_status(token, callback, allow_unregistered)
  if not allow_unregistered then
    local registered, problem = registration_ready()
    if not registered then callback(nil, problem); return end
  end
  local active = 0
  for _ in pairs(discoveries) do active = active + 1 end
  for _, request in pairs(requests) do if request.checking then active = active + 1 end end
  for _, check in pairs(title_sidecar_checks) do if check.checking then active = active + 1 end end
  if active >= 4 then callback(nil, "recap status reads are busy; retry shortly"); return end
  local command = { token.cli or options.cli or "trailmix", "recap", "--meeting", token.id, "--status" }
  token.checking = true
  local started = launch(token, command, { text = true, timeout = 8000 }, function(result)
    token.checking = false
    local state = result.code == 0 and require("trailmix.cli").decode(result.stdout, "recap status") or nil
    if not valid(token) or not state or state.meeting_id ~= token.id or not associated(token, state.note_path)
      or not states[state.state] or type(state.editor_conflict) ~= "boolean"
      or (state.editor_reason ~= nil and type(state.editor_reason) ~= "string") then
      callback(nil, "cannot verify the matching note's recap status")
    else
      remember_title(token, state.note_path)
      callback(state)
    end
  end)
  if not started then token.checking = false; callback(nil, "cannot launch " .. command[1]) end
end

local function submit(buffer, opts, restore)
  local id = note_id(buffer)
  if not id then notify("recap needs a meeting note with exactly one valid meeting ID"); return end
  if requests[id] then notify("a recap request for this meeting is already pending"); return end
  if not unique_note(id, buffer) then notify("more than one loaded note has this meeting ID"); return end
  if not vim.bo[buffer].modifiable or vim.bo[buffer].readonly then notify("this note is not writable"); return end
  local lines = vim.api.nvim_buf_get_lines(buffer, 0, -1, false)
  local plan, problem = layout(lines, vim.bo[buffer].endofline)
  if not plan then notify(problem); return end
  local snapshot = text(lines, vim.bo[buffer].endofline)
  local token = token_for(buffer, id)
  token.rough, token.owned, token.manual = not restore and plan.rough_text or nil, plan.first ~= nil, true
  token.cli = opts.cli
  requests[id], pending[buffer], canceled[buffer] = token, nil, nil
  if discoveries[buffer] then kill(discoveries[buffer]); discoveries[buffer] = nil end
  local command = { opts.cli or "trailmix", "recap", "--meeting", id, restore and "--latest" or "--stdin" }
  local config = { text = true }
  if restore then config.timeout = 4000
  else
    local input = { notes = snapshot, note_path = token.path, editor_pid = vim.fn.getpid() }
    local swaps, seen = {}, {}
    local function add_swap(path)
      if type(path) ~= "string" or path == "" then return end
      path = vim.fn.fnamemodify(path, ":p")
      if not seen[path] then swaps[#swaps + 1], seen[path] = path, true end
    end
    local found_swap, current_swap = pcall(vim.fn.swapname, buffer)
    if found_swap then add_swap(current_swap) end
    add_swap(vim.b[buffer].trailmix_recap_original_swap)
    if #swaps > 0 then input.editor_swap_paths = swaps end
    config.stdin = vim.json.encode(input)
  end
  local function receive(result)
    if requests[id] ~= token then return end
    local value, err = decode(result, id, not restore and snapshot or nil)
    if not value then requests[id] = nil; notify(err); return end
    remember_title(token, value.note_path)
    token.result_id = value.recap_id
    local function finish(state, problem)
      if requests[id] ~= token then return end
      requests[id] = nil
      token.conflict = not state or state.editor_conflict
      token.reason = problem or (state and state.editor_reason)
      local inserted, reason
      if token.conflict then reason = token.reason or "another editor has this note open"
      else inserted, reason = insert(token, value, restore) end
      if not inserted then
        if valid(token) and associated(token, value.note_path) then pending[buffer] = { token = token, value = value } end
        notify(reason .. "; recap saved privately. Use :TrailMixRecapRestore in the matching note")
      end
    end
    if restore then finish({ editor_conflict = false }) else check_status(token, finish) end
  end
  local function fetch()
    if not launch(token, command, config, receive) then
      requests[id] = nil; notify("cannot launch " .. command[1])
    end
  end
  if not restore then fetch(); return end
  -- Restore is the user's confirmation that recovery has been resolved. Clear
  -- only this instance's recovery flag before asking about other editors.
  swap_buffers[buffer], swap_paths[token.path] = nil, nil
  vim.b[buffer].trailmix_recap_swap_pending = nil
  check_status(token, function(state, problem)
    if requests[id] ~= token then return end
    if not state or state.editor_conflict then
      requests[id] = nil
      notify((problem or (state and type(state.editor_reason) == "string" and state.editor_reason)
        or "another editor has this note open") .. "; recap remains saved privately")
      return
    end
    fetch()
  end)
end

function M.generate(opts, buffer) submit(buffer or vim.api.nvim_get_current_buf(), opts or options, false) end
function M.restore(opts) submit(vim.api.nvim_get_current_buf(), opts or options, true) end
function M.cancel()
  local buffer = vim.api.nvim_get_current_buf()
  local id = note_id(buffer)
  local token = id and requests[id] or discoveries[buffer]
  local waiting = pending[buffer]
  if not id or not unique_note(id, buffer) then notify("recap cancellation needs one matching meeting note"); return end
  if token then
    kill(token)
    if token.result_id then
      applied[buffer] = { id = token.id, path = token.path, result_id = token.result_id }
    else
      -- A canceled status read has not told us which result was pending yet.
      -- Bind suppression to the first ready ID on the next read-only status.
      canceled[buffer] = { id = token.id, path = token.path }
    end
    if id then requests[id] = nil end
    discoveries[buffer] = nil
  end
  if waiting then
    -- Suppress rediscovery of the canceled result until Restore or a new ID.
    applied[buffer] = { id = id, path = waiting.token.path, result_id = waiting.value.recap_id }
    pending[buffer] = nil
  end
  local cancellation = token_for(buffer, id)
  if not cancellation.realpath then notify("cannot verify this meeting note's path"); return end
  requests[id] = cancellation
  check_status(cancellation, function(state, problem)
    if requests[id] ~= cancellation then return end
    if not state then requests[id] = nil; notify(problem); return end
    if state.state == "ready" and type(state.recap_id) == "string" then
      applied[buffer] = { id = id, path = cancellation.path, result_id = state.recap_id }
      canceled[buffer] = nil
    end
    if state.state ~= "queued" and state.state ~= "generating" then
      requests[id] = nil
      if state.state == "cancelled" then cancel_notified[buffer], canceled[buffer] = id, nil end
      notify("recap canceled; any result already saved can be loaded with :TrailMixRecapRestore")
      return
    end
    local command = { options.cli or "trailmix", "recap", "--meeting", id, "--cancel" }
    if not launch(cancellation, command, { text = true, timeout = 8000 }, function(result)
      if requests[id] ~= cancellation then return end
      requests[id] = nil
      if result.code ~= 0 then notify("could not cancel the background recap; retry :TrailMixRecapCancel"); return end
      local response = require("trailmix.cli").decode(result.stdout, "recap cancellation")
      if response and response.meeting_id == id and response.state == "cancelled" then
        cancel_notified[buffer], canceled[buffer] = id, nil
        notify("background recap was cancelled; any saved result can be loaded with :TrailMixRecapRestore")
      else notify("background recap cancellation requested; any saved result can be loaded with :TrailMixRecapRestore") end
    end) then requests[id] = nil; notify("cannot launch " .. command[1]) end
  end, true) -- Cancellation cannot mutate a note and does not require registration.
end

local function stop_timer()
  if timer then timer:stop(); timer:close(); timer = nil end
end

local function loaded_notes()
  local notes = {}
  for _, buffer in ipairs(vim.api.nvim_list_bufs()) do
    local id = note_id(buffer)
    if id then notes[buffer] = id end
  end
  return notes
end

local function discover(buffer, id)
  if not registration_ready() then return end
  local token = token_for(buffer, id)
  if not token.realpath or not unique_note(id, buffer) then return end
  discoveries[buffer] = token
  local command = { options.cli or "trailmix", "recap", "--meeting", id, "--status" }
  local ok = launch(token, command, { text = true, timeout = 8000 }, function(result)
    if discoveries[buffer] ~= token then return end
    discoveries[buffer] = nil
    if not valid(token) or result.code ~= 0 then return end
    local state = require("trailmix.cli").decode(result.stdout, "recap status")
    if not state or state.meeting_id ~= id or not states[state.state]
      or type(state.editor_conflict) ~= "boolean" or not associated(token, state.note_path)
      or (state.editor_reason ~= nil and type(state.editor_reason) ~= "string") then return end
    remember_title(token, state.note_path)
    if state.state == "cancelled" then
      if cancel_notified[buffer] ~= id then notify("background recap was cancelled; use :TrailMixRecap to generate a new recap") end
      cancel_notified[buffer], canceled[buffer] = id, nil
    else cancel_notified[buffer] = nil end
    token.conflict, token.reason = state.editor_conflict, state.editor_reason
    if state.state ~= "ready" or type(state.recap_id) ~= "string" or #state.recap_id ~= 32
      or not state.recap_id:match("^%x+$") then pending[buffer] = nil; return end
    local cancellation = canceled[buffer]
    if cancellation and cancellation.id == id and cancellation.path == token.path then
      applied[buffer] = { id = id, path = token.path, result_id = state.recap_id }
      canceled[buffer], pending[buffer] = nil, nil
      return
    end
    local previous = applied[buffer]
    if previous and previous.id == id and previous.path == token.path and previous.result_id == state.recap_id then
      pending[buffer] = nil; return
    end
    local current = vim.api.nvim_buf_get_lines(buffer, 0, -1, false)
    local plan = layout(current, vim.bo[buffer].endofline)
    if not plan then return end
    if plan.result_id == state.recap_id then pending[buffer] = nil; return end
    token.owned, token.result_id = plan.first ~= nil, state.recap_id
    local waiting = pending[buffer]
    if waiting and waiting.value.recap_id == state.recap_id and valid(waiting.token) then
      waiting.token.conflict, waiting.token.reason = token.conflict, token.reason
      if insert(waiting.token, waiting.value, false) then pending[buffer] = nil end
      return
    end
    if state.editor_conflict or swap_buffers[buffer] or swap_paths[token.path] then return end
    discoveries[buffer] = token
    local latest = { options.cli or "trailmix", "recap", "--meeting", id, "--latest" }
    local started = launch(token, latest, { text = true, timeout = 4000 }, function(response)
      if discoveries[buffer] ~= token then return end
      discoveries[buffer] = nil
      if not valid(token) then return end
      local value = decode(response, id)
      -- A newer result may have appeared between status and latest. Poll again
      -- rather than applying data that has not had its conflict check yet.
      if not value or value.recap_id ~= state.recap_id or not associated(token, value.note_path) then return end
      if not insert(token, value, false) then pending[buffer] = { token = token, value = value } end
    end)
    if not started then discoveries[buffer] = nil end
  end)
  if not ok then discoveries[buffer] = nil end
end

local function poll()
  local notes = loaded_notes()
  if not next(notes) or not registration_ready() then return end
  local active = 0
  for _, token in pairs(requests) do if token.checking then active = active + 1 end end
  for _, check in pairs(title_sidecar_checks) do if check.checking then active = active + 1 end end
  for buffer, token in pairs(discoveries) do
    if not notes[buffer] or not valid(token) then kill(token); discoveries[buffer] = nil
    else active = active + 1 end
  end
  -- Four bounded read-only CLI calls at a time, independent of recording status.
  -- Rotation prevents a large tab set from starving buffers later in the list.
  local order = {}
  for buffer in pairs(notes) do order[#order + 1] = buffer end
  table.sort(order, function(a, b) return (vim.b[a].trailmix_recap_poll or 0) < (vim.b[b].trailmix_recap_poll or 0) end)
  for _, buffer in ipairs(order) do
    if active >= 4 then break end
    local id = notes[buffer]
    if not discoveries[buffer] and not requests[id] and unique_note(id, buffer) then
      vim.b[buffer].trailmix_recap_poll = uv.hrtime()
      discover(buffer, id)
      if discoveries[buffer] then active = active + 1 end
    end
  end
end

local function valid_sidecar(token)
  local id, kind = sidecar_id(token.buffer)
  return not token.invalid and token.epoch == epoch and id == token.id and kind == token.kind
    and vim.api.nvim_buf_get_name(token.buffer) == token.path
    and token.realpath ~= nil and canonical(token.path) == token.realpath
end

local function discover_sidecars(sidecars)
  local active = 0
  for _ in pairs(discoveries) do active = active + 1 end
  for _, request in pairs(requests) do if request.checking then active = active + 1 end end
  for buffer, check in pairs(title_sidecar_checks) do
    if not valid_sidecar(check) then kill(check); title_sidecar_checks[buffer] = nil
    elseif check.checking then active = active + 1 end
  end
  for buffer, sidecar in pairs(sidecars) do
    if active >= 4 then break end
    if not title_note(buffer) and not title_sidecar_checks[buffer] then
      local token = token_for(buffer, sidecar.id)
      token.kind, token.checking, token.cli = sidecar.kind, true, options.cli or "trailmix"
      -- One bounded read per load/identity cycle. A marker registers this
      -- editor, but only the backend's canonical main path can verify ownership.
      title_sidecar_checks[buffer] = token
      if token.realpath then
        local command = { token.cli, "recap", "--meeting", token.id, "--status" }
        local started = launch(token, command, { text = true, timeout = 8000 }, function(result)
          token.checking = false
          local state = result.code == 0 and require("trailmix.cli").decode(result.stdout, "recap status") or nil
          if valid_sidecar(token) and state and state.meeting_id == token.id and states[state.state]
            and type(state.editor_conflict) == "boolean" and type(state.note_path) == "string"
            and (state.editor_reason == nil or type(state.editor_reason) == "string") then
            local main = canonical(state.note_path)
            local expected = state.note_path:gsub("%.[^./]+$", "") .. "." .. token.kind .. ".md"
            if main and canonical(expected) == token.realpath then
              cache_title(token.id, state.note_path, main, token.cli)
              title_note(buffer)
            end
          end
          refresh() -- Drain any other sidecar reads waiting for the shared limit.
        end)
        if started then active = active + 1 else token.checking = false end
      else token.checking = false end
    end
  end
end

refresh = function()
  local notes = loaded_notes()
  local title_loaded, sidecars = false, {}
  for _, buffer in ipairs(vim.api.nvim_list_bufs()) do
    if title_note(buffer) then title_loaded = true end
    local id, kind = sidecar_id(buffer)
    if id then sidecars[buffer] = { id = id, kind = kind } end
  end
  local registered
  -- Register potential owned sidecars even without a loaded main note. This
  -- permits backend inspection without trusting the marker's association.
  if next(notes) or title_loaded or next(sidecars) then registered = registration_ready() else editors.update(false) end
  if registered then discover_sidecars(sidecars) end
  for buffer, token in pairs(discoveries) do
    if not registered or options.recap_on_stop == false or not notes[buffer] or not valid(token) then
      kill(token); discoveries[buffer] = nil
    end
  end
  for buffer, waiting in pairs(pending) do
    if not notes[buffer] or not valid(waiting.token) then pending[buffer] = nil
    elseif registered and waiting.token.manual and not requests[waiting.token.id] and not discoveries[buffer]
      and vim.bo[buffer].modifiable and not vim.bo[buffer].readonly then
      local plan = layout(vim.api.nvim_buf_get_lines(buffer, 0, -1, false), vim.bo[buffer].endofline)
      if plan and not cursor_in_region(buffer, plan) then
        local token = waiting.token
        requests[token.id] = token
        check_status(token, function(state, problem)
          if requests[token.id] ~= token then return end
          requests[token.id] = nil
          token.conflict, token.reason = not state or state.editor_conflict, problem or (state and state.editor_reason)
          if state and not token.conflict and insert(token, waiting.value, false) then pending[buffer] = nil end
        end)
      end
    end
  end
  if options.recap_on_stop == false or not next(notes) then stop_timer(); return end
  if not timer then
    timer = uv.new_timer()
    local generation, source = epoch, timer
    timer:start(0, math.max(100, options.recap_refresh_ms or 1000), vim.schedule_wrap(function()
      if generation == epoch and timer == source then refresh(); poll() end
    end))
  end
end

function M.setup(opts)
  options = opts or {}
  title_exiting = false
  epoch = epoch + 1
  stop_timer()
  for _, token in pairs(discoveries) do kill(token) end
  for _, token in pairs(requests) do kill(token) end
  for _, token in pairs(title_sidecar_checks) do kill(token) end
  title_sidecar_checks = {}
  discoveries, requests, pending, refresh_queued = {}, {}, {}, false
  local group = vim.api.nvim_create_augroup("TrailMixRecap", { clear = true })
  local function queue_refresh()
    if refresh_queued then return end
    refresh_queued = true
    local generation = epoch
    vim.schedule(function()
      if generation ~= epoch then return end
      refresh_queued = false
      refresh()
    end)
  end
  vim.api.nvim_create_autocmd({ "BufReadPost", "BufNewFile", "BufEnter", "BufHidden", "BufFilePost",
    "CursorMoved", "CursorMovedI", "InsertLeave", "TextChanged", "TextChangedI", "TabEnter" }, {
    group = group, callback = queue_refresh,
  })
  vim.api.nvim_create_autocmd("SwapExists", { group = group, callback = function(event)
    local found_swap, original_swap = pcall(function() return vim.v.swapname end)
    if found_swap and type(original_swap) == "string" and original_swap ~= "" then
      pcall(function() vim.b[event.buf].trailmix_recap_original_swap = original_swap end)
    end
    local path = vim.fn.fnamemodify(event.file, ":p")
    swap_paths[path] = true
    if vim.api.nvim_buf_get_name(event.buf) == path then
      swap_buffers[event.buf] = true
      vim.b[event.buf].trailmix_recap_swap_pending = true
    end
    notify("swap recovery detected; automatic recap insertion is blocked for this note. Resolve recovery before using :TrailMixRecapRestore")
  end })
  vim.api.nvim_create_autocmd("BufReadPost", { group = group, callback = function(event)
    title_notes[event.buf] = nil -- A fresh load starts a new close/retry cycle.
    if title_sidecar_checks[event.buf] then kill(title_sidecar_checks[event.buf]); title_sidecar_checks[event.buf] = nil end
    title_note(event.buf)
    if swap_paths[vim.api.nvim_buf_get_name(event.buf)] then
      swap_buffers[event.buf] = true
      vim.b[event.buf].trailmix_recap_swap_pending = true
    end
    -- Only the untouched built-in template gets a starting cursor position.
    -- Setup and custom/previously edited notes never move the user's cursor.
    if vim.api.nvim_get_current_buf() ~= event.buf or vim.bo[event.buf].modified
      or not vim.bo[event.buf].endofline or not note_id(event.buf) then return end
    local content = vim.api.nvim_buf_get_lines(event.buf, 0, -1, false)
    if #content == 5 and content[1]:match("^<!%-%- trailmix: meeting %x+ %-%->$")
      and content[2]:match("^# Meeting %d%d%d%d%-%d%d%-%d%d %d%d:%d%d:%d%d$")
      and content[3] == "" and content[4] == "## Rough notes" and content[5] == ""
      and vim.deep_equal(vim.api.nvim_win_get_cursor(0), { 1, 0 }) then
      vim.api.nvim_win_set_cursor(0, { 5, 0 })
    end
  end })
  vim.api.nvim_create_autocmd({ "BufUnload", "BufDelete", "BufWipeout", "BufFilePre" }, {
    group = group, callback = function(event)
      if event.event == "BufFilePre" then title_notes[event.buf] = nil
      else queue_title_close(event.buf) end
      if title_sidecar_checks[event.buf] then kill(title_sidecar_checks[event.buf]); title_sidecar_checks[event.buf] = nil end
      for id, token in pairs(requests) do
        if token.buffer == event.buf then token.invalid = true; requests[id] = nil end
      end
      if discoveries[event.buf] then kill(discoveries[event.buf]); discoveries[event.buf] = nil end
      pending[event.buf] = nil
      if event.event == "BufWipeout" then
        applied[event.buf], canceled[event.buf], swap_buffers[event.buf], cancel_notified[event.buf] = nil, nil, nil, nil
      end
      queue_refresh()
    end,
  })
  vim.api.nvim_create_autocmd("VimLeavePre", { group = group, callback = function()
    title_exiting = true
    for id, meeting in pairs(title_meetings) do launch_title(id, meeting.cli) end
    epoch = epoch + 1
    stop_timer()
    for _, token in pairs(discoveries) do kill(token) end
    for _, token in pairs(requests) do kill(token) end
    for _, token in pairs(title_sidecar_checks) do kill(token) end
    title_sidecar_checks = {}
    discoveries, requests, pending = {}, {}, {}
    editors.stop()
  end })
  refresh()
end

return M
