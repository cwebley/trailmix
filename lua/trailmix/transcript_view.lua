local M = {}

function M.clock(seconds)
  seconds = math.floor(seconds)
  if seconds >= 3600 then
    return string.format("%02d:%02d:%02d", math.floor(seconds / 3600), math.floor(seconds / 60) % 60, seconds % 60)
  end
  return string.format("%02d:%02d", math.floor(seconds / 60), seconds % 60)
end

local function number(value)
  return type(value) == "number" and value >= 0 and value < math.huge
end

-- Validate before touching the buffer, so a failed refresh retains the last
-- usable transcript instead of displaying another meeting or partial garbage.
function M.validate(snapshot, id)
  assert(type(snapshot) == "table" and snapshot.meeting_id == id, "transcript meeting identity mismatch")
  local state = snapshot.transcription
  assert(type(state) == "table" and state.meeting_id == id and type(state.state) == "string" and type(state.tracks) == "table", "invalid transcription status")
  assert(type(snapshot.passages) == "table" and vim.islist(snapshot.passages), "invalid passages")
  if snapshot.duration_seconds ~= nil then assert(number(snapshot.duration_seconds), "invalid meeting duration") end
  local seen = {}
  for _, p in ipairs(snapshot.passages) do
    assert(type(p) == "table" and p.meeting_id == id and type(p.id) == "string" and p.id ~= "" and not seen[p.id], "invalid passage identity")
    assert((p.track == "microphone" or p.track == "system") and type(p.text) == "string"
      and number(p.start_seconds) and number(p.end_seconds) and p.end_seconds >= p.start_seconds, "invalid passage")
    seen[p.id] = true
  end
  for _, name in ipairs({ "microphone", "system" }) do
    local track = state.tracks[name]
    assert(type(track) == "table" and type(track.state) == "string", "invalid track status")
    assert(track.error == nil or type(track.error) == "string", "invalid track error")
    for _, gap in ipairs(track.incomplete_ranges or {}) do
      assert(type(gap.id) == "string" and number(gap.start_seconds) and number(gap.end_seconds), "invalid incomplete range")
    end
    for key, text in pairs(track.provisional or {}) do
      assert(type(key) == "string" and type(text) == "string", "invalid provisional text")
    end
  end
end

function M.render(id, snapshot, following, failure)
  local result = { lines = {}, keys = {}, highlights = {}, passages = {} }
  local function add(key, text, highlight)
    for index, line in ipairs(vim.split(text, "\n", { plain = true })) do
      local row = #result.lines + 1
      result.lines[row], result.keys[row] = line, key .. ":" .. index
      if highlight then result.highlights[#result.highlights + 1] = { row = row, group = highlight } end
    end
  end
  local state = snapshot and snapshot.transcription
  add("title", "Transcript · " .. (state and state.state or (failure and "unavailable" or "loading")) .. " · " .. (following and "following latest" or "paused"), "Title")
  add("meeting", "Meeting " .. id, "Comment")
  add("help", "G: follow latest   q: close   /: search", "Comment")
  if failure then add("failure", "Refresh failed: " .. failure .. (snapshot and " · showing last snapshot" or ""), "WarningMsg") end
  add("header-gap", "")
  if not snapshot then return result end
  local finals = {}
  local passages = vim.list_extend({}, snapshot.passages)
  table.sort(passages, function(a, b)
    if a.start_seconds == b.start_seconds then return a.id < b.id end
    return a.start_seconds < b.start_seconds
  end)
  for _, p in ipairs(passages) do
    finals[p.id] = true
    result.passages[#result.passages + 1] = { id = p.id, row = #result.lines + 1, start = p.start_seconds, finish = p.end_seconds }
    add("passage:" .. p.id, string.format("[%s–%s %s]", M.clock(p.start_seconds), M.clock(p.end_seconds), p.track), "Identifier")
    add("text:" .. p.id, p.text ~= "" and p.text or "(empty final)")
    add("gap:" .. p.id, "")
  end
  if #passages == 0 then add("empty", "No finalized passages yet.", "Comment") end
  for _, name in ipairs({ "microphone", "system" }) do
    local track = state.tracks[name]
    if track.error and track.error ~= "" then add("error:" .. name, name .. " · " .. track.state .. ": " .. track.error, "WarningMsg") end
    for _, gap in ipairs(track.incomplete_ranges or {}) do
      if not finals[gap.id] then
        add("incomplete:" .. gap.id, string.format("[incomplete %s–%s %s]", M.clock(gap.start_seconds), M.clock(gap.end_seconds), name), "WarningMsg")
      end
    end
    for _, key in ipairs(vim.fn.sort(vim.tbl_keys(track.provisional or {}))) do
      add("provisional-heading:" .. name .. key, name .. " · Transcribing…", "Comment")
      add("provisional:" .. name .. key, track.provisional[key], "Comment")
      add("provisional-gap:" .. name .. key, "")
    end
  end
  return result
end

return M
