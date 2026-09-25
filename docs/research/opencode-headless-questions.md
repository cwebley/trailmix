# OpenCode headless questions

Checked 2026-09-24 against official docs and the local OpenCode checkout at `6e7c9eb820a2dd85724bcc8b4ad521776f937bca`. Relevant source files were clean. Commands below were verified by reading docs/source, not executed. No model requests, credential inspection, authentication, or configuration changes were made. Installed-binary behavior was not tested.

## Conclusion

Yes. `:TrailMixAsk` can launch one `opencode run` subprocess, supply a question and meeting context, capture its answer, and save/open Markdown in Neovim. It reuses OpenCode's provider setup. No separate OpenRouter client, interactive terminal pane, MCP integration, or managed server is necessary. `--attach` is an optional startup optimization. The checked implementation uses an in-process server internally. Omitting `--continue` and `--session` creates a fresh session; OpenCode still stores session history, so this is not an ephemeral/no-storage mode. [1][6]

## Command and input

Exact command shape is `opencode run [message..]`. Example with illustrative paths and existing note/transcript files:

```sh
opencode run \
  --dir "/absolute/path/to/meeting" \
  --format default \
  --file "notes.md" \
  --file "transcript.txt" \
  -- "Using the attached notes and transcript, what decisions and follow-ups were agreed? Answer in Markdown." \
  </dev/null > "/absolute/path/to/meeting/answer.md"
```

- `--file` / `-f` is an array option. Repeat it as above or supply multiple paths. An unseparated question after `-f` can be consumed as another filename. The `--` boundary avoids that: the source explicitly includes arguments after it in the message. All options must precede the boundary. [1][6][7]
- Without `--attach`, `--dir` changes cwd before relative attachments resolve. It also selects the project context. Use absolute paths when convenient. Shell output redirection resolves in the caller's cwd, which is why the output path above is absolute. [6]
- Non-TTY stdin is appended to the prompt, and OpenCode waits for EOF. A Neovim subprocess must close stdin after writing, or use closed/empty stdin as above. This also permits sending a composed question + notes + transcript through stdin instead of temporary attachments. [6]
- Important for meetings: attached text uses the Read tool's input-expansion path. In this checkout, defaults cap reads at 2,000 lines and 50 KiB, with individual lines truncated at 2,000 characters. Explicit attachments expand even when model tool calls are denied. A no-tools agent cannot subsequently retrieve omitted text. Supplying full text through stdin avoids these file-reader caps, though model context limits still apply. [9][10]

## Output and minimal wrapper

`--format default` is formatted output, not a flag named `text`. In the checked source, redirected/non-TTY stdout receives completed assistant text blocks; tool displays, headings, and warnings go to stderr. Capture the two streams separately. It can emit several text blocks, not just a guaranteed final answer. [6][8]

`--format json` emits newline-delimited event objects, not one JSON document or a final-answer object. Events include `text`, `tool_use`, `step_start`, `step_finish`, and `error`; answer text is in `part.text` on `text` events. These are completed text parts, not token deltas. A wrapper can collect text events and report error events. The checked run handler does not turn every `session.error` into a nonzero exit, so exit status alone is insufficient. [1][6]

Simplest design: an asynchronous argv-based subprocess, explicit cwd, closed stdin, separate output/error capture, then save/open the answer. Plain stdout is the smallest first version; JSON adds structured error handling with a small line parser. No persistent session ID is needed.

## Provider, model, and permissions

- Existing provider credentials and configuration are reused under the same user/environment. `--model provider/model` / `-m` selects explicitly; otherwise OpenCode applies its configured/default model selection, including agent-specific models. Headless invocation does not itself choose a new billing route. Supported subscription logins, such as ChatGPT Plus/Pro or GitHub Copilot, use that subscription's access and limits; API-key routes retain their API billing. Do not promise that an arbitrary subscription covers an API-key request. Current Anthropic docs say bundled Claude Pro/Max support was removed as of 1.3.0. [1][2][3][4]
- Headless does not mean tool-free. Build is the documented default and most permissions allow actions. In the checked source, requests requiring approval are automatically rejected in ordinary `run`; allowed tools still execute. Built-in `plan` is not a strict no-tools/read-only guarantee: the checkout permits plan-file edits and inherits general tool defaults, differing from the docs' description of bash/edit approval. [4][5][6][11]
- The documented mechanism for a strict Q&A agent is a custom **primary** agent selected with `--agent trailmix-ask`, with agent-level `permission: {"*":"deny"}`. For read-only retrieval, start deny-all and selectively allow `read`, `glob`, and `grep`. Agent permissions override global ones. An unknown agent or a subagent passed to `run --agent` falls back to the default agent in this checkout, so the intended agent must exist. [4][5][6]

Illustrative configuration only, not installed or changed during this research:

```json
{
  "agent": {
    "trailmix-ask": {
      "description": "Answer questions from supplied meeting context",
      "mode": "primary",
      "prompt": "Answer from the supplied notes and transcript. Return Markdown and identify missing information.",
      "permission": { "*": "deny" }
    }
  }
}
```

Once defined, add `--agent trailmix-ask` before `--` in the example. This leaves output-file creation to TrailMix. One version difference worth preserving: current docs call auto-approval `--auto`; this checkout names it `--dangerously-skip-permissions`. Neither is needed here. [1][6]

## Sources

1. [Official CLI reference, run and auth](https://opencode.ai/docs/cli/#run-1)
2. [Official providers, OpenAI subscription/API-key choices](https://opencode.ai/docs/providers/#openai), [GitHub Copilot](https://opencode.ai/docs/providers/#github-copilot), [Anthropic](https://opencode.ai/docs/providers/#anthropic)
3. [Official model selection](https://opencode.ai/docs/models/#loading-models)
4. [Official agents and per-agent permissions](https://opencode.ai/docs/agents/#permissions)
5. [Official permissions](https://opencode.ai/docs/permissions/)
6. [Checked run implementation](https://github.com/anomalyco/opencode/blob/6e7c9eb820a2dd85724bcc8b4ad521776f937bca/packages/opencode/src/cli/cmd/run.ts), especially lines 206–347, 359–395, 433–563, 568–675
7. [CLI parser configuration](https://github.com/anomalyco/opencode/blob/6e7c9eb820a2dd85724bcc8b4ad521776f937bca/packages/opencode/src/index.ts#L69-L70)
8. [UI output goes to stderr](https://github.com/anomalyco/opencode/blob/6e7c9eb820a2dd85724bcc8b4ad521776f937bca/packages/opencode/src/cli/ui.ts#L32-L40)
9. [Text attachment expansion](https://github.com/anomalyco/opencode/blob/6e7c9eb820a2dd85724bcc8b4ad521776f937bca/packages/opencode/src/session/prompt.ts#L1075-L1174)
10. [Read limits](https://github.com/anomalyco/opencode/blob/6e7c9eb820a2dd85724bcc8b4ad521776f937bca/packages/opencode/src/tool/read.ts)
11. [Built-in agent defaults](https://github.com/anomalyco/opencode/blob/6e7c9eb820a2dd85724bcc8b4ad521776f937bca/packages/opencode/src/agent/agent.ts#L90-L149)
