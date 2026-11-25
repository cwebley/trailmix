-- Transcript-origin questions use the associated note, never the transcript as
-- a note snapshot. All fixtures are local; no assistant or capture is started.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
local directory = vim.fn.tempname()
vim.fn.mkdir(directory, "p")
local cli = directory .. "/fake cli"
local id = "0123456789abcdef0123456789abcdef"
local other_id = "ffffffffffffffffffffffffffffffff"
local marker = "<!-- trailmix: meeting " .. id .. " -->"
local notices = {}
vim.notify = function(message) notices[#notices + 1] = message end
vim.fn.writefile({
  "#!/bin/sh",
  'test "$2" = --meeting && test "$3" = "' .. id .. '" || exit 2',
  'case "$1" in',
  'ask) cat > "' .. directory .. '/request.json"; printf "ask\\n" >> "' .. directory .. '/calls"; sleep 0.05; cat "' .. directory .. '/answers.json" ;;',
  'transcript) cat "' .. directory .. '/transcript.json" ;;',
  '*) exit 2 ;;',
  'esac',
}, cli)
vim.fn.setfperm(cli, "rwx------")
vim.fn.writefile({ vim.json.encode({ schema_version = 1, meeting_id = id, path = directory .. "/meeting.answers.md", markdown = marker .. "\n# AI answers\n\nReady.\n" }) }, directory .. "/answers.json")
require("trailmix").setup({ cli = cli, mapping = false, ask_mapping = "<F6>", transcript_refresh_ms = 100 })
local passages = {
  { id = "microphone-1", meeting_id = id, track = "microphone", start_seconds = 58, end_seconds = 61, text = "When is the review?" },
  { id = "system-1", meeting_id = id, track = "system", start_seconds = 62, end_seconds = 65, text = "Friday is tentative." },
  { id = "system-2", meeting_id = id, track = "system", start_seconds = 90, end_seconds = 95, text = "First paragraph.\n\nSecond paragraph." },
}
local function response()
  vim.fn.writefile({ vim.json.encode({ schema_version = 1, meeting_id = id, duration_seconds = 120, passages = passages,
    transcription = { meeting_id = id, state = "streaming", tracks = {
      microphone = { state = "streaming", provisional = { pending = "Maybe Monday" } }, system = { state = "complete" },
    } },
  }) }, directory .. "/transcript.json")
end
local function text(buffer) return table.concat(vim.api.nvim_buf_get_lines(buffer, 0, -1, false), "\n") end
local function wait_for(predicate, message) assert(vim.wait(3000, predicate, 10), message or "context request timed out") end
local function keys(sequence) vim.api.nvim_feedkeys(vim.api.nvim_replace_termcodes(sequence, true, false, true), "xt", false) end
local function row_of(buffer, needle)
  for row, line in ipairs(vim.api.nvim_buf_get_lines(buffer, 0, -1, false)) do if line == needle then return row end end
  error("missing line: " .. needle)
end
local function quote_preview()
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do
    if vim.api.nvim_win_get_config(window).relative ~= "" then return text(vim.api.nvim_win_get_buf(window)) end
  end
end
local function calls() return vim.fn.filereadable(directory .. "/calls") == 1 and #vim.fn.readfile(directory .. "/calls") or 0 end
local function request() return vim.json.decode(table.concat(vim.fn.readfile(directory .. "/request.json"), "\n")) end
local ok, err = xpcall(function()
  local note_path = directory .. "/meeting.md"
  vim.cmd("edit " .. vim.fn.fnameescape(note_path))
  local note, note_window = vim.api.nvim_get_current_buf(), vim.api.nvim_get_current_win()
  vim.api.nvim_buf_set_lines(note, 0, -1, false, { marker, "- personal concern about Friday" })
  vim.cmd("silent write")
  vim.api.nvim_buf_set_lines(note, 1, 2, false, { "- unsaved concern about Friday" })
  -- A newer meeting in another loaded buffer cannot supply the note context.
  local other = vim.api.nvim_create_buf(true, false)
  vim.api.nvim_buf_set_name(other, directory .. "/newer.md")
  vim.api.nvim_buf_set_lines(other, 0, -1, false, { "<!-- trailmix: meeting " .. other_id .. " -->", "wrong meeting" })
  response()
  vim.cmd("TrailMixTranscript")
  local transcript_window
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do if window ~= note_window then transcript_window = window end end
  local transcript = vim.api.nvim_win_get_buf(transcript_window)
  wait_for(function() return text(transcript):find("Friday is tentative.", 1, true) end)
  vim.api.nvim_set_current_win(transcript_window)
  local answer
  local function ask(sequence, expected_quote, question)
    local count = calls()
    vim.ui.input = function(options, callback)
      assert(options.default == "", "selection became a question")
      assert(quote_preview() == expected_quote, "wrong quote preview: " .. tostring(quote_preview()))
      callback(question or "Does this conflict with my notes?")
    end
    keys(sequence or "<F6>")
    wait_for(function() return calls() > count end)
    for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do
      if vim.b[vim.api.nvim_win_get_buf(window)].trailmix_kind == "answers" then answer = vim.api.nvim_win_get_buf(window) end
    end
    wait_for(function() return text(answer):find("Ready.", 1, true) and not text(answer):find("Working…", 1, true) end)
    assert(vim.api.nvim_get_current_win() == transcript_window, "ask stole transcript focus")
    assert(#vim.api.nvim_tabpage_list_wins(0) == 3, "quote preview leaked or answer split duplicated")
    local sent = request()
    assert(sent.notes:find("unsaved concern", 1, true) and not sent.notes:find("wrong meeting", 1, true), "wrong note snapshot")
    assert(vim.fn.resolve(sent.note_path) == vim.fn.resolve(note_path) and sent.selection_source == "transcript", "wrong selection source: " .. vim.inspect(sent))
    return sent
  end
  -- Normal mode uses the entire passage, even from its header or trailing gap.
  local row = row_of(transcript, "Friday is tentative.")
  vim.api.nvim_win_set_cursor(transcript_window, { row - 1, 0 })
  local sent = ask(nil, "Friday is tentative.")
  assert(sent.selection == "Friday is tentative." and vim.deep_equal(sent.selection_passage_ids, { "system-1" }))
  vim.api.nvim_win_set_cursor(transcript_window, { row + 1, 0 })
  ask(nil, "Friday is tentative.")
  -- Characterwise and reverse selections quote exactly the selected characters.
  vim.api.nvim_win_set_cursor(transcript_window, { row, 0 })
  sent = ask("ve<F6>", "Friday")
  assert(sent.selection == "Friday" and vim.deep_equal(sent.selection_passage_ids, { "system-1" }))
  vim.api.nvim_win_set_cursor(transcript_window, { row, 5 })
  ask("v5h<F6>", "Friday")
  -- Exclusive endpoints and block selections keep their actual selected text.
  vim.o.selection = "exclusive"
  vim.api.nvim_win_set_cursor(transcript_window, { row, 0 })
  ask("v6l<F6>", "Friday")
  vim.o.selection = "inclusive"
  vim.api.nvim_win_set_cursor(transcript_window, { row, 0 })
  ask("<C-v>5l<F6>", "Friday")
  -- Visual and Ex selections spanning passages omit headers and preserve IDs.
  local first = row_of(transcript, "When is the review?")
  vim.api.nvim_win_set_cursor(transcript_window, { first, 0 })
  sent = ask("V3j<F6>", "When is the review?\n\nFriday is tentative.")
  assert(vim.deep_equal(sent.selection_passage_ids, { "microphone-1", "system-1" }))
  local count = calls()
  vim.ui.input = function(options, callback)
    assert(options.default == "" and quote_preview() == "When is the review?\n\nFriday is tentative.")
    callback("Explain these two statements")
  end
  vim.cmd(first .. "," .. row .. "TrailMixAsk")
  wait_for(function() return calls() > count and not text(answer):find("Working…", 1, true) end)
  assert(vim.deep_equal(request().selection_passage_ids, { "microphone-1", "system-1" }))
  vim.api.nvim_win_set_cursor(transcript_window, { row_of(transcript, "First paragraph."), 0 })
  sent = ask(nil, "First paragraph.\n\nSecond paragraph.")
  assert(sent.selection == passages[3].text, "whole passage context dropped paragraph breaks")
  vim.api.nvim_win_set_cursor(transcript_window, { row_of(transcript, "First paragraph."), 0 })
  sent = ask("V2j<F6>", "First paragraph.\n\nSecond paragraph.")
  assert(sent.selection == passages[3].text, "selected quote dropped paragraph breaks")
  -- A pending prompt keeps its exact quote when late finals shift display rows.
  local delayed
  vim.ui.input = function(_, callback) delayed = callback end
  vim.api.nvim_win_set_cursor(transcript_window, { row, 0 })
  keys("ve<F6>")
  assert(quote_preview() == "Friday")
  table.insert(passages, 1, { id = "late-final", meeting_id = id, track = "system", start_seconds = 1, end_seconds = 2, text = "An earlier final" })
  response()
  wait_for(function() return text(transcript):find("An earlier final", 1, true) end)
  assert(quote_preview() == "Friday", "refresh changed the quoted selection")
  vim.api.nvim_buf_set_lines(note, -1, -1, false, { "- unsaved concern added during prompt" })
  count = calls()
  delayed("What did they mean?")
  wait_for(function() return calls() > count and not text(answer):find("Working…", 1, true) end)
  sent = request()
  assert(sent.selection == "Friday" and sent.notes:find("added during prompt", 1, true))
  assert(vim.deep_equal(sent.selection_passage_ids, { "system-1" }))
  -- Hidden notes still contribute unsaved text; there is no need to navigate.
  local scratch = vim.api.nvim_create_buf(true, true)
  vim.api.nvim_win_set_buf(note_window, scratch)
  vim.api.nvim_win_set_cursor(transcript_window, { row_of(transcript, "Friday is tentative."), 0 })
  ask(nil, "Friday is tentative.")
  vim.api.nvim_win_set_buf(note_window, note)
  -- Provisional speech and transcript UI rows cannot masquerade as evidence.
  count = calls()
  vim.ui.input = function() error("invalid selection opened a prompt") end
  vim.api.nvim_win_set_cursor(transcript_window, { row_of(transcript, "Maybe Monday"), 0 })
  keys("V<F6>")
  assert(notices[#notices]:find("finalized", 1, true))
  vim.api.nvim_win_set_cursor(transcript_window, { 1, 0 })
  vim.cmd("TrailMixAsk")
  assert(notices[#notices]:find("finalized", 1, true) and calls() == count)
  -- A changed association during an async prompt is rejected before submission.
  vim.api.nvim_win_set_cursor(transcript_window, { row_of(transcript, "Friday is tentative."), 0 })
  vim.ui.input = function(_, callback) delayed = callback end
  vim.cmd("TrailMixAsk")
  vim.api.nvim_buf_set_lines(note, 0, 1, false, { "<!-- trailmix: meeting " .. other_id .. " -->" })
  delayed("Wrong note")
  wait_for(function() return notices[#notices]:find("source changed", 1, true) end)
  assert(calls() == count and not quote_preview())
  vim.api.nvim_buf_set_lines(note, 0, 1, false, { marker })
  -- Ambiguous loaded notes should never silently pick one snapshot.
  local duplicate = vim.api.nvim_create_buf(true, false)
  vim.api.nvim_buf_set_name(duplicate, directory .. "/duplicate.md")
  vim.api.nvim_buf_set_lines(duplicate, 0, -1, false, { marker })
  vim.cmd("TrailMixAsk")
  assert(notices[#notices]:find("more than one loaded note", 1, true) and calls() == count)
  assert(vim.bo[note].modified and vim.fn.readfile(note_path)[2] == "- personal concern about Friday", "ask saved the note")
end, debug.traceback)
vim.fn.delete(directory, "rf")
if not ok then error(err) end
print("TrailMix ask context checks passed")
vim.cmd("qa!")
