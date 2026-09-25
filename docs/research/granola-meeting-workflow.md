# Granola meeting workflow research

Research date: 2026-09-24, using the session date. Sources are official pages retrieved through webfetch. This describes what those pages currently say, not a verified release history or hands-on app test. The chat announcement displays "April 21" without a year in the fetched text.

## Main finding

Granola preserves editable human notes alongside editable AI-enhanced notes and offers chat over meeting data. A human/enhanced distinction therefore matches Granola conceptually. Separate local documents, independent editing rules, and three simultaneously visible Neovim splits would be TrailMix design choices. Granola documents returning to raw notes through a control and showing or hiding the transcript panel. [1][2]

## Notes, transcripts, and editing

- When Granola detects the call has ended, or the user stops it manually, it automatically generates enhanced notes from the transcript, raw notes, and calendar-event information. Raw-note contributions appear in black in the enhanced view. [1]
- Raw notes survive. Users can return to them using the three-horizontal-lines control, edit them, and regenerate the enhanced notes. Users can also edit enhanced notes directly, change templates, regenerate, or ask chat for revisions. Edits to one meeting do not teach the generator a style for future meetings. [1]
- The live transcript is a show/hide panel. It supports search, copying the whole transcript or individual chunks, and deleting chunks. TrailMix's proposed read-only transcript is therefore its own constraint, not an exact copy of Granola. The page does not document arbitrary transcript-text editing. [2]
- Unknown: how direct edits to enhanced notes survive regeneration, whether changes propagate back to raw notes, and whether the two views correspond to independently stored documents. The docs establish editable views/content, not a local-file model. [1]

## Chat, cross-meeting recall, and post-meeting outputs

- Chat works before, during, and after a meeting. Its scope can be one meeting, all meetings from the homepage, a folder, or explicitly selected meetings. The docs also describe People/Companies contexts on desktop. Current documented history limits are 30 days on Basic and full history on Business/Enterprise. [3]
- Cross-meeting recall is explicit: finding information when the user cannot remember which meeting contained it, tracking recurring themes, and summarizing across meetings. This supports the product idea of natural-language or "fuzzy" recall. It does not establish a particular fuzzy-matching algorithm, embedding index, or exhaustive retrieval guarantee. [3][4]
- Chat can rewrite notes or selected sections. Ordinary Q&A and editing are distinguishable actions; the docs say language such as "rewrite" triggers rewriting. [3]
- Answers have a home in private chat history. Threads can be deleted, and sharing a meeting note does not share its chat conversations. The docs do not specify the history retention duration or promise that every answer automatically becomes a separate saved document. [3]
- Post-meeting outputs include enhanced notes, follow-up actions, and chat-generated writing. Granola advertises drafting emails, briefs, product specs, proposals, and project plans. These are supported output types, not evidence that every meeting automatically produces a bundle of separate artifacts. [1][3][5]

## What citations actually target

| Context | Explicitly documented interaction and evidence | Not established by these sources |
| --- | --- | --- |
| Enhanced notes | A magnifying glass beside each note lets the user investigate where the point came from in the transcript **or raw notes**. [1] | Exact excerpt presentation, timestamp display, jump-to-transcript mechanics, or citation stability after edits. |
| Chat answers | Inline citations allow drilling into detail. Chat proactively identifies which notes it refers to, and users can redirect it to other notes. The announcement discusses both notes and transcripts as context. [4] | Whether every citation opens an exact transcript snippet, a timestamped location, or a meeting-level view. The announcement's "double-click into the detail" wording is not a precise gesture specification. |
| Audio evidence | Granola says it does not record or save audio/video, and meeting audio cannot be accessed afterward. [2] | There is no basis here for claiming citations play audio clips. |

Keep these separate in a TrailMix design: the source meeting identifies the context; a transcript passage or human-note range supports a particular claim. Optional user-created anchors are another concept. These sources do not establish that Granola requires such anchors for enrichment or citations.

## Export, local files, and external agents

- Historical export is an emailed CSV containing titles, note summaries, transcripts, and basic details. It includes owned notes in selected workspaces, including history beyond the Basic visibility window, but excludes deleted notes and notes without summaries. The docs do not explicitly enumerate raw human notes or chat history as export fields. [6]
- A transcript can also be copied directly from its panel. The reviewed sources do not document an automatically maintained local Markdown meeting directory or filesystem round-trip editing. This is an evidence limit, not a claim about all Granola internal storage. [2][6]
- Official MCP support exists, including a Claude Code setup and a public remote endpoint, `https://mcp.granola.ai/mcp`. Standard clients need Streamable HTTP and OAuth support. Tools query meetings, list meetings, retrieve private and summarized notes, and retrieve raw transcripts on paid plans. Access follows the user's active workspace and documented permissions; it does not combine multiple workspaces. [7]
- An official API provides programmatic access to notes, transcripts, and AI summaries for custom agents and integrations. API keys require Business or Enterprise. This is documented agent support, not an inferred or unofficial local-cache integration. [8]
- OpenCode is not named in the reviewed Granola documentation. Compatibility with Granola MCP would need separate verification. Pointing OpenCode at TrailMix's own local meeting files is a different proposed workflow and was not tested in this research.

## Calendar connection

Follow-up checked 2026-09-24. Granola's calendar integration supplies an upcoming-meeting list, launch/reminder controls, and meeting context. [9][10]

- Supported providers are Google Calendar and Microsoft/Outlook. Microsoft support covers cloud Microsoft 365 calendars, not on-premises Exchange. Apple Calendar is not supported. Users can select additional calendars visible through their connected provider. [9][11]
- Upcoming events appear in "Coming up", five at a time by default. Granola uses event titles, scheduled times, and attendee information in its meeting workflow: titles also help link related meetings, times drive reminders/start behavior, and meeting details and attendees provide context for notes. The text does not enumerate every field displayed on an upcoming-meeting card or every metadata field sent to note generation. [9][10][1]
- Calendar reminders appear one minute before events with at least two attendees. Clicking one opens the event's video-call URL, opens its Granola note, and starts transcription. Notifications can be disabled. Users can also open the event from "Coming up" or use **New Note** for an unscheduled meeting. [9][10]
- Calendar sync alone does not start transcription or send a bot to meetings. The user must open a note or act on a notification. There is a timing nuance: the transcription documentation says opening an upcoming note before its start time allows transcription to start automatically at that scheduled time. Granola captures local microphone/system audio and says it does not retain audio recordings. [9][2]
- Calendar context is one input to enhanced notes alongside raw notes and the transcript. The calendar guide specifically names meeting details and attendees. It does not specify an exhaustive generation-input schema, so do not assume all event descriptions, attachments, or other fields are included. [1][9]
- **Optionality is only partly verified.** Individual meetings need not be on a calendar, since ad-hoc notes are supported. However, the reviewed onboarding guide connects Google/Microsoft calendars during sign-in and prompts SSO users to link one. These sources do not establish whether a user can complete onboarding and use Granola with calendar authorization entirely omitted. [9][11]

The canonical chat documentation URL was fetched successfully in this follow-up: https://docs.granola.ai/help-center/getting-more-from-your-notes/chatting-with-your-meetings. The path contains `your-notes`, not `your-nototes`. [3]

## Implications for TrailMix

These are product recommendations based on the distinctions above, not claims about Granola implementation.

1. **Keep document identity separate from window layout.** If TrailMix later adds an enhanced document, preserving the human original has a Granola precedent. A separate file is TrailMix's choice, and it can open on demand in an existing window. Granola's switchable notes and transcript panel provide no reason to require three splits. [1][2]
2. **Q&A can precede note expansion.** Keep the editable human note and live read-only transcript as the capture workflow. Afterward, Q&A can reuse the transcript window or run in an external agent. This borrows Granola's meeting-scoped questions without requiring its automatic enhancement step. Decide explicitly whether useful answers remain in chat history or are inserted into the human note. [1][3]
3. **Separate retrieval from synthesis.** "Find the meeting where we discussed X" and "summarize our decisions across these meetings" deserve distinct behavior, even if both use a chat prompt. Return source meetings and supporting passages where available. Do not treat a meeting link as proof of a precise quotation. [3][4]
4. **Local agent access is a plausible first experiment.** Granola already supports external agents through MCP/API. TrailMix could instead expose its ordinary local files to OpenCode, with meeting identifiers and stable source references. Whether this handles cross-meeting recall well enough remains an experiment; file access alone does not establish retrieval quality or citation accuracy. [7][8]

## Sources

1. [AI-enhanced notes](https://docs.granola.ai/help-center/taking-notes/ai-enhanced-notes)
2. [How transcription works](https://docs.granola.ai/help-center/taking-notes/transcription)
3. [Chatting with your meetings](https://docs.granola.ai/help-center/getting-more-from-your-notes/chatting-with-your-meetings)
4. [Granola Chat just got smarter](https://www.granola.ai/blog/granola-chat-just-got-smarter)
5. [Granola Chat product page](https://www.granola.ai/chat), first-party feature descriptions only, excluding embedded customer testimonials.
6. [Exporting historical notes](https://docs.granola.ai/help-center/sharing/exporting-notes)
7. [Granola MCP](https://docs.granola.ai/help-center/sharing/integrations/mcp)
8. [Granola API](https://docs.granola.ai/help-center/sharing/integrations/granola-api)
9. [Syncing your calendars](https://docs.granola.ai/help-center/getting-started/syncing-your-calendars)
10. [Notifications](https://docs.granola.ai/help-center/taking-notes/notifications)
11. [Signing in and connecting your calendar](https://docs.granola.ai/help-center/signing-in-and-connecting-your-calendar)
