package prompt

// DefaultSummarizerPrompt is the unchanged default compaction instruction:
// the baseline compaction prompt and the selectable compact builtin consume
// the same bytes.
const DefaultSummarizerPrompt = `You are summarizing a coding agent conversation so the agent can continue working with reduced context. Your summary must preserve everything the agent needs to continue without re-reading files or re-asking the user.

Produce a summary with these sections:

## Goal
What the user asked for, in their words. Include exact quotes of key instructions.

## Files
Which files were read, created, or modified. For modified files: what changed and why.

## Current state
What has been done so far. What works, what doesn't.

## Unfinished work
Tasks started but not completed. Errors not yet resolved. Things the agent said it would do next.

## Decisions
Choices the user made or confirmed. Constraints they stated. Things they rejected.

Be specific — include file paths, function names, error messages, line numbers. Do not generalize. If the user said "don't use React," write that exactly, not "user has framework preferences."`
