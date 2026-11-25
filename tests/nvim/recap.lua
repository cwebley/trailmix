-- No recorder, provider, shell fixture, or network. Every process is controlled
-- here so edits and buffer lifecycle events can happen while a request is live.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
local id = "0123456789abcdef0123456789abcdef"
local other = "ffffffffffffffffffffffffffffffff"
local marker = "<!-- trailmix: meeting " .. id .. " -->"
local BEGIN, END = "<!-- trailmix: recap begin -->", "<!-- trailmix: recap end -->"
local directory = vim.fn.tempname()
vim.fn.mkdir(directory, "p")
local previous_home = vim.env.TRAILMIX_HOME
vim.env.TRAILMIX_HOME = directory .. "/home"
local notices, calls, serial = {}, {}, 0
vim.notify = function(message) notices[#notices + 1] = message end
local original_system, original_jobstart = vim.system, vim.fn.jobstart
-- Title retries use detached jobs, never a real fixture CLI process.
vim.fn.jobstart = function() return 1 end
vim.system = function(args, opts, callback)
  local note_path
  for _, buffer in ipairs(vim.api.nvim_list_bufs()) do
    if vim.api.nvim_buf_is_loaded(buffer) and not vim.b[buffer].trailmix_kind then
      for _, line in ipairs(vim.api.nvim_buf_get_lines(buffer, 0, -1, false)) do
        if vim.trim(line) == "<!-- trailmix: meeting " .. tostring(args[4]) .. " -->" then
          note_path = vim.api.nvim_buf_get_name(buffer)
        end
      end
    end
  end
  local call = { args = args, opts = opts, callback = callback, note_path = note_path }
  calls[#calls + 1] = call
  return { kill = function(_, signal) call.killed = signal end }
end
local plugin = require("trailmix")
local function setup(auto)
  plugin.setup({ cli = "fixture-trailmix", recap_on_stop = auto == true, recap_refresh_ms = 100,
    mapping = false, ask_mapping = false, transcript_mapping = false, transcript_jump_mapping = false,
    note_jump_mapping = false, note_jump_back_mapping = false })
end
setup(false)
local function lines(buffer) return vim.api.nvim_buf_get_lines(buffer or 0, 0, -1, false) end
local function text(buffer)
  buffer = buffer or vim.api.nvim_get_current_buf()
  return table.concat(lines(buffer), "\n") .. (vim.bo[buffer].endofline and "\n" or "")
end
local function drain() vim.wait(15, function() return false end, 1) end
local function count(kind)
  local n = 0
  for _, call in ipairs(calls) do if call.args[2] == kind then n = n + 1 end end
  return n
end
local function latest(kind)
  for index = #calls, 1, -1 do if calls[index].args[2] == kind then return calls[index] end end
end
local finish
finish = function(call, value, code, err)
  assert(call and not call.finished, "missing or already finished process")
  call.finished = true
  call.callback({ code = code or 0, stdout = type(value) == "string" and value or vim.json.encode(value), stderr = err or "" })
  drain()
  -- Existing manual fixtures have no foreign editor. Each generated result now
  -- gets a separate conflict check before it may touch the live buffer.
  if call.opts.stdin and not call.defer_status then
    local check = latest("recap")
    if check ~= call and vim.tbl_contains(check.args, "--status") and not check.finished and not check.killed then
      finish(check, { schema_version = 1, meeting_id = check.args[4], note_path = check.note_path,
        state = "ready", recap_id = type(value) == "table" and value.recap_id or string.rep("a", 32), editor_conflict = false })
    end
  end
end
local function markdown(label)
  return "## Summary\n\n" .. (label or "A recap") .. " [system 00:12](trailmix://meetings/" .. id .. "/passages/system-1)\n\n## Observations\n\n- A detail\n\n## Questions to follow up\n\n- A question\n\n## My commitments\n\n- A commitment\n"
end
local function response(call, label)
  local sent = call.opts.stdin and vim.json.decode(call.opts.stdin)
  call.result_id = call.result_id or (call.opts.stdin and string.format("%032x", #calls) or string.rep("a", 32))
  return { schema_version = 1, meeting_id = id, recap_id = call.result_id, markdown = markdown(label),
    suggested_title = "A suggested title", notes_snapshot = sent and sent.notes or marker .. "\n",
    path = directory .. "/private-recap.json", note_path = call.note_path }
end
local function request(command)
  local before = count("recap")
  vim.cmd(command or "TrailMixRecap")
  assert(count("recap") == before + 1, notices[#notices])
  if command == "TrailMixRecapRestore" then
    local check = latest("recap")
    assert(vim.tbl_contains(check.args, "--status"), "Restore did not verify editor conflicts")
    finish(check, { schema_version = 1, meeting_id = id, note_path = check.note_path, state = "ready",
      recap_id = string.rep("a", 32), editor_conflict = false })
    assert(count("recap") == before + 2, notices[#notices])
  end
  return latest("recap")
end
local function fresh(content, eol)
  setup(false)
  for _, buffer in ipairs(vim.api.nvim_list_bufs()) do
    if vim.api.nvim_buf_is_valid(buffer) and vim.api.nvim_buf_get_name(buffer):find(directory, 1, true) then
      vim.api.nvim_buf_delete(buffer, { force = true })
    end
  end
  serial = serial + 1
  local buffer = vim.api.nvim_create_buf(true, false)
  vim.api.nvim_buf_set_name(buffer, directory .. "/note-" .. serial .. ".md")
  vim.api.nvim_set_current_buf(buffer)
  vim.api.nvim_buf_set_lines(buffer, 0, -1, false, content or { "# Meeting", marker, "", "## Rough notes", "", "- rough" })
  vim.bo[buffer].endofline = eol ~= false
  vim.bo[buffer].bufhidden = "hide"
  vim.bo[buffer].filetype = "markdown"
  vim.fn.writefile(lines(buffer), vim.api.nvim_buf_get_name(buffer))
  drain()
  return buffer
end
local function row(buffer, needle)
  for index, line in ipairs(lines(buffer)) do if line == needle then return index end end
  error("line missing: " .. needle)
end
local function occurrences(value, needle)
  local n, start = 0, 1
  while true do
    local at = value:find(needle, start, true)
    if not at then return n end
    n, start = n + 1, at + #needle
  end
end
local function refused(content, message)
  local buffer = fresh(content)
  local before, original = count("recap"), text(buffer)
  vim.cmd("TrailMixRecap")
  assert(count("recap") == before and text(buffer) == original, "unsafe layout was submitted")
  if message then assert(notices[#notices]:find(message, 1, true), notices[#notices]) end
end
-- Run a completion while Neovim is genuinely processing Insert/Replace mode,
-- then type more text before Escape. Source-only startinsert does not test this.
local function while_typing(keys, callback)
  local problem
  _G.__trailmix_recap_typing = function()
    local okay, failure = xpcall(callback, debug.traceback)
    if not okay then problem = failure end
  end
  local input = keys .. "<Cmd>lua _G.__trailmix_recap_typing()<CR> typed<Esc>"
  vim.api.nvim_feedkeys(vim.api.nvim_replace_termcodes(input, true, false, true), "xt", false)
  _G.__trailmix_recap_typing = nil
  if problem then error(problem) end
end
local function has_flag(call, flag) return vim.tbl_contains(call.args, flag) end
local function wait_flag(flag, meeting)
  local found
  assert(vim.wait(1200, function()
    for index = #calls, 1, -1 do
      local call = calls[index]
      if has_flag(call, flag) and not call.finished and not call.killed and (not meeting or call.args[4] == meeting) then
        found = call; return true
      end
    end
    return false
  end, 5), flag .. " call did not start")
  return found
end
local function status(state, result_id, extras, meeting)
  local call = wait_flag("--status", meeting)
  local value = { schema_version = 1, meeting_id = meeting or id, note_path = call.note_path,
    state = state, recap_id = result_id or string.rep("a", 32), editor_conflict = false }
  for key, item in pairs(extras or {}) do value[key] = item end
  finish(call, value)
  return call
end
local ok, err = xpcall(function()
  -- Opening the exact pristine built-in template starts in its rough area.
  -- Re-running setup or opening a custom note must not reposition the cursor.
  local pristine = directory .. "/pristine.md"
  vim.fn.writefile({ marker, "# Meeting 2026-09-24 09:30:00", "", "## Rough notes", "" }, pristine)
  vim.cmd("edit " .. vim.fn.fnameescape(pristine))
  assert(vim.api.nvim_win_get_cursor(0)[1] == 5, "new default note did not start in rough area")
  vim.api.nvim_win_set_cursor(0, { 1, 0 })
  setup(false)
  assert(vim.api.nvim_win_get_cursor(0)[1] == 1, "setup moved an existing note's cursor")
  local custom = directory .. "/custom.md"
  vim.fn.writefile({ marker, "# Custom title", "", "## Rough notes", "" }, custom)
  vim.cmd("edit " .. vim.fn.fnameescape(custom))
  assert(vim.api.nvim_win_get_cursor(0)[1] == 1, "custom template cursor moved")

  -- Flat notes migrate without trimming a single body line or final newline.
  local original = { "# Old meeting", "", marker, "", "- original rough", "", "### Detail", "~~~markdown",
    "## Rough notes", BEGIN, END, "~~~", "last line" }
  local buffer = fresh(original, false)
  local path = vim.api.nvim_buf_get_name(buffer)
  vim.fn.writefile({ "disk must stay untouched" }, path)
  vim.api.nvim_win_set_cursor(0, { 5, 4 })
  local window = vim.api.nvim_get_current_win()
  local call = request()
  assert(vim.deep_equal(call.args, { "fixture-trailmix", "recap", "--meeting", id, "--stdin" }))
  local sent = vim.json.decode(call.opts.stdin)
  assert(sent.notes == table.concat(original, "\n") and sent.note_path == path)
  assert(sent.editor_pid == vim.fn.getpid(), "generation did not identify its editor process")
  assert(vim.tbl_count(sent) == 3, "stdin leaked extra fields")
  vim.api.nvim_buf_set_lines(buffer, 4, 5, false, { "- edited rough during generation" })
  vim.api.nvim_buf_set_lines(buffer, -1, -1, false, { "appended without newline" })
  local value = response(call)
  finish(call, value)
  assert(vim.api.nvim_get_current_win() == window and vim.api.nvim_get_current_buf() == buffer, "recap stole focus")
  assert(vim.api.nvim_get_current_line() == "- edited rough during generation" and vim.api.nvim_win_get_cursor(0)[2] == 4, "migration lost cursor")
  local expected_rough = { "", "", "- edited rough during generation", "", "### Detail", "~~~markdown", "## Rough notes", BEGIN, END, "~~~", "last line", "appended without newline" }
  local boundary = row(buffer, "## Rough notes")
  assert(vim.deep_equal(vim.list_slice(lines(buffer), boundary + 1), expected_rough), "migration changed rough lines")
  assert(not vim.bo[buffer].endofline and vim.bo[buffer].modified, "recap changed final newline or saved note")
  assert(vim.fn.readfile(path)[1] == "disk must stay untouched", "recap wrote disk")
  assert(text(buffer):sub(1, #"# Old meeting") == "# Old meeting", "suggestion renamed the note")
  assert(table.concat(notices, "\n"):find("rough notes changed", 1, true), "missing stale snapshot warning")
  assert(notices[#notices]:find("generated title available", 1, true))
  assert(notices[#notices]:find("after editor closure", 1, true))
  local generated = text(buffer)
  vim.cmd("undo")
  local prior = vim.deepcopy(original)
  prior[5] = "- edited rough during generation"
  prior[#prior + 1] = "appended without newline"
  assert(text(buffer) == table.concat(prior, "\n"), "undo lost rough edits made while the request was pending")
  vim.cmd("redo")
  assert(text(buffer) == generated, "redo failed to restore the generated region")

  -- Regeneration replaces only the explicit region. Prefix and rough edits stay.
  vim.api.nvim_buf_set_lines(buffer, 2, 2, false, { "Personal preamble above the recap", "" })
  vim.api.nvim_buf_set_lines(buffer, row(buffer, BEGIN), row(buffer, BEGIN) + 1, false, { "## Summary", "manual generated-region edit" })
  local before = lines(buffer)
  local first, last = row(buffer, BEGIN), row(buffer, END)
  local prefix, suffix = vim.list_slice(before, 1, first - 1), vim.list_slice(before, last + 1)
  vim.api.nvim_win_set_cursor(0, { row(buffer, "appended without newline"), 6 })
  call = request()
  local notice_count = #notices
  vim.api.nvim_buf_set_lines(buffer, first + 1, first + 2, false, { "another in-flight owned-region edit" })
  finish(call, response(call, "Regenerated"))
  for index = notice_count + 1, #notices do
    assert(not notices[index]:find("rough notes changed", 1, true), "generated-region edits caused a false stale warning")
  end
  assert(vim.deep_equal(vim.list_slice(lines(buffer), 1, first - 1), prefix))
  assert(vim.deep_equal(vim.list_slice(lines(buffer), row(buffer, END) + 1), suffix))
  assert(vim.api.nvim_get_current_line() == "appended without newline" and vim.api.nvim_win_get_cursor(0)[2] == 6)
  assert(occurrences(text(buffer), "## Summary") == 1 and not text(buffer):find("manual generated-region edit", 1, true))

  -- Existing Rough notes preserve a non-recap preamble byte for byte.
  buffer = fresh({ "# Meeting", marker, "", "My preamble", "## Agenda", "- reviewed", "", "## Rough notes", "raw", "" })
  before = lines(buffer)
  call = request()
  finish(call, response(call))
  assert(vim.deep_equal(vim.list_slice(lines(buffer), 1, 7), vim.list_slice(before, 1, 7)))
  assert(vim.deep_equal(vim.list_slice(lines(buffer), row(buffer, "## Rough notes") + 1), { "raw", "" }))

  -- Recap-like headings below the explicit boundary are ordinary rough notes.
  local structured_rough = { "### Summary", "my own rough summary", "## Observations", "a rough observation", "" }
  buffer = fresh(vim.list_extend({ "# Meeting", marker, "", "## Rough notes" }, structured_rough))
  call = request()
  finish(call, response(call))
  assert(vim.deep_equal(vim.list_slice(lines(buffer), row(buffer, "## Rough notes") + 1), structured_rough))

  -- A tilde fence may have a backtick in its info string. Both the backend
  -- and this scanner must see the closing tilde run as a close, not an opener.
  buffer = fresh()
  call = request()
  finish(call, response(call, "~~~`\ncode\n~~~\n"))
  assert(text(buffer):find("~~~`\ncode\n~~~", 1, true), "mixed-character fence info rejected")
  call = request()
  finish(call, response(call, "Regenerated after fenced content"))
  assert(text(buffer):find("Regenerated after fenced content", 1, true))

  -- Views in all windows retain the same rough text, including a scrolled view.
  local many = { "# Meeting", marker, "", "## Rough notes" }
  for index = 1, 100 do many[#many + 1] = "rough line " .. index end
  buffer = fresh(many)
  vim.api.nvim_win_set_cursor(0, { 50, 4 })
  vim.cmd("normal! zt")
  local view = vim.fn.winsaveview()
  local top = lines(buffer)[view.topline]
  local focus = vim.api.nvim_get_current_win()
  vim.cmd("vsplit")
  local second = vim.api.nvim_get_current_win()
  vim.api.nvim_win_set_cursor(second, { 80, 2 })
  vim.api.nvim_set_current_win(focus)
  call = request()
  local before_apply = text(buffer)
  finish(call, response(call))
  assert(vim.api.nvim_get_current_win() == focus and vim.api.nvim_get_current_line() == "rough line 46")
  assert(lines(buffer)[vim.fn.winsaveview().topline] == top, "scroll position moved")
  assert(lines(buffer)[vim.api.nvim_win_get_cursor(second)[1]] == "rough line 76", "other window cursor moved")
  vim.cmd("undo")
  assert(text(buffer) == before_apply, "recap is not one undoable edit")
  vim.api.nvim_win_close(second, true)

  -- Ambiguities are rejected before a provider request can start.
  refused({ "---", "title: frontmatter", "---", "# Meeting", marker, "body" }, "ambiguous")
  refused({ "body before header", "# Meeting", marker }, "ambiguous")
  refused({ "# Meeting", "body before identity", marker }, "ambiguous")
  refused({ "# Meeting", marker, "# Another title", "body" }, "ambiguous")
  refused({ "# Meeting", marker, "## Summary", "today's carefully organized recap", "## Rough notes", "raw" }, "unowned")
  refused({ "# Meeting", marker, "## Summary", "manual recap without rough" }, "unowned")
  refused({ "# Meeting", marker, "## Rough notes", "raw", "## Rough notes" }, "exactly one")
  refused({ "# Meeting", marker, BEGIN, "## Summary", "## Rough notes" }, "complete region")
  refused({ "# Meeting", marker, END, BEGIN, "## Rough notes" }, "complete region")
  refused({ "# Meeting", marker, BEGIN, BEGIN, END, "## Rough notes" }, "malformed")
  refused({ "# Meeting", marker, "<!-- trailmix: recap start -->", "## Rough notes" }, "malformed")
  refused({ "# Meeting", marker, "## Rough notes", "```", "unclosed" }, "unclosed")
  -- Fenced headings and malformed marker examples do not own any boundaries.
  buffer = fresh({ "# Meeting", marker, "", "```md", "## Summary", "## Rough notes", "<!-- trailmix: recap broken -->", "```", "## Rough notes", "raw" })
  call = request()
  finish(call, response(call))
  assert(text(buffer):find("<!-- trailmix: recap broken -->", 1, true))

  -- Duplicate associations are rejected both before and after submission.
  buffer = fresh()
  local duplicate = vim.api.nvim_create_buf(true, false)
  vim.api.nvim_buf_set_name(duplicate, directory .. "/duplicate.md")
  vim.api.nvim_buf_set_lines(duplicate, 0, -1, false, { "# Duplicate", marker })
  before = count("recap")
  vim.cmd("TrailMixRecap")
  assert(count("recap") == before and notices[#notices]:find("more than one", 1, true))
  vim.api.nvim_buf_delete(duplicate, { force = true })
  call = request()
  duplicate = vim.api.nvim_create_buf(true, false)
  vim.api.nvim_buf_set_name(duplicate, directory .. "/duplicate-late.md")
  vim.api.nvim_buf_set_lines(duplicate, 0, -1, false, { marker })
  before = text(buffer)
  finish(call, response(call))
  assert(text(buffer) == before and notices[#notices]:find("RecapRestore", 1, true))

  -- A saved result can be restored only by an explicit latest request. No stdin.
  buffer = fresh()
  call = request()
  value = response(call)
  before = text(buffer)
  vim.api.nvim_buf_delete(buffer, { force = true })
  local replacement = fresh()
  finish(call, value)
  assert(text(replacement) == before, "late result applied to recreated note")
  vim.api.nvim_buf_set_lines(replacement, -1, -1, false, { "new rough after close" })
  call = request("TrailMixRecapRestore")
  assert(has_flag(call, "--latest") and not has_flag(call, "--stdin") and call.opts.stdin == nil)
  value.note_path = vim.api.nvim_buf_get_name(replacement)
  finish(call, value)
  assert(text(replacement):find("new rough after close", 1, true) and text(replacement):find(BEGIN, 1, true))
  assert(notices[#notices - 1]:find("rough notes changed", 1, true), "restore did not warn about stale rough text")

  -- Closing a visible note, renaming it, changing identity, or deleting markers
  -- keeps the result private instead of editing a different target.
  for _, change in ipairs({ "rename", "identity", "readonly", "unload", "markers" }) do
    buffer = fresh()
    if change == "markers" then
      call = request(); finish(call, response(call))
    end
    call = request()
    before = text(buffer)
    if change == "hide" then vim.api.nvim_set_current_buf(vim.api.nvim_create_buf(true, true))
    elseif change == "rename" then vim.api.nvim_buf_set_name(buffer, directory .. "/renamed.md")
    elseif change == "identity" then vim.api.nvim_buf_set_lines(buffer, 1, 2, false, { "<!-- trailmix: meeting " .. other .. " -->" })
    elseif change == "readonly" then vim.bo[buffer].modifiable = false
    elseif change == "unload" then vim.cmd("bunload! " .. buffer)
    elseif change == "markers" then vim.api.nvim_buf_set_lines(buffer, row(buffer, BEGIN) - 1, row(buffer, END), false, {}) end
    local changed = vim.api.nvim_buf_is_loaded(buffer) and text(buffer)
    finish(call, response(call))
    if changed then assert(text(buffer) == changed, change .. " changed the target") end
  end

  -- Invalid schemas, wrong IDs, missing sections and CLI errors cannot edit notes.
  for _, bad in ipairs({ "schema", "meeting", "recap", "sections", "snapshot", "json", "exit" }) do
    buffer = fresh()
    call = request()
    value, before = response(call), text(buffer)
    if bad == "schema" then value.schema_version = 2
    elseif bad == "meeting" then value.meeting_id = other
    elseif bad == "recap" then value.recap_id = "not-an-id"
    elseif bad == "sections" then value.markdown = "## Summary\nMissing sections"
    elseif bad == "snapshot" then value.notes_snapshot = "wrong snapshot"
    elseif bad == "json" then value = "{broken"
    end
    finish(call, value, bad == "exit" and 1 or 0, "fixture provider failed")
    assert(text(buffer) == before, bad .. " response edited note")
  end
  buffer = fresh()
  call = request()
  before = count("recap")
  vim.cmd("TrailMixRecap")
  assert(count("recap") == before and notices[#notices]:find("already pending", 1, true))
  before = text(buffer)
  vim.cmd("TrailMixRecapCancel")
  assert(call.killed == 15)
  finish(call, response(call))
  assert(text(buffer) == before, "cancel still applied its result")
  local fake = vim.system
  vim.system = function() error("cannot spawn fixture") end
  vim.cmd("TrailMixRecap")
  assert(notices[#notices]:find("cannot launch", 1, true) and text(buffer) == before)
  vim.system = fake
  call = request(); finish(call, response(call))

  -- Discovery on opening a historical note is read-only. There is no global
  -- recording status dependency and no automatic provider request.
  buffer = fresh()
  local automatic_start = #calls
  setup(true)
  status("none")
  status("queued")
  status("generating")
  vim.api.nvim_buf_set_lines(buffer, -1, -1, false, { "unsaved rough while backend works" })
  vim.api.nvim_win_set_cursor(0, { #lines(buffer), 5 })
  status("ready")
  call = wait_flag("--latest")
  assert(call.opts.stdin == nil and call.opts.timeout == 4000)
  while_typing("A", function()
    assert(vim.api.nvim_get_mode().mode == "i", "fixture did not enter Insert mode")
    local cursor = vim.api.nvim_win_get_cursor(0)
    finish(call, response(call, "Discovered recap"))
    assert(vim.api.nvim_get_mode().mode == "i", "insertion interrupted typing")
    assert(vim.api.nvim_get_current_line() == "unsaved rough while backend works" and vim.api.nvim_win_get_cursor(0)[2] == cursor[2], "insertion moved the typing cursor")
  end)
  assert(text(buffer):find("Discovered recap", 1, true))
  assert(vim.api.nvim_get_current_line() == "unsaved rough while backend works typed", "typing after insertion went into another line")
  assert(text(buffer):find("<!-- trailmix: recap result " .. string.rep("a", 32) .. " -->", 1, true))
  for index = automatic_start + 1, #calls do
    assert(calls[index].args[2] == "recap" and not has_flag(calls[index], "--stdin") and not has_flag(calls[index], "--auto"), "discovery generated a provider request")
  end

  -- Typed text after insertion has its own undo block. Then undoing the recap
  -- leaves the rough typing done before arrival untouched.
  vim.cmd("undo")
  assert(text(buffer):find(BEGIN, 1, true) and vim.api.nvim_get_current_line() == "unsaved rough while backend works")
  vim.cmd("undo")
  assert(not text(buffer):find(BEGIN, 1, true))
  before = count("recap")
  setup(true); setup(true)
  status("ready")
  assert(count("recap") == before + 1 and not text(buffer):find(BEGIN, 1, true), "undo was automatically discarded")
  call = request("TrailMixRecapRestore")
  finish(call, response(call, "Explicit restore after undo"))
  assert(text(buffer):find("Explicit restore after undo", 1, true))

  -- The saved marker also protects edits to generated text after close/reopen.
  vim.api.nvim_buf_set_lines(buffer, row(buffer, "## Summary"), row(buffer, "## Summary"), false, { "my generated-region edit" })
  local saved = lines(buffer)
  local saved_path = vim.api.nvim_buf_get_name(buffer)
  vim.fn.writefile(saved, saved_path)
  vim.api.nvim_buf_delete(buffer, { force = true })
  vim.cmd("edit " .. vim.fn.fnameescape(saved_path))
  buffer = vim.api.nvim_get_current_buf()
  setup(true)
  before = count("recap")
  status("ready")
  assert(count("recap") == before + 1 and text(buffer):find("my generated-region edit", 1, true), "saved result was reinserted")

  -- A new result waits while any visible split has a cursor in the generated
  -- region. Inactive tabs are not visible and do not block background insertion.
  local newer = string.rep("b", 32)
  vim.api.nvim_win_set_cursor(0, { row(buffer, "## Summary"), 0 })
  status("ready", newer)
  call = wait_flag("--latest")
  value = response(call, "Newer recap")
  value.recap_id = newer
  while_typing("R", function()
    assert(vim.api.nvim_get_mode().mode == "R", "fixture did not enter Replace mode")
    finish(call, value)
    assert(text(buffer):find("my generated-region edit", 1, true) and not text(buffer):find("Newer recap", 1, true), "generated edit was replaced during Replace mode")
  end)
  assert(text(buffer):find("my generated-region edit", 1, true), "generated edit was replaced under cursor")
  vim.api.nvim_buf_set_lines(buffer, -1, -1, false, { "rough edited while recap deferred" })
  local first_window = vim.api.nvim_get_current_win()
  vim.cmd("vsplit")
  local other_window = vim.api.nvim_get_current_win()
  vim.api.nvim_win_set_cursor(other_window, { row(buffer, "## Rough notes") + 1, 0 })
  status("ready", newer)
  assert(not text(buffer):find("Newer recap", 1, true), "another visible split's cursor was ignored")
  vim.api.nvim_win_close(other_window, true)
  assert(vim.api.nvim_get_current_win() == first_window)
  vim.cmd("tab split")
  other_window = vim.api.nvim_get_current_win()
  vim.api.nvim_win_set_cursor(other_window, { row(buffer, "## Rough notes") + 1, 0 })
  status("ready", newer)
  assert(text(buffer):find("Newer recap", 1, true) and text(buffer):find("rough edited while recap deferred", 1, true), "inactive tab cursor blocked insertion")
  assert(vim.api.nvim_get_current_win() == other_window, "other-tab insertion stole focus")
  vim.cmd("tabclose")

  -- Hidden loaded notes are eligible, without changing focus or writing disk.
  buffer = fresh()
  local on_disk = vim.fn.readfile(vim.api.nvim_buf_get_name(buffer))
  setup(true)
  local scratch = vim.api.nvim_create_buf(true, true)
  vim.api.nvim_set_current_buf(scratch)
  local focus_before = vim.api.nvim_get_current_win()
  status("ready")
  call = wait_flag("--latest")
  vim.api.nvim_buf_set_lines(scratch, 0, -1, false, { "scratch typing" })
  while_typing("A", function()
    finish(call, response(call, "Hidden recap"))
    assert(vim.api.nvim_get_mode().mode == "i" and vim.api.nvim_get_current_buf() == scratch, "hidden insertion interrupted unrelated typing")
  end)
  assert(vim.api.nvim_get_current_line() == "scratch typing typed", "hidden insertion redirected unrelated typing")
  assert(text(buffer):find("Hidden recap", 1, true))
  assert(vim.api.nvim_get_current_buf() == scratch and vim.api.nvim_get_current_win() == focus_before)
  assert(vim.deep_equal(vim.fn.readfile(vim.api.nvim_buf_get_name(buffer)), on_disk), "automatic insertion saved disk")

  -- Backend cross-process conflicts defer reads/insertion. Once resolved the
  -- same result is discovered without having to restart this editor.
  buffer = fresh()
  setup(true)
  before = count("recap")
  status("ready", nil, { editor_conflict = true, editor_reason = "another editor owns this note" })
  assert(count("recap") == before + 1 and not text(buffer):find(BEGIN, 1, true))
  status("ready")
  call = wait_flag("--latest")
  finish(call, response(call, "Conflict resolved"))
  assert(text(buffer):find("Conflict resolved", 1, true))
  setup(false)
  before = count("recap")
  vim.cmd("TrailMixRecapRestore")
  call = latest("recap")
  assert(has_flag(call, "--status"))
  finish(call, { schema_version = 1, meeting_id = id, note_path = call.note_path, state = "ready",
    recap_id = newer, editor_conflict = true, editor_reason = "another editor still owns the note" })
  assert(count("recap") == before + 1 and not has_flag(latest("recap"), "--latest"), "Restore bypassed another editor's conflict")

  -- Manual generation also checks foreign editors after the provider returns.
  -- A deferred manual result rechecks status before every later insertion try.
  buffer = fresh()
  call = request()
  call.defer_status = true
  before = text(buffer)
  finish(call, response(call, "Manual after conflict"))
  local check = wait_flag("--status")
  finish(check, { schema_version = 1, meeting_id = id, note_path = check.note_path, state = "ready",
    recap_id = call.result_id, editor_conflict = true, editor_reason = "other editor" })
  assert(text(buffer) == before, "manual result bypassed another editor's conflict")
  vim.api.nvim_exec_autocmds("CursorMoved", { buffer = buffer })
  check = wait_flag("--status")
  finish(check, { schema_version = 1, meeting_id = id, note_path = check.note_path, state = "ready",
    recap_id = call.result_id, editor_conflict = true, editor_reason = "still another editor" })
  assert(text(buffer) == before, "deferred manual result reused an old conflict check")
  vim.api.nvim_exec_autocmds("CursorMoved", { buffer = buffer })
  check = wait_flag("--status")
  finish(check, { schema_version = 1, meeting_id = id, note_path = check.note_path, state = "ready",
    recap_id = call.result_id, editor_conflict = false })
  assert(text(buffer):find("Manual after conflict", 1, true), "resolved manual conflict did not apply")

  -- Copies and wrong associations cannot be edited, even when only a copy is
  -- open. Both status and latest must match the canonical associated main note.
  for _, mismatch in ipairs({ "status", "latest" }) do
    buffer = fresh()
    local foreign = directory .. "/foreign.md"
    vim.fn.writefile({ marker, "## Rough notes", "foreign" }, foreign)
    setup(true)
    before = text(buffer)
    if mismatch == "status" then status("ready", nil, { note_path = foreign })
    else
      status("ready")
      call = wait_flag("--latest")
      value = response(call); value.note_path = foreign
      finish(call, value)
    end
    assert(text(buffer) == before, mismatch .. " path mismatch applied a recap")
  end
  buffer = fresh()
  duplicate = vim.api.nvim_create_buf(true, false)
  vim.api.nvim_buf_set_name(duplicate, directory .. "/copy.md")
  vim.api.nvim_buf_set_lines(duplicate, 0, -1, false, { marker, "## Rough notes", "copy" })
  vim.fn.writefile(lines(duplicate), vim.api.nvim_buf_get_name(duplicate))
  setup(true)
  before = count("recap")
  vim.wait(220, function() return false end, 10)
  assert(count("recap") == before, "duplicate note copy was polled")
  vim.api.nvim_buf_delete(duplicate, { force = true })
  status("ready")
  call = wait_flag("--latest")
  finish(call, response(call))
  assert(text(buffer):find(BEGIN, 1, true))

  -- Canonical aliases are accepted, rather than requiring byte-identical paths.
  buffer = fresh()
  local alias = directory .. "/main-note-link.md"
  assert((vim.uv or vim.loop).fs_symlink(vim.api.nvim_buf_get_name(buffer), alias))
  setup(true)
  status("ready", nil, { note_path = alias })
  call = wait_flag("--latest")
  value = response(call, "Canonical alias"); value.note_path = alias
  finish(call, value)
  assert(text(buffer):find("Canonical alias", 1, true))

  -- Canceling an in-flight discovery suppresses that result, not future IDs.
  buffer = fresh()
  setup(true)
  status("ready")
  call = wait_flag("--latest")
  before = text(buffer)
  vim.cmd("TrailMixRecapCancel")
  assert(call.killed == 15)
  finish(call, response(call))
  local call_count = count("recap")
  status("ready")
  assert(count("recap") <= call_count + 1 and text(buffer) == before, "canceled discovery was reapplied")
  status("ready")
  assert(text(buffer) == before, "canceled result was rediscovered on the next poll")
  status("ready", newer)
  call = wait_flag("--latest")
  value = response(call, "New result after cancel"); value.recap_id = newer
  finish(call, value)
  assert(text(buffer):find("New result after cancel", 1, true))

  -- Cancel before the status response reveals its result ID. The next ready
  -- ID becomes the suppressed baseline, while a distinct result is eligible.
  buffer = fresh()
  setup(true)
  local unidentified = wait_flag("--status")
  before = text(buffer)
  vim.cmd("TrailMixRecapCancel")
  assert(unidentified.killed == 15)
  finish(unidentified, { schema_version = 1, meeting_id = id, note_path = unidentified.note_path,
    state = "ready", recap_id = string.rep("a", 32), editor_conflict = false })
  status("ready")
  status("ready")
  assert(text(buffer) == before, "status-phase cancellation did not suppress discovery")
  status("ready", newer)
  call = wait_flag("--latest")
  value = response(call, "Different ID after status cancel"); value.recap_id = newer
  finish(call, value)
  assert(text(buffer):find("Different ID after status cancel", 1, true))

  -- A backend job can be canceled even without any local request. The plugin
  -- verifies the association first and never submits a generation request.
  buffer = fresh()
  before = text(buffer)
  vim.cmd("TrailMixRecapCancel")
  check = wait_flag("--status")
  finish(check, { schema_version = 1, meeting_id = id, note_path = check.note_path,
    state = "generating", editor_conflict = false })
  call = wait_flag("--cancel")
  assert(not call.opts.stdin and call.opts.timeout == 8000)
  finish(call, { schema_version = 1, meeting_id = id, note_path = call.note_path,
    state = "generating", error = "background recap cancellation requested", editor_conflict = false })
  assert(text(buffer) == before)
  setup(true)
  local notice_count = #notices
  status("cancelled")
  assert(#notices == notice_count + 1, "cancellation terminal state was not reported")
  status("cancelled")
  assert(#notices == notice_count + 1, "cancellation terminal state was reported repeatedly")
  status("queued")
  status("ready", newer)
  call = wait_flag("--latest")
  value = response(call, "Re-enqueued job"); value.recap_id = newer
  finish(call, value)
  assert(text(buffer):find("Re-enqueued job", 1, true), "old cancellation suppressed a fresh backend job")

  -- SwapExists does not choose recovery or remove a swap file. Automatic reads
  -- remain blocked for this note/session; explicit Restore is the user's opt-in.
  buffer = fresh()
  vim.api.nvim_exec_autocmds("SwapExists", { pattern = vim.api.nvim_buf_get_name(buffer) })
  assert(vim.b[buffer].trailmix_recap_swap_pending == true, "swap block is not visible to the recorder")
  setup(true)
  status("ready")
  assert(not text(buffer):find(BEGIN, 1, true))
  assert(table.concat(notices, "\n"):find("Resolve recovery", 1, true))
  call = request("TrailMixRecapRestore")
  assert(vim.b[buffer].trailmix_recap_swap_pending == nil, "explicit Restore did not clear its own recovery flag")
  finish(call, response(call, "Recovered explicitly"))
  assert(text(buffer):find("Recovered explicitly", 1, true))

  -- Stale latest callbacks cannot affect closed, renamed, identity-changed or
  -- nonmodifiable buffers. Reopening the original note can discover privately
  -- saved results later. A hidden buffer, by contrast, was tested above.
  for _, change in ipairs({ "close", "rename", "identity", "nonmodifiable" }) do
    buffer = fresh()
    setup(true)
    status("ready")
    call = wait_flag("--latest")
    value = response(call, "Must remain pending")
    if change == "close" then vim.api.nvim_buf_delete(buffer, { force = true })
    elseif change == "rename" then vim.api.nvim_buf_set_name(buffer, directory .. "/renamed-auto.md")
    elseif change == "identity" then vim.api.nvim_buf_set_lines(buffer, 1, 2, false, { "<!-- trailmix: meeting " .. other .. " -->" })
    else vim.bo[buffer].modifiable = false end
    local changed = vim.api.nvim_buf_is_loaded(buffer) and text(buffer)
    finish(call, value)
    if changed then assert(text(buffer) == changed, change .. " stale callback edited note") end
  end

  -- Surrounding whitespace on the sole identity is harmless.
  buffer = fresh({ "# Meeting", "  " .. marker .. "  ", "## Rough notes", "raw" })
  setup(true)
  status("ready")
  call = wait_flag("--latest"); finish(call, response(call))
  assert(text(buffer):find(BEGIN, 1, true))

  -- Fenced IDs don't count, duplicated or malformed live IDs do not poll.
  buffer = fresh({ "# Meeting", marker, "```md", "<!-- trailmix: meeting " .. other .. " -->", "```", "## Rough notes", "raw" })
  setup(true)
  status("ready")
  call = wait_flag("--latest"); finish(call, response(call))
  assert(text(buffer):find(BEGIN, 1, true))
  buffer = fresh({ marker, marker, "## Rough notes", "raw" })
  setup(true)
  before = count("recap")
  vim.wait(220, function() return false end, 10)
  assert(count("recap") == before)

  -- Status concurrency stays bounded, and polling rotates across loaded notes.
  buffer = fresh()
  local extra_ids = {}
  for index = 1, 5 do
    local meeting = string.format("%032x", index + 100)
    extra_ids[#extra_ids + 1] = meeting
    local candidate = vim.api.nvim_create_buf(true, false)
    vim.api.nvim_buf_set_name(candidate, directory .. "/extra-" .. index .. ".md")
    vim.api.nvim_buf_set_lines(candidate, 0, -1, false, { "<!-- trailmix: meeting " .. meeting .. " -->", "## Rough notes", "raw" })
    vim.fn.writefile(lines(candidate), vim.api.nvim_buf_get_name(candidate))
  end
  setup(true)
  wait_flag("--status")
  local inflight_calls = {}
  for _, candidate in ipairs(calls) do
    if has_flag(candidate, "--status") and not candidate.finished and not candidate.killed then inflight_calls[#inflight_calls + 1] = candidate end
  end
  assert(#inflight_calls == 4, "status concurrency is not bounded to four")
  local visited = {}
  for _, candidate in ipairs(inflight_calls) do
    visited[candidate.args[4]] = true
    finish(candidate, { schema_version = 1, meeting_id = candidate.args[4], note_path = candidate.note_path, state = "none", editor_conflict = false })
  end
  for _, meeting in ipairs(extra_ids) do
    if not visited[meeting] then
      local candidate = wait_flag("--status", meeting)
      finish(candidate, { schema_version = 1, meeting_id = meeting, note_path = candidate.note_path, state = "none", editor_conflict = false })
    end
  end

  -- Registration is a prerequisite for discovery and all safe insertion, not
  -- for generation or cancellation. Fail closed without warning on every tick.
  buffer = fresh()
  local editor_registry = require("trailmix.recap_editors")
  local usable_home = vim.env.TRAILMIX_HOME
  local unusable_home = directory .. "/state-home-is-a-file"
  vim.fn.writefile({ "not a directory" }, unusable_home)
  vim.env.TRAILMIX_HOME = unusable_home
  local registered, registration_error = editor_registry.update(true)
  assert(registered == false and type(registration_error) == "string", "registration failure was not reported")
  before = count("recap")
  setup(true)
  vim.wait(240, function() return false end, 10)
  assert(count("recap") == before, "unregistered editor still discovered recaps")
  local original_note = text(buffer)
  call = request()
  assert(vim.json.decode(call.opts.stdin).editor_pid == vim.fn.getpid(), "unregistered generation omitted its editor PID")
  finish(call, response(call, "Must remain private without registration"))
  assert(text(buffer) == original_note, "manual generation inserted without registration")
  before = count("recap")
  vim.cmd("TrailMixRecapRestore")
  assert(count("recap") == before and text(buffer) == original_note, "Restore inserted without registration")
  -- Cancellation still verifies the association and can cancel a backend job,
  -- even when registration is unavailable and another editor is reported.
  vim.cmd("TrailMixRecapCancel")
  check = wait_flag("--status")
  finish(check, { schema_version = 1, meeting_id = id, note_path = check.note_path,
    state = "generating", editor_conflict = true, editor_reason = "another editor" })
  call = wait_flag("--cancel")
  finish(call, { schema_version = 1, meeting_id = id, note_path = call.note_path,
    state = "cancelled", editor_conflict = true })
  assert(text(buffer) == original_note, "cancellation modified an unregistered note")
  vim.wait(240, function() return false end, 10)
  local registration_notices = 0
  for _, message in ipairs(notices) do
    if message:find(registration_error, 1, true) then registration_notices = registration_notices + 1 end
  end
  assert(registration_notices == 1, "registration failure warning repeated every poll")
  vim.env.TRAILMIX_HOME = usable_home
  setup(true)
  status("ready", newer)
  call = wait_flag("--latest")
  value = response(call, "Registry recovered"); value.recap_id = newer
  finish(call, value)
  assert(text(buffer):find("Registry recovered", 1, true), "registration repair did not resume discovery")

  -- Registration can also fail after latest starts. The callback must recheck
  -- registration, rather than trusting the success before the async read.
  buffer = fresh()
  setup(true)
  status("ready")
  call = wait_flag("--latest")
  vim.env.TRAILMIX_HOME = unusable_home
  original_note = text(buffer)
  finish(call, response(call, "Registry failed in flight"))
  assert(text(buffer) == original_note, "late result inserted after registration failed")
  vim.env.TRAILMIX_HOME = usable_home
  setup(false)

  -- Repeated setup cancels polls and manual requests, deduplicates autocmds and
  -- ignores stale callbacks. Opt-out and no-notes clean up timers/registration.
  buffer = fresh()
  setup(true)
  local obsolete = wait_flag("--status")
  assert(obsolete.opts.timeout == 8000)
  setup(true); setup(true)
  assert(obsolete.killed == 15)
  finish(obsolete, { schema_version = 1, state = "ready", meeting_id = id, note_path = obsolete.note_path,
    recap_id = string.rep("a", 32), editor_conflict = false })
  status("none")
  call = request()
  setup(false)
  assert(call.killed == 15)
  finish(call, response(call))
  assert(not text(buffer):find(BEGIN, 1, true), "setup allowed stale manual insertion")
  before = count("recap")
  vim.wait(220, function() return false end, 10)
  assert(count("recap") == before, "opt-out still polls")
  local registry_path = vim.env.TRAILMIX_HOME .. "/recap-editors/" .. vim.fn.getpid() .. ".json"
  local registration = vim.json.decode(table.concat(vim.fn.readfile(registry_path), "\n"))
  assert(registration.pid == vim.fn.getpid() and registration.socket == vim.v.servername and vim.tbl_count(registration) == 2)
  assert((vim.uv or vim.loop).fs_stat(registry_path).mode % 512 == 384, "registry is not owner-only")
  assert((vim.uv or vim.loop).fs_stat(vim.env.TRAILMIX_HOME .. "/recap-editors").mode % 512 == 448, "registry directory is not owner-only")
  assert(editor_registry.update(true) == true, "cached registration did not report success")
  assert((vim.uv or vim.loop).fs_chmod(registry_path, 420))
  assert(editor_registry.update(true) == true, "registration permissions were not repaired")
  assert((vim.uv or vim.loop).fs_stat(registry_path).mode % 512 == 384, "cached registration accepted public permissions")
  editor_registry.stop()
  local filesystem = vim.uv or vim.loop
  local original_write = filesystem.fs_write
  filesystem.fs_write = function() return nil, "fixture registry write failure" end
  registered, registration_error = editor_registry.update(true)
  filesystem.fs_write = original_write
  assert(registered == false and type(registration_error) == "string", "atomic write failure was not reported")
  assert(vim.fn.filereadable(registry_path) == 0, "failed registration was published")
  assert(#vim.fn.glob(vim.env.TRAILMIX_HOME .. "/recap-editors/*.tmp", false, true) == 0, "failed atomic registration leaked a temporary file")
  assert(editor_registry.update(true) == true, "fresh registration did not report success")
  -- The normal auto socket can be absent. Start a private managed endpoint,
  -- without exposing note contents in the registry or deleting any swap file.
  require("trailmix.recap_editors").stop()
  if vim.v.servername ~= "" then vim.fn.serverstop(vim.v.servername) end
  assert(vim.v.servername == "")
  local tcp_socket = vim.fn.serverstart("127.0.0.1:0")
  assert(tcp_socket:sub(1, 1) ~= "/" and vim.v.servername == tcp_socket)
  setup(false)
  registration = vim.json.decode(table.concat(vim.fn.readfile(registry_path), "\n"))
  assert(registration.socket == vim.env.TRAILMIX_HOME .. "/e-" .. vim.fn.getpid() .. ".sock")
  assert((vim.uv or vim.loop).fs_stat(registration.socket).mode % 512 == 384, "private socket is not owner-only")
  setup(true)
  local inflight = wait_flag("--status")
  vim.api.nvim_buf_delete(buffer, { force = true })
  drain()
  assert(inflight.killed == 15)
  finish(inflight, { schema_version = 1, state = "none", meeting_id = id, note_path = inflight.note_path, editor_conflict = false })
  before = count("recap")
  vim.wait(220, function() return false end, 10)
  assert(count("recap") == before, "no-note timer leaked")
  assert(vim.fn.filereadable(registry_path) == 0, "no-note registry leaked")
  vim.fn.serverstop(tcp_socket)
end, debug.traceback)
setup(false)
vim.api.nvim_del_augroup_by_name("TrailMixRecap")
vim.system, vim.fn.jobstart = original_system, original_jobstart
require("trailmix.recap_editors").stop()
vim.env.TRAILMIX_HOME = previous_home
vim.fn.delete(directory, "rf")
if not ok then error(err) end
print("TrailMix recap checks passed")
vim.cmd("qa!")
