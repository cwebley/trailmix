# TrailMix

Write the bits that matter.

TrailMix is a CLI-driven meeting-notes tool for Neovim on macOS 15+. It creates a dated Markdown note, starts native capture with hosted transcription, and opens the note through tmux. The Neovim companion inserts timestamps, shows a searchable live transcript, and asks questions through a configurable headless assistant. Answers and cited evidence share the right-hand window.

TrailMix is early software. Short controlled trials work end to end; long real calls have seen little use. Expect rough edges and file issues.

## Install

Requires macOS 15 or newer on Apple silicon or Intel.

### From source

Requires Go 1.25+ and Apple's Command Line Tools with the macOS 15 SDK. No FFmpeg or virtual audio driver is needed.

```sh
git clone https://github.com/cwebley/trailmix
cd trailmix
make install            # PREFIX defaults to ~/.local
trailmix version
trailmix devices
```

`make install` copies `trailmix` and its `TrailMixCapture.app` helper into `$PREFIX/lib/trailmix/` and links `$PREFIX/bin/trailmix` to it. The CLI finds the helper beside its resolved path, so keep the two together. `make uninstall` removes both. For development, `make build` writes `bin/trailmix` and `bin/TrailMixCapture.app`.

### From a release

Download `trailmix-VERSION-darwin-universal.tar.gz` from GitHub Releases, check it against its `.sha256`, and extract it somewhere permanent:

```sh
shasum -a 256 -c trailmix-VERSION-darwin-universal.tar.gz.sha256
tar -xzf trailmix-VERSION-darwin-universal.tar.gz -C ~/.local/lib
xattr -dr com.apple.quarantine ~/.local/lib/trailmix-VERSION-darwin-universal
ln -sf ~/.local/lib/trailmix-VERSION-darwin-universal/trailmix ~/.local/bin/trailmix
```

Release builds are ad-hoc signed, not notarized, so Gatekeeper blocks the downloaded helper until the quarantine attribute is removed. macOS asks for Microphone and Screen & System Audio Recording access on first capture, and may ask again after an upgrade replaces the helper.

### Check your setup

```sh
trailmix doctor
```

`doctor` checks macOS, the capture helper and its signature, Microphone and Screen & System Audio Recording access, settings, the notes folder and template, a pinned microphone, the saved OpenAI key, tmux, Neovim, the terminal command, focus, the assistant command, and the tmux recording indicator. Each line is ✓, `!` (works, but worth knowing), or ✗, with a fix where one applies. It exits nonzero if any check fails; `--json` returns the same results for scripts.

It never records, contacts OpenAI, or triggers a permission prompt. macOS attributes permissions to the app that launched TrailMix, so a hotkey launch can be treated differently from the terminal where you ran `doctor`. A saved key is not verified with OpenAI.

Examples below use `trailmix` on your PATH.

## Meeting workspace

Requires tmux and Neovim 0.11+. When no tmux client is attached, TrailMix opens a terminal with the configured `terminal.command`, which defaults to Terminal.app. TrailMix checks PATH, `/opt/homebrew/bin`, and `/usr/local/bin` for executables. Use an absolute TrailMix executable path in external hotkeys.

```sh
trailmix start    # New note, hosted transcription, tmux + Neovim, terminal focus
trailmix open     # Return to the active or latest meeting note; never capture
trailmix status   # Capture state, actual microphone, transcription health
trailmix stop     # Finalize capture; leave notes and focus alone
trailmix toggle   # Full start when idle; stop active capture otherwise
```

Recordings stop automatically after 3 hours, so a forgotten session cannot run overnight; `status` then shows a warning naming the limit. Low-level `record --max-duration 90m` sets a different limit. Repeated `start` reopens the active meeting. An active low-level recording without notes blocks `start`; `toggle` can still stop it. Capture continues if the editor closes, transcription fails, or navigation fails. A navigation error reports the note path and tmux attach target; retry with `open`, or finish with `stop`. Startup can remain `starting` while macOS asks for permission. Only `recording` confirms capture.

Notes default to `~/Documents/TrailMix/YYYY-MM-DD/HH-MM-SS.md` in local time. Collisions get a numeric suffix. Each daily `trailmix-YYYY-MM-DD` tmux session has one window per meeting, starting in the daily notes directory. Original dates and paths survive midnight and recorder restart. `open` recreates closed targets and restores the note if its Neovim window switched to another buffer, preserving unsaved text there.

TrailMix prefers the invoking tmux client's best available identity, captured before recorder startup, then the most recently active eligible attached client. When several clients share a pane, tmux cannot identify the invoking client from `TMUX_PANE` alone. A tmux binding can supply `TRAILMIX_TMUX_CLIENT` using `#{client_name}` to make that choice explicit. From an external hotkey, leave it unset. With no attached client, TrailMix runs the terminal command and waits for a client to attach. This slice uses the invoking tmux server or the default server from an external hotkey.

### Recording indicator in tmux

While capture is active, the recorder sets the tmux user option `@trailmix_rec` on the server it was launched from (the default server from an external hotkey) and redraws attached clients. Add this to your status line:

```tmux
set -g status-right "#{?@trailmix_rec,#(~/.local/bin/trailmix status --tmux) ,}%R "
set -g status-right-length 60
```

`trailmix status --tmux` prints `● HH:MM` with the start time, `○ …` while starting, and a yellow `!txt` when live transcription is degraded. It prints nothing when idle. The command only runs while the option is set. If the recorder dies without clearing the option, the next status-line refresh clears it. The option is harmless if your status line never reads it. Use an absolute path; tmux jobs may not share your shell's PATH.

macOS also shows its own orange microphone indicator in the menu bar while any app, including TrailMix, uses the microphone.

### Terminal and focus

`terminal.command` is an executable/argument array, never a shell string. `{tmux}`, `{target}`, and `{dir}` are replaced with the tmux executable, the attach target, and the note's directory. The default opens a Terminal.app window through `osascript`. Other terminals:

```sh
trailmix config set terminal.command '["alacritty", "--working-directory", "{dir}", "-e", "{tmux}", "attach-session", "-t", "{target}"]'
trailmix config set terminal.command '["kitty", "--directory", "{dir}", "{tmux}", "attach-session", "-t", "{target}"]'
trailmix config set terminal.command '["wezterm", "start", "--cwd", "{dir}", "--", "{tmux}", "attach-session", "-t", "{target}"]'
trailmix config set terminal.command '["open", "-na", "Ghostty", "--args", "-e", "{tmux}", "attach-session", "-t", "{target}"]'
```

After selecting the note, TrailMix brings forward the terminal hosting that tmux client, found through its process ancestry. `terminal.focus` chooses how:

| Value | Behavior |
| --- | --- |
| `auto` | Default. Aerospace when installed, otherwise `macos`. Falls back to `macos` when Aerospace cannot identify one exact window. |
| `aerospace` | Focus the terminal's exact Aerospace window, across workspaces. Ambiguous matches report an error rather than focusing a guessed window. |
| `macos` | Activate the terminal app. macOS raises that app's frontmost window, which may not be the meeting's window. |
| `none` | Select the note in tmux and leave focus alone. |

### Settings and templates

```sh
trailmix config
trailmix config set notes_root "$HOME/notes/meetings"
trailmix config set template "$HOME/notes/meeting-template.md"
trailmix config set template ''       # Use the built-in dated heading
trailmix config set microphone ''     # Use the system-default microphone
trailmix config set microphone 'ID'   # Pin an ID from trailmix devices
```

Settings use `$XDG_CONFIG_HOME/trailmix/settings.json`, falling back to `~/.config/trailmix/settings.json` if XDG is unset or relative. Credentials remain separate. CLI-set note and template paths become absolute immediately, including `~/` expansion. An unavailable pinned microphone is an error; TrailMix never substitutes another device or changes macOS audio settings. `record --mic ID` overrides the setting, and `record --mic ''` explicitly chooses the system default.

Templates are copied into new notes with only `{{date}}` and `{{time}}` substitutions. TrailMix adds `<!-- trailmix: meeting <stable-id> -->` to each note. Keep this marker when editing headings. The template is never opened as the meeting note or overwritten. Notes remain ordinary Markdown; recording and transcript metadata stays in TrailMix storage.

### Neovim companion

Add the plugin to your lazy.nvim configuration. The plugin and CLI come from the same repository; keep their versions in step.

```lua
return {
  {
    "cwebley/trailmix",
    lazy = false,
    opts = {
      cli = vim.fn.exepath("trailmix"), -- or an absolute path
      mapping = "<leader>mt", -- normal and insert mode; false disables the mapping
      ask_mapping = "<leader>ma", -- normal and visual mode; false disables it
      transcript_mapping = "<leader>mT", -- normal-mode toggle; false disables it
    },
  },
}
```

`:TrailMixAddTimestamp` inserts a standalone `@MM:SS` line above the cursor, or `@HH:MM:SS` after an hour, leaving the cursor on the original line. It calls `status` asynchronously and requires the same meeting to be actively recording. `elapsed_seconds` comes from capture's roughly one-second updates. Historical status, an unreachable recorder, or a capture clock older than five seconds is rejected.

Only one insertion request can be pending. Moving the cursor, editing, leaving the buffer/window, or changing the meeting ID cancels that request. Calls time out after four seconds. Closing and reopening Neovim works because the note carries its identity.

TrailMix launches Neovim with a private local control socket for `open`. The plugin supplies timestamp editing, transcript inspection, and meeting questions. No Neovim lifecycle command is needed. Your editor configuration and hotkeys are installed by you; this checkout does not modify them.

For a global hotkey, point any launcher (Aerospace `exec-and-forget`, skhd, Raycast, Hammerspoon) at `/absolute/path/to/trailmix toggle`, for example `~/.local/bin/trailmix`, expanded. Choose the key yourself. If the hotkey runs TrailMix through `tmux run-shell`, append `>/dev/null`: tmux otherwise shows the printed JSON status over the new note until you press `q`, while errors still appear. `stop` and the stopping half of `toggle` do not navigate or focus any window.

### Transcript split

From a meeting note:

```vim
:TrailMixTranscript
:TrailMixTranscriptToggle
:TrailMixTranscriptJump
```

`TrailMixTranscript` opens a read-only vertical split on the right and leaves focus in your notes. It resolves the note's embedded meeting ID, even when a newer recording exists. Repeating the command reuses the split. It also works after stopping capture or reopening Neovim.

In normal mode, `<leader>mT` toggles the current meeting's transcript open or closed, from the note, answer view, or transcript itself. The `T` is uppercase; `<leader>mt` still inserts a timestamp. If the right-hand window is showing answers, the toggle reuses that window for the transcript. `:TrailMixTranscriptToggle` does the same without a mapping. Set `transcript_mapping` in the plugin's `opts` to change or disable the binding.

The split shows finalized passages in capture order, with source labels and time ranges. Partial provider text appears in a dimmed `Transcribing…` section. Incomplete ranges and transcription errors remain visible. Refresh failures preserve the previous transcript and display a warning rather than replacing it with an empty result.

- Use normal Neovim window navigation, `/`, `n`, `N`, selection, and yank.
- A newly opened streaming transcript follows the latest text. Moving the cursor, scrolling back, searching, or selecting pauses following. Press `G` in the transcript to resume; a counted motion such as `10G` stays paused.
- Refreshes preserve the passage under the cursor and the viewport when earlier finals arrive late. Rendering waits until a search or visual selection finishes.
- `q` closes the transcript window. Closing the last view stops its polling. Moving it to another tab or splitting its window is supported.

`TrailMixTranscriptJump` uses the standalone timestamp on the current line, or the nearest preceding timestamp, and focuses the transcript near that time. Anchors inside backtick or tilde fences are ignored. With no preceding anchor, navigation starts at `00:00`; the note remains untimed for AI context. Invalid anchors and times beyond the recorded duration report an error. The view includes up to about ten seconds of preceding passage context when it fits. Passage ranges locate the conversation, not individual words.

For an exact finalized passage, use `:TrailMixTranscript PASSAGE_ID` from the note or its transcript. IDs printed by `trailmix transcript` remain valid after reopening Neovim. A missing ID reports an error rather than jumping to another passage.

The plugin reads local snapshots asynchronously, with one request at a time and a four-second timeout. It polls visible views every second, including stopped meetings so retry results appear. Hidden tabs do not make requests. Set `transcript_refresh_ms` in the plugin's `opts` to change the interval, with a minimum of 100 ms. No capture or provider request is started by viewing a transcript.

The editor uses this CLI interface, which is also available for scripts:

```sh
trailmix transcript --meeting MEETING_ID --json
```

It returns the verified `meeting_id`, `passages`, `transcription` status, and `duration_seconds` when session metadata exists. `--meeting` and `--output` are mutually exclusive. Omitting both retains the CLI's current-or-last-recording behavior. The JSON interface checks the association, recording, status, and passage identities without falling back to a newer meeting.

### Ask about a meeting

In a meeting note:

1. Select text in visual mode and press `<leader>ma`. A question prompt opens with the selection prefilled. Edit the question and press Enter. In normal mode, `<leader>ma` opens an empty prompt. `:TrailMixAsk` also works; an Ex range supplies its lines as context.
2. The answer view opens in the existing right-hand window, or creates one if needed, and shows `Working…`. Your note keeps focus. The request includes the question, selected text, an unsaved note snapshot, and the meeting's finalized transcript passages.
3. The completed question and answer append to `NAME.answers.md` beside `NAME.md`. The main note is neither saved nor edited by asking. Questions and agenda bullets trigger nothing until you explicitly ask.
4. Move into the answer window and press `gf` on a time-label citation such as `[12:14]`. Evidence opens in the same window. Backspace or `:TrailMixBack` returns to the answer and its reading position.

```vim
:TrailMixAsk
:TrailMixAnswers
:TrailMixAskCancel
:TrailMixFollowCitation
:TrailMixBack
```

`TrailMixAnswers` reopens saved answers without running an assistant. Owned `.answers.md` files opened with ordinary `:edit` also receive buffer-local citation and back mappings. `gf` keeps its normal behavior away from a TrailMix citation. `q` closes answer or transcript windows; it does not cancel an active question. Use `TrailMixAskCancel` to cancel. Exiting Neovim cancels pending commands. Closing a view, looking at evidence, or switching notes cannot make a late answer reopen a window or steal focus.

Answer views enable window-local word wrapping, including when reopened or restored after following a citation. This wraps the display without inserting line breaks in the saved Markdown or changing your note window's settings.

Answers are Markdown with links of the form `[12:14](trailmix://meetings/MEETING_ID/passages/PASSAGE_ID)`. The assistant emits passage tokens; TrailMix verifies them against the request's finalized evidence and renders their actual capture times. Unknown references fail without appending an answer. Answers with no transcript citations receive a visible `No transcript evidence cited` note. Valid references establish that evidence exists, not that it supports every claim; review the cited passage. The prompt preserves tentative notes, distinguishes agenda questions from established facts, and labels requested general explanations separately from meeting evidence.

#### Configure the assistant command

The default uses your existing OpenCode installation, provider configuration, and authentication:

```json
{
  "assistant": {
    "command": ["opencode", "run", "--format", "json", "--agent", "trailmix-ask"],
    "format": "opencode-json",
    "timeout_seconds": 180
  }
}
```

For OpenCode, TrailMix injects a process-local `trailmix-ask` agent with tools disabled and sharing disabled. It does not install or modify OpenCode configuration files. The subprocess runs in its private request directory with the full prompt on stdin, avoiding attachment-reader truncation. OpenCode's existing model/provider settings determine billing and subscription use. Its JSON error events are checked even when the command exits with code zero. The launcher checks PATH, then `~/.opencode/bin`, `/opt/homebrew/bin`, and `/usr/local/bin` for the default executable.

To use another headless CLI, point TrailMix at a command or wrapper that reads the composed prompt from stdin, returns only a Markdown answer on stdout, sends diagnostics to stderr, and exits nonzero on failure:

```sh
trailmix config set assistant.command '["/absolute/path/to/meeting-assistant"]'
trailmix config set assistant.format markdown
trailmix config set assistant.timeout_seconds 180
```

The command is an executable/argument array, never a shell-interpolated prompt. Executable names can resolve through PATH; executable paths must be absolute. The wrapper receives the citation instructions with the question and context. Tool permissions for custom commands are controlled by that command. Changing the command or appending model flags does not require a Neovim restart; settings are read per request.

Requests are bounded by the configured timeout, from 1 to 1,800 seconds. Cancellation terminates the runner's process group. Context above 2 MiB is rejected before invoking the assistant rather than silently truncated; the selected model may have a smaller context limit. Each output stream is bounded to 4 MiB. Failed requests retain input snapshots and diagnostics under the recording's `answers/REQUEST_ID/` directory. Concurrent CLI requests serialize their answer-file appends.

CLI equivalents:

```sh
trailmix ask --meeting MEETING_ID --question 'What did we decide about Friday?'
trailmix answers --meeting MEETING_ID
```

Without `--question`, `ask` reads one JSON object on stdin containing `question` and optional `notes`, `note_path`, and `selection`. It returns JSON with the saved answer Markdown and paths. The editor uses this form to include unsaved notes. Each request uses one meeting; cross-meeting search and MCP remain future work.

#### Files alongside your notes

```text
12-45-02.md
12-45-02.transcript.md
12-45-02.answers.md
```

The transcript sidecar is generated after stopping a transcribed workspace meeting, after retry, and before asking. It contains finalized passages, stable IDs, source labels, and incomplete-range status. It is a generated, replaceable export; edit your main note instead. Audio and original evidence remain in recording storage. An unrelated file or symlink at either sidecar path is rejected rather than overwritten.

For an older meeting, or to refresh the export explicitly:

```sh
trailmix transcript --meeting MEETING_ID --export
```

The main note and transcript sidecar can then be searched together by an external agent. The answers file is created on the first successful answer; subsequent answers append while preserving existing saved text.

## Low-level recording

```sh
# Select the system's current default microphone at recording start.
trailmix record

# Check startup, elapsed time, and whether both tracks receive audio.
trailmix status

# Stop capture and finalize the local files.
trailmix stop
```

`record` is the former low-level `start` command. It launches a detached Go process and returns. Closing the terminal leaves it recording. Run `status` or `stop` from any terminal using the same executable and storage configuration. A second `record` returns the current recording's status without opening another session. The background process exits after this recording ends.

`record` may initially report `starting` while macOS checks permissions. Only `recording` means ScreenCaptureKit has started. Check that both `tracks.microphone.frames` and `tracks.system.frames` increase while speaking and playing audio. `peak` is the latest buffer's linear amplitude, not a decibel measurement; `peak_max` is the maximum over the recording. Level measurement handles floating-point and integer PCM, including 16-bit Bluetooth input. An all-zero track can have increasing frame counts. Status warns if a track has received only silence after three seconds, but listen to both recordings too.

macOS may request access to **Microphone** and **Screen & System Audio Recording**. If capture fails, enable the relevant permission under **System Settings > Privacy & Security** for TrailMix Capture or the app macOS attributes the launch to, such as your terminal. Follow any macOS quit/reopen instruction, then start a new recording. The helper uses a ScreenCaptureKit display filter to capture system audio; it neither registers a video output nor saves screen images.

Choose a specific microphone by its ID from `devices`:

```sh
trailmix record --mic 'BuiltInMicrophoneDevice'
```

Connect Bluetooth headphones before listing devices. To use their microphone, select its listed ID. To listen through Bluetooth while using the Mac microphone, keep the Mac's ID and select your headphones as the meeting app's output. The recorder does not change your input or output settings.

To choose an output location:

```sh
trailmix record --output "$HOME/Desktop/trailmix-audio-test"
```

The directory must not exist, and its parent must exist. TrailMix refuses to reuse an existing directory.

## Streaming transcription

Save your OpenAI API key once:

```sh
trailmix auth
trailmix auth status
```

`auth` prompts for the key without displaying it. Run it again to replace the saved key. Empty input, Ctrl-C, or an interrupted prompt leaves existing credentials intact. The command requires an interactive terminal and accepts no key argument. `auth status` reports whether a key is saved and its file path, without showing the key. Saving a key does not verify provider access.

Credentials use the same XDG path rules on macOS and Linux:

- `$XDG_CONFIG_HOME/trailmix/credentials.json` when `XDG_CONFIG_HOME` is an absolute path.
- `~/.config/trailmix/credentials.json` otherwise, including when `XDG_CONFIG_HOME` is unset or relative.

TrailMix creates its credentials directory with `0700` permissions and the plaintext JSON file with `0600` permissions. Replacing the key writes a private temporary file and atomically replaces the old file. The path is independent of your checkout, executable location, working directory, and `TRAILMIX_HOME`.

Recording and retry read that saved credential directly, without loading `.env` or falling back to an exported `OPENAI_API_KEY`. If you used the earlier `.env` setup, run `trailmix auth` and paste the same key at the prompt. Existing `.env` files are neither imported nor deleted. The Go recorder keeps the key in memory and does not pass it to the native capture helper or write it into logs, metadata, or command arguments.

To remove the saved key:

```sh
trailmix auth logout
```

Logout removes the local credential, not the key in your OpenAI account. A recording already in progress retains its loaded key until it stops. Future recordings and retries need authentication again.

Then start a new recording:

```sh
trailmix record --transcribe --mic 'MICROPHONE_ID'
trailmix status
trailmix transcript
trailmix stop
trailmix transcript
```

Substitute a current microphone ID from `devices`, or omit `--mic` to use the configured or system-default input. Check for `recording` before speaking. `record` without `--transcribe` keeps the capture-only workflow. High-level `start` always enables hosted transcription.

`--transcribe` sends separate microphone and system streams to OpenAI's `gpt-live-transcribe`. The helper saves original CAF audio and a second, mono signed PCM16 queue at 24 kHz. Go reads that queue independently of capture. Network delays do not block the native audio callback. The queue uses about 346 MB per hour for two continuously delivered tracks, in addition to the original CAF files and timing journals.

The local energy detector closes a turn after 600 ms below RMS 0.008, or after 15 seconds of uninterrupted audio. Capture gaps also close turns. Audio is sent while the turn is open. Entirely below-threshold turns are cleared rather than turned into evidence. This is a simple detector to evaluate on the real headset and call audio; quiet speech can be missed. A trailing speech turn shorter than 100 ms gets provider-only zero padding. Its evidence range excludes that padding.

`transcript` prints finalized passages in capture order, followed by incomplete ranges and provisional text. Run it again to refresh. `status` includes per-track transcription health, incomplete ranges, submitted duration, estimated cost, and final counts. Source labels are `microphone` and `system`, not speaker names. Passage ranges identify submitted audio, not individual word timestamps.

On stop, the helper drains its resamplers and closes the local files. Go then allows up to 12 seconds for remaining network work. A stopped recording can have degraded transcription; check both states. Missing credentials, rejected sessions, disconnects, and finalization timeouts retain local audio. After an unexpected network failure, this slice requires an explicit retry rather than repeatedly reconnecting and resubmitting during the call. Planned session rollover uses the provider's expiry timestamp or a conservative 55-minute lifetime and preserves the meeting clock.

After stopping, retry unfinished ranges using the same app-local credentials:

```sh
trailmix retry
# Or choose a recording explicitly:
trailmix retry --output '/path/to/recording'
trailmix transcript --output '/path/to/recording'
```

Retry waits for exclusive access to the recording through locks, reporting an error if capture or another retry is active. It reconstructs the streaming queue from CAF and the original timing journals, checking that existing PCM bytes match before replacing it. Finalized passages are immutable and skipped on replay. Pending ranges can be recovered without changing existing evidence. Corrupted original audio or a truncated original capture journal needs investigation rather than a guessed reconstruction. Recordings made with the older capture-only binary lack the native lock file and are not supported by this retry command.

### Provider facts checked September 23, 2026

- The [Realtime transcription guide](https://developers.openai.com/api/docs/guides/realtime-transcription) recommends `gpt-live-transcribe`, a transcription session with `session.update`, 24 kHz PCM, and client-side turn commits. This model does not support server VAD, word timestamps, confidence scores, or speaker labels.
- The [server events reference](https://developers.openai.com/api/reference/resources/realtime/server-events) supplies committed item IDs and transcript events. Finals can arrive out of turn order. TrailMix binds commit acknowledgments to local turns, then reconciles finals by item ID within each connection.
- The [model page](https://developers.openai.com/api/docs/models/gpt-live-transcribe) lists $0.017 per input audio minute and rate limits for paid tiers. Two continuously submitted tracks estimate to $2.04 per meeting hour, before retry overhead. Local duration-based estimates and provider usage are retained; neither is a verified invoice total.
- The [Realtime lifecycle guide](https://developers.openai.com/api/docs/guides/realtime-conversations) gives a general 60-minute session limit. TrailMix also respects an earlier returned `expires_at`. Hosted session rollover remains untested.

## Local files and timing

By default, recordings live under `~/Library/Application Support/TrailMix/recordings/`. Every recording has its own directory, printed by the CLI.

| File | Contents |
| --- | --- |
| `microphone.caf` | Selected microphone's PCM audio in its native format. |
| `system.caf` | System PCM audio, requested at 48 kHz stereo. Includes unrelated playback too. |
| `microphone.timeline.jsonl` | Per-buffer microphone file-frame ranges and meeting-relative timestamps. |
| `system.timeline.jsonl` | Equivalent mapping for system audio. |
| `capture.json` | Actual microphone identity and monotonic capture origin. |
| `meeting.json` | Recorder-owned stable ID, also available for capture-only recordings. |
| `session.json` | Meeting identity and optional note association, recording state, actual microphone, track statistics, elapsed time, and any error. |
| `capture.log` | Native framework diagnostics. |
| `capture.lock` | Native process lock, held until capture finalization finishes. |
| `microphone.stream.pcm`, `system.stream.pcm` | Optional durable 24 kHz mono PCM16 provider input queues. |
| `microphone.stream.jsonl`, `system.stream.jsonl` | Published PCM byte ranges mapped to the meeting clock, including capture discontinuities. |
| `*.stream.error` | A native conversion or queue-write failure, cleared only after successful reconstruction. |
| `transcription/meeting.json` | The same meeting ID plus segmentation version. Legacy transcription IDs are retained. |
| `transcription/*.turn.json` | Durable turn ranges and locally detected silence. |
| `transcription/*.passage.json` | Immutable finalized evidence, source ranges, provider identities, usage, and latency measurements. |
| `transcription/status.json` | Latest transcription state, provisional text, incomplete ranges, and duration/cost estimates. |

CAF files appear when the first audio buffer arrives. Permission failures can leave a session record without audio files.

Storage also contains `meetings/<id>.json` associations and `latest-meeting.json`, independent of the latest low-level recording. `live: true` means status was obtained from the recorder socket; disk snapshots report `live: false`. A recording can be stopped with degraded transcription, so inspect capture and transcription state separately. Lifecycle commands share a workspace lock to prevent duplicate note and editor creation.

To listen after stopping, substitute the directory printed by the CLI:

```sh
afplay '/path/to/recording/microphone.caf'
afplay '/path/to/recording/system.caf'
```

Both tracks use the same capture origin. The helper converts sample timestamps from the stream's synchronization clock into host-clock time, then subtracts that origin. It does not timestamp audio by callback arrival time.

The CAF files concatenate received buffers. **Use the timeline files for alignment**, since the first samples can arrive at different offsets and a track can contain gaps. Each row includes `file_frame`, `frames`, `start_seconds`, `end_seconds`, `sample_rate`, and `discontinuity_seconds`. Discontinuities larger than 2 ms are counted; positive values denote gaps and negative values denote overlaps. Resampling maps output frames back through each captured buffer's timestamp, retaining drift between the audio sample clock and meeting clock. The converter compensates its filter delay and drains trailing samples before a gap or stop. No silence is inserted to conceal missing audio.

Status refreshes approximately once a second. A missing or stale track produces a warning after a few seconds; silence can also cause sparse system-audio delivery. A disconnected selected microphone or an audio-format change ends the recording with a failure rather than silently changing the file's format. Seamless device switching is not implemented. A stopped session's success means the helper finalized its files, not that both sides were intelligible.

If the Go recorder crashes, its closed control pipe asks the helper to stop. A later `status` labels an unfinished session `interrupted`. Completed writes remain on disk; automatic recovery or repair is not implemented.

## Try your meeting setup

For each normal trial, record 20–30 seconds, speak, play remote speech or other recognizable audio, then stop and listen to both tracks:

1. Mac microphone with Bluetooth headphones for playback.
2. Bluetooth microphone with Bluetooth headphones for playback.
3. Simultaneous local and remote speech. Check both tracks, microphone bleed, and timing.
4. Disconnect the headphones during capture. Check for a reported error, missing-track warning, or output-route change. Stop explicitly if capture continues.

Also start a recording, close the starting terminal, and stop it from another terminal. Check an existing output directory is rejected.

Record the headset model, microphone ID, audible quality, timing offsets, discontinuities, and any permission/device trouble. Native capture remains provisional until this works in a real call. FFmpeg plus BlackHole is the fallback if native capture proves unsuitable.

## Development checks

```sh
make test
go vet ./...
go mod verify
```

CI runs these on macOS 15 for every push and pull request. Pushing a `v*` tag runs `make dist` and publishes the universal tarball and checksum as a GitHub release.

Go tests exercise real subprocesses, pipes, and Unix sockets using a fake audio helper. Transcription tests use local WebSocket servers and real PCM queue files. They cover live deltas before commit, out-of-order and duplicate finals, connection-local item IDs, gaps, trailing speech, session rollover including idle periods, cancellation, disconnects, retry deduplication, unpublished PCM, and native queue failures. Recorder tests verify that missing credentials degrade transcription while both local tracks keep recording.

The workspace integration test uses a private tmux server, clean Neovim instances, fake native audio, and substituted terminal/focus commands. It checks concurrent starts, note paths containing spaces and shell metacharacters, unique meetings, buffer restoration with unsaved edits, editor closure, server restart, saved dates, pinned-microphone rejection, and capture preservation through editor/focus failures. It skips if tmux or Neovim is unavailable. `make test-neovim` runs isolated asynchronous timestamp checks with `-u NONE --noplugin -i NONE`.

A separate navigation regression test loads a fixture `init.lua` through real tmux and Neovim. It checks leader mappings with unset, empty, and custom `XDG_CONFIG_HOME` values, including a stale tmux server value. This catches configuration-loading failures that `-u NONE` tests cannot detect.

Transcript checks exercise the real CLI and two fresh isolated Neovim processes against one older meeting while a newer recording exists. Headless editor checks cover asynchronous updates, provisional text, scroll preservation after late finals, timestamp and passage jumps, fenced and invalid anchors, normal search/yank, visual selections, moved/duplicated windows, failed reads, timeouts, and polling cleanup. These tests use fixture data and never start capture or contact the transcription provider.

Ask checks cross the actual CLI and a fake headless assistant process. They cover unsaved input snapshots, validated citations, concurrent appends, preservation of source notes and existing files, OpenCode error events, output limits, timeouts, and cleanup of wrapper child processes. Headless Neovim checks exercise selection and prompt behavior, shared-window reuse, named answer files, cancellation, pending-buffer replacement, and citation/back navigation. An editor-to-CLI integration check follows a generated citation back to stored evidence without using a real model.

Auth tests use real pseudo-terminals to check hidden input, terminal restoration after success and cancellation, credential replacement, owner-only permissions, redacted status/errors, logout, and XDG fallback paths. Subprocess tests check that detached recording and retry use the same saved credentials from a different working directory. Tests use isolated config directories and do not read your saved key.

The Swift self-test writes and reads real synthetic PCM buffers, including 16-bit mono headset audio. It checks levels, capture gaps, refusal of existing files, 16/44.1/48 kHz resampling, final-sample filter flushing, and byte-identical queue reconstruction from CAF. Tests do not record the microphone, request capture permissions, or contact OpenAI.

`TRAILMIX_HOME` overrides local state and recording storage. `TRAILMIX_CAPTURE_HELPER` overrides the helper executable for development. Use an absolute, reasonably short home path because macOS Unix socket paths must fit in 104 bytes.

## Implementation

- `cmd/trailmix/`: Go command entry point.
- `internal/recorder/`: CLI lifecycle, tmux recording indicator, unique meeting notes and associations, detached recorder, Unix-socket control, and native process supervision.
- `internal/meeting/`: recorder-owned identity, legacy ID preservation, and atomic JSON persistence.
- `internal/settings/`: XDG workspace preferences and CLI configuration.
- `internal/navigation/`: tmux targets, Neovim buffer restoration, configured terminal launch, and Aerospace or app focus.
- `lua/trailmix/`: Neovim companion plugin.
- `tests/nvim/`: isolated headless Neovim checks.
- `native/Capture.swift`: ScreenCaptureKit microphone/system capture, PCM files, and timing journals.
- `internal/transcription/`: queue reading, local turn segmentation, OpenAI WebSocket sessions, transcript persistence, and retry.
- `internal/auth/`: hidden CLI credential entry and XDG credential storage.
- `internal/assistant/`: bounded headless command execution and OpenCode event decoding.

The local socket carries status and stop commands. Go starts the helper with a pipe on stdin; closing it requests stop. The helper emits newline-delimited JSON lifecycle/statistics events on stdout and saves audio locally. The transcription process reads published PCM/timing files rather than putting audio or network backpressure on that control pipe. Go uses `github.com/gorilla/websocket` for the provider connection and `golang.org/x/term` for hidden credential input. Auth tests use `github.com/creack/pty`.
