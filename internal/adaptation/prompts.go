package adaptation

const gptTaskExecutionAddition = `Use more tool calls when they can improve the response, provide useful context, or help plan or complete the task.

If you encounter a new or unexpected challenge before the active task is complete, try to resolve it yourself first. If a tool call fails or returns incomplete or unhelpful output, try another tool or a different approach.`

var gptTaskExecutionAdaptation = Adaptation{
	Name: "gpt-task-execution",
	Additions: map[string]string{
		"task_execution": gptTaskExecutionAddition,
	},
}

const googleTaskExecutionAddition = "Use absolute paths when calling tools or running commands.\n\n" +
	"Do not run commands that require interaction, such as `y`/`yes` confirmation prompts. Run them in non-interactive mode when available, such as with `-y`, `--yes`, or `--non-interactive`.\n\n" +
	"Always provide a brief explanation BEFORE running commands that modify the filesystem."

var googleTaskExecutionAdaptation = Adaptation{
	Name: "google-task-execution",
	Additions: map[string]string{
		"task_execution": googleTaskExecutionAddition,
	},
}
