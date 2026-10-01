-- Run with nvim --headless -u NONE --noplugin -i NONE -l tests/nvim/note_jump.lua.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
vim.g.mapleader = " "
local plugin = require("trailmix")
local directory = vim.fn.tempname()
vim.fn.mkdir(directory, "p")
local cli = directory .. "/fake cli"
local id = "0123456789abcdef0123456789abcdef"
local marker = "<!-- trailmix: meeting " .. id .. " -->"
vim.fn.writefile({
  "#!/bin/sh",
  'test "$1" = transcript && test "$2" = --meeting && test "$3" = "' .. id .. '" && test "$4" = --json || exit 2',
  'cat "' .. directory .. '/response.json"',
}, cli)
vim.fn.setfperm(cli, "rwx------")
vim.fn.writefile({ vim.json.encode({
  schema_version = 1, meeting_id = id, duration_seconds = 120,
  passages = {
    { id = "microphone-first", meeting_id = id, track = "microphone", start_seconds = 58, end_seconds = 63, text = "When is the review?" },
    { id = "system-later", meeting_id = id, track = "system", start_seconds = 62, end_seconds = 65, text = "Friday is tentative." },
  },
  transcription = { meeting_id = id, state = "complete", tracks = {
    microphone = { state = "complete" }, system = { state = "complete" },
  } },
}) }, directory .. "/response.json")
plugin.setup({ cli = cli, transcript_refresh_ms = 100 })
local notices = {}
vim.notify = function(message) notices[#notices + 1] = message end

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
  error("missing line: " .. needle)
end

local function last_notice() return notices[#notices] or "" end

local function keys(sequence)
  vim.api.nvim_feedkeys(vim.api.nvim_replace_termcodes(sequence, true, false, true), "xt", false)
end

local ok, err = xpcall(function()
  -- Default mappings exist for both jump directions and stepping back.
  for _, lhs in ipairs({ " ml", " mh", " mH" }) do
    assert(vim.fn.maparg(lhs, "n") ~= "", "missing default mapping " .. lhs)
  end

  -- Anchors deliberately out of order, with a fenced one that must be ignored.
  local note, note_window = vim.api.nvim_get_current_buf(), vim.api.nvim_get_current_win()
  vim.api.nvim_buf_set_lines(note, 0, -1, false, {
    marker, "@01:30", "- later topic", "@00:55", "- review", "```", "@01:00", "```", "@01:10", "- friday",
  })
  vim.cmd("TrailMixTranscript")
  local transcript_window
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do
    if window ~= note_window then transcript_window = window end
  end
  local transcript = vim.api.nvim_win_get_buf(transcript_window)
  wait_for(function() return text(transcript):find("Friday is tentative.", 1, true) end)

  -- From a passage, go to the closest anchor by time, wherever it sits.
  vim.api.nvim_set_current_win(transcript_window)
  vim.api.nvim_win_set_cursor(transcript_window, { line_of(transcript, "When is the review?"), 0 })
  vim.cmd("TrailMixNoteJump")
  assert(vim.api.nvim_get_current_win() == note_window, "note jump did not focus the note")
  assert(vim.api.nvim_win_get_cursor(note_window)[1] == 4, "closest anchor not chosen")
  assert(last_notice():find("@00:55", 1, true) and last_notice():find("3s before", 1, true) and last_notice():find("1 of 3", 1, true),
    "jump did not describe the match: " .. last_notice())

  -- Repeating from the note steps outward by closeness, wraps, and steps back.
  vim.cmd("TrailMixNoteJump")
  assert(vim.api.nvim_win_get_cursor(note_window)[1] == 9 and last_notice():find("12s after", 1, true) and last_notice():find("2 of 3", 1, true),
    "repeat did not move to the next-closest anchor: " .. last_notice())
  vim.cmd("TrailMixNoteJump")
  assert(vim.api.nvim_win_get_cursor(note_window)[1] == 2 and last_notice():find("3 of 3", 1, true), "third candidate wrong")
  vim.cmd("TrailMixNoteJump")
  assert(vim.api.nvim_win_get_cursor(note_window)[1] == 4 and last_notice():find("1 of 3", 1, true), "cycle did not wrap forward")
  vim.cmd("TrailMixNoteJumpBack")
  assert(vim.api.nvim_win_get_cursor(note_window)[1] == 2 and last_notice():find("3 of 3", 1, true), "cycle did not wrap backward")
  keys(" mH")
  assert(vim.api.nvim_win_get_cursor(note_window)[1] == 9, "mapping did not step back")
  keys(" mh")
  assert(vim.api.nvim_win_get_cursor(note_window)[1] == 2, "mapping did not step forward")

  -- Moving away ends the cycle instead of jumping somewhere surprising.
  vim.api.nvim_win_set_cursor(note_window, { 5, 0 })
  vim.cmd("TrailMixNoteJump")
  assert(vim.api.nvim_win_get_cursor(note_window)[1] == 5 and last_notice():find("transcript passage", 1, true), "stale cycle continued")
  -- So does editing the note.
  vim.api.nvim_set_current_win(transcript_window)
  vim.cmd("TrailMixNoteJump")
  vim.api.nvim_buf_set_lines(note, -1, -1, false, { "- typed afterwards" })
  vim.cmd("TrailMixNoteJump")
  assert(vim.api.nvim_win_get_cursor(note_window)[1] == 4 and last_notice():find("transcript passage", 1, true), "cycle survived an edit")
  vim.api.nvim_buf_set_lines(note, -2, -1, false, {})

  -- A passage header line belongs to its passage; stepping back starts at the closest too.
  vim.api.nvim_set_current_win(transcript_window)
  vim.api.nvim_win_set_cursor(transcript_window, { line_of(transcript, "01:02–01:05 system"), 0 })
  vim.cmd("TrailMixNoteJumpBack")
  assert(vim.api.nvim_win_get_cursor(note_window)[1] == 4 and last_notice():find("7s before", 1, true) and last_notice():find("1 of 3", 1, true),
    "header line or back-start failed: " .. last_notice())

  -- The transcript's own header has no passage.
  vim.api.nvim_set_current_win(transcript_window)
  vim.api.nvim_win_set_cursor(transcript_window, { 1, 0 })
  vim.cmd("TrailMixNoteJump")
  assert(vim.api.nvim_get_current_win() == transcript_window and last_notice():find("place the cursor on a transcript passage", 1, true),
    "header jump not rejected: " .. last_notice())

  -- The forward jump explains itself from the transcript instead of a meeting ID error.
  vim.cmd("TrailMixTranscriptJump")
  assert(last_notice():find("already in the transcript", 1, true), "forward jump from transcript: " .. last_notice())

  -- A note without anchors, and a note not shown in this tab.
  vim.api.nvim_win_set_cursor(transcript_window, { line_of(transcript, "When is the review?"), 0 })
  local saved = vim.api.nvim_buf_get_lines(note, 0, -1, false)
  vim.api.nvim_buf_set_lines(note, 0, -1, false, { marker, "- untimed notes" })
  vim.cmd("TrailMixNoteJump")
  assert(vim.api.nvim_get_current_win() == transcript_window and last_notice():find("no timestamps", 1, true), "anchorless note: " .. last_notice())
  vim.api.nvim_buf_set_lines(note, 0, -1, false, saved)
  local scratch = vim.api.nvim_create_buf(true, true)
  vim.api.nvim_win_set_buf(note_window, scratch)
  vim.cmd("TrailMixNoteJump")
  assert(vim.api.nvim_get_current_win() == transcript_window and last_notice():find("open this meeting's note", 1, true), "hidden note: " .. last_notice())
  vim.api.nvim_win_set_buf(note_window, note)

  -- Stamping a passage inserts its start above the note's cursor; focus stays.
  vim.api.nvim_buf_set_lines(note, 0, -1, false, { marker, "- first", "- second" })
  vim.api.nvim_win_set_cursor(note_window, { 3, 2 })
  vim.api.nvim_win_set_cursor(transcript_window, { line_of(transcript, "Friday is tentative."), 0 })
  local before = text(transcript)
  -- A script's edits share one undo block; a keypress would start a new one.
  vim.api.nvim_buf_call(note, function() vim.o.undolevels = vim.o.undolevels end)
  vim.cmd("TrailMixAddTimestamp")
  assert(vim.api.nvim_get_current_win() == transcript_window, "stamp moved focus")
  assert(vim.deep_equal(vim.api.nvim_buf_get_lines(note, 0, -1, false), { marker, "- first", "@01:02", "- second" }), "stamp: " .. text(note))
  assert(vim.deep_equal(vim.api.nvim_win_get_cursor(note_window), { 4, 2 }), "note cursor did not move past the stamp")
  assert(text(transcript) == before, "stamp changed the transcript")
  vim.api.nvim_buf_call(note, function() vim.cmd("silent undo") end)
  assert(vim.deep_equal(vim.api.nvim_buf_get_lines(note, 0, -1, false), { marker, "- first", "- second" }), "stamp is not one undo step")

  -- A visual selection adds a quote of the selected transcript text only.
  vim.api.nvim_win_set_cursor(note_window, { 3, 0 })
  vim.api.nvim_win_set_cursor(transcript_window, { line_of(transcript, "When is the review?"), 0 })
  keys("Vjjj mt")
  assert(vim.fn.mode() == "n", "stamp left visual mode active")
  assert(vim.deep_equal(vim.api.nvim_buf_get_lines(note, 0, -1, false), {
    marker, "- first", "@00:58", "> When is the review?", ">", "> Friday is tentative.", "- second",
  }), "quote: " .. text(note))
  assert(vim.api.nvim_win_get_cursor(note_window)[1] == 7, "note cursor did not move past the quote")
  -- A characterwise selection quotes only the selected characters.
  local row = line_of(transcript, "Friday is tentative.")
  vim.api.nvim_win_set_cursor(transcript_window, { row, 0 })
  keys("ve mt")
  assert(vim.api.nvim_buf_get_lines(note, 6, 8, false)[1] == "@01:02" and vim.api.nvim_buf_get_lines(note, 7, 8, false)[1] == "> Friday",
    "characterwise quote: " .. text(note))

  -- The stamped note is still a valid note for the forward jump.
  vim.api.nvim_set_current_win(note_window)
  vim.api.nvim_win_set_cursor(note_window, { 4, 0 })
  vim.cmd("TrailMixTranscriptJump")
  wait_for(function() return vim.api.nvim_get_current_line():find("00:58–01:03 microphone", 1, true) end, "stamped anchor did not jump")

  -- A read-only note is never edited.
  vim.bo[note].modifiable = false
  local frozen = text(note)
  vim.api.nvim_win_set_cursor(transcript_window, { line_of(transcript, "Friday is tentative."), 0 })
  vim.cmd("TrailMixAddTimestamp")
  assert(text(note) == frozen and last_notice():find("not modifiable", 1, true), "read-only note edited: " .. last_notice())
  vim.bo[note].modifiable = true
end, debug.traceback)

vim.fn.delete(directory, "rf")
if not ok then error(err) end
print("TrailMix note jump checks passed")
vim.cmd("qa!")
