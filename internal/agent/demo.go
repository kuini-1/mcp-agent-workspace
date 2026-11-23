package agent

// Demo mode runs the dashboard without an API key. Every task replays the
// same canned conversation: the agent writes a shell script with a typo, the
// run fails (a debug note is written), it records the cause, fixes the
// script, verifies it and finishes. Running the script needs approval in the
// default policy, so the approval flow shows up too.
//
// The replies are fixed: they don't react to tool output or to a denied
// approval. Token counts are made up and the model name is "demo-scripted",
// which has no price, so the cost stays at $0.

const DemoModel = "demo-scripted"

const DemoPrompt = "Create greet.sh that prints a greeting for the name given as its first argument, then run it to check it works."

func DemoScript() *Scripted {
	u := func(in, read, out int64) ReplyUsage { return ReplyUsage{Input: in, CacheRead: read, Output: out} }
	return &Scripted{Replies: []string{
		Reply(DemoModel, "tool_use", u(2150, 0, 180),
			Text("I'll write the script first, then run it with a sample name to check the output."),
			ToolUse("demo_1", "write_file", Block{
				"path":    "greet.sh",
				"content": "#!/bin/sh\nname=\"$1\"\nprnt 'Hello, %s!\\n' \"$name\"\n",
			})),
		Reply(DemoModel, "tool_use", u(240, 2150, 60),
			ToolUse("demo_2", "run_shell", Block{"command": "sh greet.sh Ada"})),
		Reply(DemoModel, "tool_use", u(310, 2390, 240),
			Text("The run failed: `prnt` isn't a command. It's a typo for `printf`. Recording that, then fixing the script."),
			ToolUse("demo_3", "journal_note", Block{"kind": "failed", "text": "sh greet.sh Ada exited 127: 'prnt: not found'. Typo for printf on line 3."}),
			ToolUse("demo_4", "write_file", Block{
				"path":    "greet.sh",
				"content": "#!/bin/sh\nname=\"${1:-world}\"\nprintf 'Hello, %s!\\n' \"$name\"\n",
			})),
		Reply(DemoModel, "tool_use", u(280, 2700, 50),
			ToolUse("demo_5", "run_shell", Block{"command": "sh greet.sh Ada"})),
		Reply(DemoModel, "tool_use", u(260, 2980, 120),
			ToolUse("demo_6", "journal_note", Block{"kind": "worked", "text": "Replaced prnt with printf and defaulted the name to 'world'. Output: Hello, Ada!"})),
		Reply(DemoModel, "tool_use", u(200, 3240, 110),
			ToolUse("demo_7", "finish_task", Block{"summary": "Created greet.sh. It prints \"Hello, <name>!\" and falls back to \"world\" with no argument. Verified with `sh greet.sh Ada`."})),
	}}
}
