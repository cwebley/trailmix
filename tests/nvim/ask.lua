-- Headless editor contract tests; no real assistant, user config, or recording.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
local directory = vim.fn.tempname()
vim.fn.mkdir(directory, "p")
local cli = directory .. "/fake cli"
local id = "0123456789abcdef0123456789abcdef"
local marker = "<!-- trailmix: meeting " .. id .. " -->"
local note_path = directory .. "/meeting.md"
local notices = {}
vim.notify = function(message) notices[#notices + 1] = message end
vim.fn.writefile({
  "#!/bin/sh",
  'test "$2" = --meeting && test "$3" = "' .. id .. '" || exit 2',
  'case "$1" in',
  'ask) cat > "' .. directory .. '/request.json"; sleep "${TRAILMIX_ASK_DELAY:-0.1}"; cat "' .. directory .. '/answers.json"; exit "${TRAILMIX_ASK_EXIT:-0}" ;;',
  'answers) cat "' .. directory .. '/answers.json" ;;',
  'transcript) cat "' .. directory .. '/transcript.json" ;;',
  '*) exit 2 ;;',
  'esac',
}, cli)
vim.fn.setfperm(cli, "rwx------")
require("trailmix").setup({ cli = cli, mapping = false, ask_mapping = "<F6>", transcript_refresh_ms = 100 })
local citation = "[01:02](trailmix://meetings/" .. id .. "/passages/system-1)"
local answer_text = marker .. "\n# AI answers\n\nThe review is tentative. " .. citation .. "\n"
vim.fn.writefile({ vim.json.encode({ meeting_id = id, path = directory .. "/meeting.answers.md", answer_id = "answer-1", markdown = answer_text }) }, directory .. "/answers.json")
vim.fn.writefile({ vim.json.encode({ meeting_id = id, duration_seconds = 120,
  passages = { { id = "system-1", meeting_id = id, track = "system", start_seconds = 62, end_seconds = 65, text = "Friday is tentative." } },
  transcription = { meeting_id = id, state = "complete", tracks = { microphone = { state = "complete" }, system = { state = "complete" } } },
}) }, directory .. "/transcript.json")
local function wait_for(predicate, message)
  assert(vim.wait(3000, predicate, 10), message or "ask response timed out")
end
local function text(buffer)
  return table.concat(vim.api.nvim_buf_get_lines(buffer, 0, -1, false), "\n")
end
local function key(keys)
  vim.api.nvim_feedkeys(vim.api.nvim_replace_termcodes(keys, true, false, true), "xt", false)
end
local ok, err = xpcall(function()
  vim.cmd("edit " .. vim.fn.fnameescape(note_path))
  local note, note_window = vim.api.nvim_get_current_buf(), vim.api.nvim_get_current_win()
  vim.api.nvim_buf_set_lines(note, 0, -1, false, { marker, "# Notes", "- did we test feature A?", "- share the pitch" })
  vim.cmd("silent write")
  vim.api.nvim_buf_set_lines(note, 3, 4, false, { "- unsaved agenda item" })
  local original = text(note)
  -- Answer-only opening must wrap even when the user's note window is nowrap.
  -- Opening a transcript first used to hide this bug through inherited options.
  vim.wo[note_window].wrap, vim.wo[note_window].linebreak = false, false
  vim.cmd("TrailMixAnswers")
  local initial_answer_window
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do if window ~= note_window then initial_answer_window = window end end
  local initial_answer = vim.api.nvim_win_get_buf(initial_answer_window)
  wait_for(function() return text(initial_answer):find("The review is tentative.", 1, true) end)
  assert(vim.wo[initial_answer_window].wrap and vim.wo[initial_answer_window].linebreak, "answer view inherited nowrap")
  assert(not vim.wo[note_window].wrap, "answer wrapping changed note window options")
  vim.api.nvim_win_close(initial_answer_window, false)
  vim.api.nvim_buf_delete(initial_answer, { force = true })
  -- The default toggle opens without taking note focus, closes from either
  -- side, and cancels pending transcript reads without reopening the window.
  local toggle_key = (vim.g.mapleader or "\\") .. "mT"
  key(toggle_key)
  assert(#vim.api.nvim_tabpage_list_wins(0) == 2 and vim.api.nvim_get_current_win() == note_window, "toggle did not open from notes")
  key(toggle_key)
  vim.wait(80, function() return false end)
  assert(#vim.api.nvim_tabpage_list_wins(0) == 1, "toggle failed to close or a late read reopened it")
  key(toggle_key)
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do if window ~= note_window then vim.api.nvim_set_current_win(window) end end
  key(toggle_key)
  assert(#vim.api.nvim_tabpage_list_wins(0) == 1 and vim.api.nvim_get_current_win() == note_window, "toggle failed from transcript")
  require("trailmix").setup({ transcript_mapping = "<F7>" })
  assert(vim.fn.maparg("<leader>mT", "n") == "", "old toggle mapping was retained")
  key("<F7>")
  assert(#vim.api.nvim_tabpage_list_wins(0) == 2, "custom toggle mapping failed")
  require("trailmix").setup({ transcript_mapping = false })
  assert(vim.fn.maparg("<F7>", "n") == "", "toggle mapping could not be disabled")
  vim.cmd("TrailMixTranscriptToggle")
  assert(#vim.api.nvim_tabpage_list_wins(0) == 1, "toggle command failed without a keybind")
  require("trailmix").setup({ transcript_mapping = "<leader>mT" })
  assert(not vim.wo[note_window].wrap, "toggle changed note wrapping")
  -- Start with the transcript already open. Asking must reuse that window.
  vim.cmd("TrailMixTranscript")
  local right
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do if window ~= note_window then right = window end end
  wait_for(function() return text(vim.api.nvim_win_get_buf(right)):find("Friday is tentative.", 1, true) end)
  local prompt_options
  vim.ui.input = function(options, callback)
    prompt_options = options
    callback("What testing was confirmed?")
  end
  vim.api.nvim_win_set_cursor(note_window, { 3, 0 })
  key("V<F6>")
  wait_for(function() return vim.b[vim.api.nvim_win_get_buf(right)].trailmix_kind == "answers" end)
  local answer_buffer = vim.api.nvim_win_get_buf(right)
  assert(prompt_options.default == "- did we test feature A?", "visual selection did not prefill prompt")
  assert(#vim.api.nvim_tabpage_list_wins(0) == 2, "ask opened a third window")
  assert(text(answer_buffer):find("Working…", 1, true), "no pending answer indicator")
  assert(vim.api.nvim_get_current_win() == note_window, "ask stole focus")
  wait_for(function() return text(answer_buffer):find("The review is tentative.", 1, true) end)
  local sent = vim.json.decode(table.concat(vim.fn.readfile(directory .. "/request.json"), "\n"))
  assert(sent.selection == "- did we test feature A?" and sent.question == "What testing was confirmed?")
  assert(sent.notes:find("unsaved agenda item", 1, true) and vim.fn.resolve(sent.note_path) == vim.fn.resolve(note_path), "ask missed unsaved note context: " .. vim.inspect(sent))
  assert(text(note) == original and vim.bo[note].modified, "ask changed or saved source notes")
  assert(vim.api.nvim_get_current_win() == note_window, "completed answer stole focus")
  key(toggle_key)
  assert(#vim.api.nvim_tabpage_list_wins(0) == 2 and vim.b[vim.api.nvim_win_get_buf(right)].trailmix_kind == "transcript", "toggle did not reuse answer slot")
  vim.api.nvim_set_current_win(right)
  vim.cmd("TrailMixBack")
  assert(vim.api.nvim_win_get_buf(right) == answer_buffer, "toggle lost the answer view")

  -- Evidence uses the same window, and back restores answer and cursor.
  vim.api.nvim_set_current_win(right)
  local row = 4
  local line = vim.api.nvim_buf_get_lines(answer_buffer, row - 1, row, false)[1]
  vim.api.nvim_win_set_cursor(right, { row, assert(line:find("[01:02]", 1, true)) - 1 })
  local cursor = vim.api.nvim_win_get_cursor(right)
  key("gf")
  wait_for(function() return vim.api.nvim_get_current_line():find("01:02–01:05 system", 1, true) end)
  assert(#vim.api.nvim_tabpage_list_wins(0) == 2 and vim.api.nvim_get_current_win() == right, "citation created a third window")
  vim.wo[right].wrap, vim.wo[right].linebreak = false, false
  key("<BS>")
  assert(vim.api.nvim_win_get_buf(right) == answer_buffer and vim.deep_equal(vim.api.nvim_win_get_cursor(right), cursor), "back did not restore answer position")
  assert(vim.wo[right].wrap and vim.wo[right].linebreak, "back did not restore answer wrapping")

  -- Normal mode starts with an empty prompt. Dismissing it performs no work.
  vim.api.nvim_set_current_win(note_window)
  vim.ui.input = function(options, callback) assert(options.default == ""); callback(nil) end
  key("<F6>")
  vim.wait(30, function() return false end)
  assert(not text(answer_buffer):find("Working…", 1, true))
  vim.ui.input = function(options, callback) assert(options.default == "did", "characterwise selection was expanded"); callback(nil) end
  vim.api.nvim_win_set_cursor(note_window, { 3, 2 })
  key("v2l<F6>")
  vim.wait(30, function() return false end)

  -- A prompt callback for a different editing target cannot submit a request.
  local delayed
  vim.ui.input = function(_, callback) delayed = callback end
  vim.cmd("TrailMixAsk")
  vim.api.nvim_set_current_win(right)
  delayed("wrong target")
  wait_for(function() return notices[#notices] and notices[#notices]:find("source note changed", 1, true) end)
  assert(not text(answer_buffer):find("Working…", 1, true))

  -- A pending response can complete while evidence is visible without replacing
  -- it. Back then shows the completed answer; notes remain untouched.
  vim.api.nvim_set_current_win(note_window)
  vim.ui.input = function(_, callback) callback("Another question") end
  vim.env.TRAILMIX_ASK_DELAY = "0.25"
  vim.cmd("TrailMixAsk")
  wait_for(function() return text(answer_buffer):find("Working…", 1, true) end)
  vim.api.nvim_set_current_win(right)
  vim.api.nvim_win_set_cursor(right, cursor)
  key("gf")
  local transcript_buffer = vim.api.nvim_win_get_buf(right)
  wait_for(function() return not text(answer_buffer):find("Working…", 1, true) end)
  assert(vim.api.nvim_win_get_buf(right) == transcript_buffer, "response replaced evidence being read")
  vim.cmd("TrailMixBack")
  assert(vim.api.nvim_win_get_buf(right) == answer_buffer)

  -- Failure retains prior answers and cancellation does not publish success.
  vim.api.nvim_set_current_win(note_window)
  vim.env.TRAILMIX_ASK_EXIT = "1"
  vim.cmd("TrailMixAsk")
  wait_for(function() return text(answer_buffer):find("Ask failed", 1, true) end)
  assert(text(answer_buffer):find("The review is tentative.", 1, true))
  vim.env.TRAILMIX_ASK_EXIT, vim.env.TRAILMIX_ASK_DELAY = "0", "0.5"
  vim.cmd("TrailMixAsk")
  wait_for(function() return text(answer_buffer):find("Working…", 1, true) end)
  vim.cmd("TrailMixAskCancel")
  wait_for(function() return text(answer_buffer):find("request canceled", 1, true) end)
  assert(text(note) == original)

  -- Reading saved answers works after wiping the cached view, and untimestamped
  -- note sections navigate from zero rather than producing a missing-anchor error.
  vim.api.nvim_win_close(right, false)
  vim.api.nvim_buf_delete(answer_buffer, { force = true })
  vim.cmd("TrailMixAnswers")
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do if window ~= note_window then right = window end end
  wait_for(function() return text(vim.api.nvim_win_get_buf(right)):find("The review is tentative.", 1, true) end)
  vim.api.nvim_win_set_cursor(note_window, { 4, 0 })
  vim.cmd("TrailMixTranscriptJump")
  wait_for(function() return vim.api.nvim_get_current_line():find("01:02–01:05 system", 1, true) end)
  assert(#vim.api.nvim_tabpage_list_wins(0) == 2)

  -- Ordinary named answer files get the same citation/back behavior after a
  -- restart; a file buffer in the existing right slot must not create a third.
  local saved = directory .. "/saved.answers.md"
  vim.fn.writefile(vim.split("<!-- trailmix: answers " .. id .. " -->\n" .. answer_text, "\n", { plain = true }), saved)
  vim.wo[right].wrap, vim.wo[right].linebreak = false, false
  vim.cmd("edit " .. vim.fn.fnameescape(saved))
  local saved_buffer = vim.api.nvim_get_current_buf()
  local mapping = vim.fn.maparg("gf", "n", false, true)
  assert(mapping.buffer == 1, "saved answers did not acquire citation mapping")
  assert(vim.wo[right].wrap and vim.wo[right].linebreak, "saved answer file inherited nowrap")
  vim.api.nvim_win_set_cursor(right, { 5, #"The review is tentative. " })
  key("gf")
  wait_for(function() return vim.api.nvim_get_current_line():find("01:02–01:05 system", 1, true) end)
  assert(#vim.api.nvim_tabpage_list_wins(0) == 2)
  vim.cmd("TrailMixBack")
  assert(vim.api.nvim_get_current_buf() == saved_buffer)

  -- Reuse the slot after :enew without abandoning unrelated unsaved text.
  vim.cmd("enew")
  local unrelated = vim.api.nvim_get_current_buf()
  vim.api.nvim_buf_set_lines(unrelated, 0, -1, false, { "keep this unsaved text" })
  vim.api.nvim_set_current_win(note_window)
  vim.fn.writefile({ vim.json.encode({ meeting_id = id, path = directory .. "/meeting.answers.md", markdown = answer_text .. "\nRefreshed from disk\n" }) }, directory .. "/answers.json")
  vim.cmd("TrailMixAnswers")
  wait_for(function() return vim.b[vim.api.nvim_win_get_buf(right)].trailmix_kind == "answers" end)
  assert(#vim.api.nvim_tabpage_list_wins(0) == 2, "enew stranded the right slot")
  assert(text(unrelated) == "keep this unsaved text" and vim.bo[unrelated].modified)
  wait_for(function() return text(vim.api.nvim_win_get_buf(right)):find("Refreshed from disk",1,true) end)

  -- Wiping a pending answer view then reopening it must receive completion in
  -- the replacement buffer, not leave a blank scratch buffer behind.
  vim.env.TRAILMIX_ASK_DELAY = "0.4"
  vim.cmd("TrailMixAsk")
  answer_buffer = vim.api.nvim_win_get_buf(right)
  wait_for(function() return text(answer_buffer):find("Working…",1,true) end)
  vim.api.nvim_buf_delete(answer_buffer, { force = true })
  vim.cmd("TrailMixAnswers")
  for _, window in ipairs(vim.api.nvim_tabpage_list_wins(0)) do if window ~= note_window then right = window end end
  local replacement = vim.api.nvim_win_get_buf(right)
  assert(replacement ~= answer_buffer and text(replacement):find("Working…",1,true), "replacement lost pending state")
  wait_for(function() return not text(replacement):find("Working…",1,true) and text(replacement):find("The review is tentative.",1,true) end)

  -- Back follows the clicked answer, not whichever meeting occupied the
  -- destination slot when two saved answer documents are visible.
  local other = directory .. "/other.answers.md"
  local other_id = "ffffffffffffffffffffffffffffffff"
  vim.fn.writefile(vim.split(("<!-- trailmix: answers " .. id .. " -->\n" .. answer_text):gsub(id, other_id), "\n", { plain = true }), other)
  vim.api.nvim_win_call(right, function() vim.cmd("edit " .. vim.fn.fnameescape(other)) end)
  vim.api.nvim_set_current_win(note_window)
  vim.cmd("hide edit " .. vim.fn.fnameescape(saved))
  local source_answer = vim.api.nvim_get_current_buf()
  vim.api.nvim_win_set_cursor(note_window, { 5, #"The review is tentative. " })
  local source_cursor = vim.api.nvim_win_get_cursor(note_window)
  vim.cmd("TrailMixFollowCitation")
  wait_for(function() return vim.api.nvim_get_current_line():find("01:02–01:05 system",1,true) end)
  assert(vim.api.nvim_get_current_win() == right, "citation did not use the established right-hand slot")
  vim.cmd("TrailMixBack")
  assert(vim.api.nvim_get_current_buf() == source_answer and vim.deep_equal(vim.api.nvim_win_get_cursor(0), source_cursor), "back returned to the destination's old answer")
  assert(text(note) == original)
end, debug.traceback)
vim.fn.delete(directory, "rf")
if not ok then error(err) end
print("TrailMix ask checks passed")
vim.cmd("qa!")
