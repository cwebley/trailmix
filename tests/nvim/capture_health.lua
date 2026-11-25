-- Run with nvim --headless -u NONE --noplugin -i NONE -l tests/nvim/capture_health.lua.
vim.opt.runtimepath:prepend(vim.fn.getcwd())
local view = require("trailmix.transcript_view")
local id = "0123456789abcdef0123456789abcdef"
local snapshot = {
  meeting_id = id, passages = {}, transcription = {
    meeting_id = id, state = "complete", tracks = { microphone = { state = "complete" }, system = { state = "complete" } },
  },
  capture = {
    state = "failed", error = "No display available", last_captured_seconds = 64, last_captured_at = "2026-11-18T12:01:04Z",
    tracks = { microphone = { last_captured_seconds = 64 }, system = { last_captured_seconds = 61 } },
    warnings = { "system: no recent audio buffers" },
  },
}
view.validate(snapshot, id)
local rendered = view.render(id, snapshot, false)
local text = table.concat(rendered.lines, "\n")
for _, expected in ipairs({ "Capture: failed · transcription: complete", "Capture error: No display available", "Last observed audio: 01:04", "2026-11-18T12:01:04Z", "microphone last observed audio: 01:04", "system last observed audio: 01:01", "Capture warning: system: no recent audio buffers" }) do
  assert(text:find(expected, 1, true), "missing health display: " .. expected)
end
local warning = false
for _, hl in ipairs(rendered.highlights) do
  if rendered.lines[hl.row]:find("Capture: failed", 1, true) then warning = hl.group == "WarningMsg" end
end
assert(warning, "capture failure was not highlighted")
snapshot.capture.last_captured_seconds = "64"
assert(not pcall(view.validate, snapshot, id), "invalid capture boundary accepted")
snapshot.capture.last_captured_seconds = 64
snapshot.capture.state, snapshot.capture.error, snapshot.capture.warnings = "stopped", nil, nil
text = table.concat(view.render(id, snapshot, false).lines, "\n")
assert(text:find("Capture: stopped", 1, true) and not text:find("Capture error", 1, true) and not text:find("uncaptured", 1, true), "intentional stop presented as capture loss")
snapshot.capture = nil
view.validate(snapshot, id)
assert(table.concat(view.render(id, snapshot, false).lines, "\n"):find("complete", 1, true), "older snapshots lost compatibility")
print("capture health display tests passed")
