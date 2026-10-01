-- Run with nvim --headless -u NONE --noplugin -i NONE -l tests/nvim/transcript.lua.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
local plugin = require("trailmix")
local directory = vim.fn.tempname()
vim.fn.mkdir(directory, "p")
local cli = directory .. "/fake cli"
local id = "0123456789abcdef0123456789abcdef"
local other_id = "ffffffffffffffffffffffffffffffff"
local marker = "<!-- trailmix: meeting " .. id .. " -->"
vim.fn.writefile({
  "#!/bin/sh",
  'test "$1" = transcript && test "$2" = --meeting && test "$3" = "' .. id .. '" && test "$4" = --json || exit 2',
  'printf "%s\\n" "$3" >> "' .. directory .. '/requests"',
  'payload=$(cat "' .. directory .. '/response.json")',
  'sleep "${TRAILMIX_TEST_DELAY:-0.02}"',
  'printf "%s\\n" "$payload"',
  'exit "${TRAILMIX_TEST_EXIT:-0}"',
}, cli)
vim.fn.setfperm(cli, "rwx------")
plugin.setup({ cli = cli, mapping = false, transcript_refresh_ms = 100 })
local notices = {}
vim.notify = function(message) notices[#notices + 1] = message end

local function response(passages, overrides)
  local snapshot = vim.tbl_extend("force", {
    meeting_id = id, duration_seconds = 120,
    passages = passages or {
      { id = "microphone-first", meeting_id = id, track = "microphone", start_seconds = 58, end_seconds = 63, text = "When is the review?" },
      { id = "system-later", meeting_id = id, track = "system", start_seconds = 62, end_seconds = 65, text = "Friday is tentative." },
    },
    transcription = { meeting_id = id, state = "complete", tracks = {
      microphone = { state = "complete" }, system = { state = "complete" },
    } },
  }, overrides or {})
  vim.fn.writefile({ vim.json.encode(snapshot) }, directory .. "/response.json")
end

local function text(buffer)
  return table.concat(vim.api.nvim_buf_get_lines(buffer, 0, -1, false), "\n")
end

local function wait_for(predicate, message)
  assert(vim.wait(3000, predicate, 10), message or "transcript response timed out")
end

local function line_of(buffer, needle)
  for index, line in ipairs(vim.api.nvim_buf_get_lines(buffer, 0, -1, false)) do
    if line:find(needle, 1, true) then return index end
  end
  error("missing transcript line: " .. needle)
end

local ok, err = xpcall(function()
  response()
  vim.api.nvim_buf_set_lines(0, 0, -1, false, { marker, "@01:03", "- review uncertain" })
  vim.api.nvim_win_set_cursor(0, { 3, 2 })
  local note, note_window = vim.api.nvim_get_current_buf(), vim.api.nvim_get_current_win()
  local original = text(note)
  vim.cmd("TrailMixTranscript")
  assert(vim.api.nvim_get_current_win() == note_window, "opening transcript stole focus")
  local windows = vim.api.nvim_tabpage_list_wins(0)
  assert(#windows == 2, "expected one transcript split")
  local transcript_window = windows[1] == note_window and windows[2] or windows[1]
  local transcript = vim.api.nvim_win_get_buf(transcript_window)
  assert(vim.api.nvim_win_get_position(transcript_window)[2] > vim.api.nvim_win_get_position(note_window)[2], "split is not on the right")
  wait_for(function() return text(transcript):find("Friday is tentative.", 1, true) end)
  assert(vim.bo[transcript].readonly and not vim.bo[transcript].modifiable and vim.bo[transcript].buftype == "nofile", "transcript is editable")
  assert(text(note) == original and vim.deep_equal(vim.api.nvim_win_get_cursor(note_window), { 3, 2 }), "note changed")
  vim.cmd("TrailMixTranscript")
  assert(#vim.api.nvim_tabpage_list_wins(0) == 2, "reopening duplicated split")

  local live = { meeting_id = id, state = "streaming", tracks = {
    microphone = { state = "streaming", provisional = { item = "Maybe Thursday…" } },
    system = { state = "degraded", error = "fixture disconnect", incomplete_ranges = { { id = "gap", start_seconds = 90, end_seconds = 95 } } },
  } }
  local passages = {}
  for i = 1, 25 do
    passages[i] = { id = "passage-" .. i, meeting_id = id, track = "microphone", start_seconds = i * 3, end_seconds = i * 3 + 2, text = "Final statement " .. i }
  end
  response(passages, { transcription = live })
  wait_for(function() return text(transcript):find("Maybe Thursday…", 1, true) end, "live polling did not refresh")
  assert(text(transcript):find("Transcribing…", 1, true), "provisional text is not distinguished")
  assert(text(transcript):find("fixture disconnect", 1, true) and text(transcript):find("incomplete", 1, true), "missing degraded status")
  assert(vim.api.nvim_get_current_win() == note_window, "refresh stole focus")
  vim.api.nvim_set_current_win(transcript_window)
  vim.api.nvim_feedkeys("G", "xt", false)
  wait_for(function() return text(transcript):find("following latest", 1, true) end)
  assert(vim.api.nvim_win_get_cursor(transcript_window)[1] == vim.api.nvim_buf_line_count(transcript))
  -- Scrolling back pauses following. An earlier final must not move the reader
  -- off the passage under the cursor or off the top of the visible viewport.
  vim.api.nvim_win_set_cursor(transcript_window, { line_of(transcript, "Final statement 12"), 0 })
  vim.cmd("normal! zz")
  vim.api.nvim_exec_autocmds("CursorMoved", { buffer = transcript })
  local top_before = vim.fn.line("w0")
  local top_text = vim.api.nvim_buf_get_lines(transcript, top_before - 1, top_before, false)[1]
  table.insert(passages, 1, { id = "late-arrival", meeting_id = id, track = "system", start_seconds = 1, end_seconds = 2, text = "An earlier final arrived late" })
  live.tracks.microphone.provisional = nil
  response(passages, { transcription = live })
  wait_for(function() return text(transcript):find("An earlier final arrived late", 1, true) end)
  assert(vim.api.nvim_get_current_line() == "Final statement 12", "late final displaced the reading position")
  assert(vim.api.nvim_buf_get_lines(transcript, vim.fn.line("w0") - 1, vim.fn.line("w0"), false)[1] == top_text, "late final displaced the viewport")
  assert(not text(transcript):find("Maybe Thursday…", 1, true), "stale provisional text retained")
  assert(text(transcript):find("paused", 1, true), "scrolling did not pause following")
  assert(text(note) == original, "live refresh edited the note")

  response()
  wait_for(function() return text(transcript):find("Friday is tentative.", 1, true) end)
  vim.api.nvim_set_current_win(note_window)
  vim.cmd("TrailMixTranscriptJump")
  assert(vim.api.nvim_get_current_win() == transcript_window, "jump did not focus transcript")
  wait_for(function() return vim.api.nvim_get_current_line():find("00:58–01:03 microphone", 1, true) end, "jump did not use bullet's preceding anchor")
  vim.cmd("TrailMixTranscript system-later")
  wait_for(function() return vim.api.nvim_get_current_line():find("01:02–01:05 system", 1, true) end, "exact passage navigation failed")
  vim.cmd("TrailMixTranscript missing-passage")
  wait_for(function() return (notices[#notices] or ""):find("not found", 1, true) end, "missing passage was silently substituted")

  vim.api.nvim_set_current_win(note_window)
  vim.api.nvim_buf_set_lines(note, 0, -1, false, { marker, "@00:59", "```markdown", "@01:30", "```", "- follow earlier anchor", "~~~", "@01:50", "~~~" })
  vim.api.nvim_win_set_cursor(note_window, { 9, 0 })
  vim.cmd("TrailMixTranscriptJump")
  wait_for(function() return vim.api.nvim_get_current_line():find("00:58–01:03 microphone", 1, true) end, "code fence anchor was interpreted")
  vim.api.nvim_set_current_win(note_window)
  vim.api.nvim_buf_set_lines(note, 0, -1, false, { marker, "@99:99", "- invalid anchor" })
  vim.api.nvim_win_set_cursor(note_window, { 3, 0 })
  vim.cmd("TrailMixTranscriptJump")
  assert(vim.api.nvim_get_current_win() == note_window and notices[#notices]:find("invalid", 1, true), "invalid anchor accepted")
  vim.api.nvim_buf_set_lines(note, 1, 2, false, { "@02:30" })
  vim.cmd("TrailMixTranscriptJump")
  wait_for(function() return (notices[#notices] or ""):find("duration", 1, true) end, "out-of-range anchor accepted")

  -- Jumps resolve against a read started after the request, not the cached
  -- snapshot: a live meeting outgrows its last duration and passage list.
  vim.env.TRAILMIX_TEST_DELAY = "0.3"
  local grown = {
    { id = "microphone-first", meeting_id = id, track = "microphone", start_seconds = 58, end_seconds = 63, text = "When is the review?" },
    { id = "system-later", meeting_id = id, track = "system", start_seconds = 62, end_seconds = 65, text = "Friday is tentative." },
    { id = "past-cache", meeting_id = id, track = "system", start_seconds = 140, end_seconds = 145, text = "Spoken after the cached read." },
  }
  response(grown, { duration_seconds = 150 })
  vim.api.nvim_set_current_win(note_window)
  vim.api.nvim_buf_set_lines(note, 0, -1, false, { marker, "@02:21", "- beyond the cached duration" })
  vim.api.nvim_win_set_cursor(note_window, { 3, 0 })
  local quiet = #notices
  vim.cmd("TrailMixTranscriptJump")
  wait_for(function() return vim.api.nvim_get_current_line():find("02:20–02:25 system", 1, true) end, "jump resolved against a stale duration")
  assert(#notices == quiet, "stale snapshot rejected a valid timestamp: " .. tostring(notices[#notices]))
  table.insert(grown, { id = "newest-passage", meeting_id = id, track = "microphone", start_seconds = 146, end_seconds = 149, text = "Cited before the transcript refreshed." })
  response(grown, { duration_seconds = 150 })
  vim.cmd("TrailMixTranscript newest-passage")
  wait_for(function() return vim.api.nvim_get_current_line():find("02:26–02:29 microphone", 1, true) end, "passage resolved against a stale snapshot")
  assert(#notices == quiet, "stale snapshot rejected a valid passage: " .. tostring(notices[#notices]))
  vim.env.TRAILMIX_TEST_DELAY = "0.02"
  response()

  -- Closing the split and reopening the note resolves stable passage IDs again.
  vim.api.nvim_set_current_win(note_window)
  vim.api.nvim_buf_set_lines(note, 0, -1, false, { marker, "@01:03", "- review uncertain" })
  local path = directory .. "/meeting.md"
  vim.cmd("write! " .. vim.fn.fnameescape(path))
  vim.api.nvim_win_close(transcript_window, false)
  assert(not vim.api.nvim_buf_is_valid(transcript), "closing split retained its scratch buffer")
  vim.cmd("bwipeout")
  vim.cmd("edit " .. vim.fn.fnameescape(path))
  note, note_window = vim.api.nvim_get_current_buf(), vim.api.nvim_get_current_win()
  vim.cmd("TrailMixTranscript system-later")
  transcript_window, transcript = vim.api.nvim_get_current_win(), vim.api.nvim_get_current_buf()
  wait_for(function() return vim.api.nvim_get_current_line():find("01:02–01:05 system", 1, true) end, "passage did not survive reopening")

  -- Failed refreshes retain evidence and show an error rather than replacing it
  -- with a complete-looking empty transcript or a different meeting's data.
  response(nil, { meeting_id = other_id })
  wait_for(function() return text(transcript):find("Refresh failed:", 1, true) end)
  assert(text(transcript):find("Friday is tentative.", 1, true), "bad refresh discarded finalized evidence")
  response()
  wait_for(function() return not text(transcript):find("Refresh failed:", 1, true) end)
  vim.env.TRAILMIX_TEST_EXIT = "1"
  wait_for(function() return text(transcript):find("CLI exited with code 1", 1, true) end)
  assert(text(transcript):find("Friday is tentative.", 1, true), "CLI failure discarded evidence")
  vim.env.TRAILMIX_TEST_EXIT = "0"
  vim.fn.writefile({ "{broken" }, directory .. "/response.json")
  wait_for(function() return text(transcript):find("invalid transcript response", 1, true) end)
  response()
  wait_for(function() return not text(transcript):find("Refresh failed:", 1, true) end)

  -- Search pauses following, and ordinary Neovim search/yank still work.
  vim.api.nvim_feedkeys("G", "xt", false)
  local search = vim.api.nvim_replace_termcodes("/Friday<CR>", true, false, true)
  vim.api.nvim_feedkeys(search, "xt", false)
  assert(vim.api.nvim_get_current_line() == "Friday is tentative.", "normal search failed")
  vim.cmd('normal! yy')
  assert(vim.fn.getreg('"'):find("Friday is tentative.", 1, true), "normal yank failed")
  wait_for(function() return text(transcript):find("paused", 1, true) end)

  -- A delayed jump cannot steal focus after the user leaves; closing its split
  -- stops future requests, and a late callback cannot recreate it.
  vim.api.nvim_feedkeys("q", "xt", false)
  assert(not vim.api.nvim_buf_is_valid(transcript))
  vim.env.TRAILMIX_TEST_DELAY = "0.3"
  vim.cmd("TrailMixTranscript system-later")
  transcript_window, transcript = vim.api.nvim_get_current_win(), vim.api.nvim_get_current_buf()
  vim.api.nvim_set_current_win(note_window)
  wait_for(function() return text(transcript):find("Friday is tentative.", 1, true) end)
  assert(vim.api.nvim_get_current_win() == note_window, "delayed jump stole focus")
  vim.api.nvim_win_close(transcript_window, false)
  vim.wait(100, function() return false end)
  local count = #vim.fn.readfile(directory .. "/requests")
  vim.wait(450, function() return false end)
  assert(#vim.fn.readfile(directory .. "/requests") == count, "closed split kept polling")
  assert(#vim.api.nvim_tabpage_list_wins(0) == 1, "late response reopened the split")

  -- Keep only one CLI request in flight even when reads exceed the interval.
  vim.env.TRAILMIX_TEST_DELAY = "0.5"
  vim.cmd("TrailMixTranscript")
  vim.wait(100, function() return false end)
  count = #vim.fn.readfile(directory .. "/requests")
  for _ = 1, 5 do vim.cmd("TrailMixTranscript") end
  vim.wait(150, function() return false end)
  assert(#vim.fn.readfile(directory .. "/requests") == count, "overlapping transcript requests")
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do
    if window ~= note_window then vim.api.nvim_win_close(window, false) end
  end

  -- Failed transcription does not make an impossible timestamp valid.
  vim.env.TRAILMIX_TEST_DELAY = "0.02"
  response(nil, { transcription = { meeting_id = id, state = "degraded", tracks = {
    microphone = { state = "degraded" }, system = { state = "complete" },
  } } })
  vim.cmd("TrailMixTranscript")
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do
    if window ~= note_window then transcript_window = window end
  end
  transcript = vim.api.nvim_win_get_buf(transcript_window)
  wait_for(function() return text(transcript):find("degraded", 1, true) end)
  vim.api.nvim_buf_set_lines(note, 0, -1, false, { marker, "@99:00:00", "- impossible time" })
  vim.api.nvim_win_set_cursor(note_window, { 3, 0 })
  local notice_count = #notices
  vim.cmd("TrailMixTranscriptJump")
  wait_for(function() return #notices > notice_count and (notices[#notices] or ""):find("duration", 1, true) end, "degraded transcript guessed out-of-range time")
  for _, invalid in ipairs({ "@-01:00", "@01:xx", "@xx:30" }) do
    vim.api.nvim_set_current_win(note_window)
    vim.api.nvim_buf_set_lines(note, 0, -1, false, { marker, "@00:30", invalid, "- current section" })
    vim.api.nvim_win_set_cursor(note_window, { 4, 0 })
    notice_count = #notices
    vim.cmd("TrailMixTranscriptJump")
    assert(#notices > notice_count and notices[#notices]:find("invalid", 1, true), "malformed anchor reused old section: " .. invalid)
  end

  -- Defer rendering during visual selection so both endpoints survive updates.
  response(passages, { transcription = live })
  wait_for(function() return text(transcript):find("Final statement 12", 1, true) end)
  vim.api.nvim_set_current_win(transcript_window)
  vim.api.nvim_win_set_cursor(0, { line_of(transcript, "Final statement 12"), 0 })
  vim.api.nvim_feedkeys("V", "xt", false)
  local selected_text = vim.api.nvim_get_current_line()
  table.insert(passages, 1, { id = "another-late-final", meeting_id = id, track = "system", start_seconds = 0, end_seconds = 1, text = "Another late final" })
  response(passages, { transcription = live })
  vim.wait(250, function() return false end)
  assert(not text(transcript):find("Another late final", 1, true), "render changed an active visual selection")
  vim.api.nvim_feedkeys("y", "xt", false)
  assert(vim.fn.getreg('"') == selected_text .. "\n", "visual yank changed selected text")
  wait_for(function() return text(transcript):find("Another late final", 1, true) end)

  -- Moving a transcript to another tab keeps polling and q closes its actual
  -- window. Duplicating and closing the original window must not strand it.
  vim.cmd("wincmd T")
  response(nil, { transcription = live })
  wait_for(function() return text(transcript):find("Friday is tentative.", 1, true) end, "moving transcript stopped polling")
  vim.api.nvim_feedkeys("q", "xt", false)
  assert(not vim.api.nvim_buf_is_valid(transcript), "q did not close moved transcript")
  vim.api.nvim_set_current_win(note_window)
  vim.cmd("TrailMixTranscript system-later")
  transcript_window, transcript = vim.api.nvim_get_current_win(), vim.api.nvim_get_current_buf()
  wait_for(function() return text(transcript):find("Friday is tentative.", 1, true) end)
  vim.cmd("vsplit")
  vim.api.nvim_win_close(transcript_window, false)
  vim.cmd("TrailMixTranscript")
  response(passages, { transcription = live })
  wait_for(function() return text(transcript):find("Another late final", 1, true) end, "closing original window stranded duplicate")
  vim.api.nvim_feedkeys("q", "xt", false)
  assert(not vim.api.nvim_buf_is_valid(transcript), "q did not close surviving transcript")

  vim.api.nvim_set_current_win(note_window)
  response(passages, { transcription = live })
  vim.cmd("TrailMixTranscript")
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do
    if window ~= note_window then transcript_window = window end
  end
  transcript = vim.api.nvim_win_get_buf(transcript_window)
  wait_for(function() return text(transcript):find("following latest", 1, true) end, "new live transcript did not follow")
  assert(vim.api.nvim_win_get_cursor(transcript_window)[1] == vim.api.nvim_buf_line_count(transcript))
  passages[#passages + 1] = { id = "newest", meeting_id = id, track = "system", start_seconds = 100, end_seconds = 110, text = "Newest speech" }
  response(passages, { transcription = live })
  wait_for(function() return text(transcript):find("Newest speech", 1, true) end)
  assert(vim.api.nvim_win_get_cursor(transcript_window)[1] == vim.api.nvim_buf_line_count(transcript), "follow did not advance")
  assert(vim.api.nvim_get_current_win() == note_window, "live following stole notes focus")
  vim.env.TRAILMIX_TEST_DELAY = "5"
  assert(vim.wait(6500, function() return text(transcript):find("request timed out", 1, true) end, 20), "hung CLI request was not bounded")
  assert(text(transcript):find("Newest speech", 1, true), "timeout discarded previous evidence")
  vim.api.nvim_win_close(transcript_window, false)

  -- A window that kept its panel tag but now holds the focused note, as an
  -- older restore could leave it, is the user's editing window, not a panel.
  vim.env.TRAILMIX_TEST_DELAY = "0.02"
  response()
  assert(#vim.api.nvim_tabpage_list_wins(0) == 1, "fixture expects only the note window")
  vim.api.nvim_set_current_win(note_window)
  vim.w[note_window].trailmix_panel = true
  vim.cmd("TrailMixTranscript")
  assert(vim.api.nvim_win_get_buf(note_window) == note, "panel replaced the focused note")
  assert(vim.api.nvim_get_current_win() == note_window, "note lost focus")
  assert(#vim.api.nvim_tabpage_list_wins(0) == 2, "transcript did not open beside the note")
  assert(not vim.w[note_window].trailmix_panel, "note window kept its stale panel tag")
end, debug.traceback)

vim.fn.delete(directory, "rf")
if not ok then error(err) end
print("TrailMix transcript checks passed")
vim.cmd("qa!")
