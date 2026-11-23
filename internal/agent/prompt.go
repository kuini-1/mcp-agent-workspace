package agent

import (
	"fmt"
	"strings"
)

// systemPrompt is built once per task and never changes during it: editing
// the system prompt mid-task would break the prompt cache and invalidate
// earlier thinking blocks.
func systemPrompt(workdir string) string {
	return strings.TrimSpace(fmt.Sprintf(`
You are a software agent working inside one workspace directory: %s
All file paths are relative to it. You cannot read or write outside it.

How to work:
- Look before you change things: list and read the relevant files first.
- Make small, verifiable steps. After a change, run the check that proves it works (build, test, or run it).
- Some shell commands need a human's approval and some are refused. If a call is denied, choose another approach; don't retry the same call.
- When a tool call fails, call journal_note with kind "failed" saying what you tried and the likely cause, before you retry. When something finally works, record it with kind "worked". These notes are kept as the task's debug log.
- Never repeat an identical tool call that already failed. Change something first.

Finishing:
- When the task is done and verified, call finish_task with a short summary of what changed and how you checked it.
- If you cannot continue (missing information, a denied action you need, an error you can't resolve), call mark_blocked with the reason and the next steps a human should take.
`, workdir))
}

const compactionPrompt = `Your context is about to be reset. Write a summary that lets you continue this task with no other memory of the conversation.

Include:
1. The task goal and any constraints from the user.
2. Files created or changed, with their purpose and current state.
3. Commands run that matter, and their results.
4. What failed, why, and what you learned from it.
5. What works now and how it was verified.
6. The exact next step you were about to take, and any open questions.

Be specific: names, paths, error messages, numbers. Don't call tools. Reply with the summary only.`

func continuationMessage(taskPrompt, summary string, epoch int) string {
	return fmt.Sprintf(`You are continuing a task. Your earlier context was compacted (reset %d); this summary replaces it.

## Task
%s

## Progress so far
%s

Continue from the next step. The workspace files are as you left them.`, epoch, taskPrompt, strings.TrimSpace(summary))
}

func failureReminder(n int) string {
	if n == 1 {
		return "A tool call above failed. Before retrying, record what you tried and the likely cause with journal_note (kind: failed). Don't repeat the identical call."
	}
	return fmt.Sprintf("%d tool calls above failed. Before retrying, record what you tried and the likely causes with journal_note (kind: failed). Don't repeat identical calls.", n)
}
