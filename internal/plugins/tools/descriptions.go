package tools

// Model-facing tool descriptions and JSON parameter schemas. The texts mirror
// the retained concrete-tool surface; the target read_file description drops
// the cached-read-notice claim because target reads always return the bounded
// requested content.

const readFileDescription = `Reads a file from disk with line numbers.
- Results are returned with line numbers: each line is prefixed with its number and a tab (e.g. "1\tpackage main"). Line numbers start at 1.
- By default reads the first 500 lines. When you already know which part of the file you need, use offset and limit to read only that part.
- Do not use cat, head, tail, or sed via run_command to read files. Use this tool.`

const readFileParameters = `{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Absolute or relative path to the file to read."
    },
    "offset": {
      "type": "integer",
      "description": "1-indexed line number to start reading from."
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of lines to read."
    }
  },
  "required": ["path"]
}`

const writeFileDescription = `Writes a file to disk.
- Creates parent directories if they don't exist. Overwrites the file if it already exists.
- Use this tool for new files or complete rewrites. For targeted changes to existing files, use edit_file instead. Writing to a path that already exists overwrites it entirely, so when creating a new file, use a path that does not already exist.`

const writeFileParameters = `{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Absolute or relative path to the file to write."
    },
    "content": {
      "type": "string",
      "description": "Full content to write to the file."
    }
  },
  "required": ["path", "content"]
}`

const editFileDescription = `Performs exact string replacement in a file.
- When using text from read_file output, never include the line number prefix in old_string or new_string. The prefix format is: number + tab. Everything after the tab is the actual file content.
- The edit will FAIL if old_string is not unique in the file. Provide more surrounding context to make it unique, or use replace_all to change every instance.
- old_string and new_string must be different. old_string must not be empty.
- ALWAYS prefer editing existing files. Use write_file only for new files or complete rewrites.`

const editFileParameters = `{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path to the file to edit."
    },
    "old_string": {
      "type": "string",
      "description": "Exact string to search for. Must match byte-for-byte."
    },
    "new_string": {
      "type": "string",
      "description": "Replacement string."
    },
    "replace_all": {
      "type": "boolean",
      "description": "If true, replace every occurrence. If false (default), old_string must be unique in the file."
    }
  },
  "required": ["path", "old_string", "new_string"]
}`

const applyPatchDescription = `Edits, creates, deletes, or renames files using the apply_patch (V4A) patch format.
- Wrap every change between *** Begin Patch and *** End Patch, with one section per file.
- Start each section with a header: *** Add File: <path> (every following line is a + line of new content), *** Update File: <path> (edit in place), or *** Delete File: <path> (nothing follows).
- To rename, put *** Move to: <new path> on the line right after *** Update File: <path>.
- In an Update, write each change as a hunk starting with @@. Prefix context lines with a space, removed lines with -, and added lines with +. Include about 3 lines of context around each change, and add @@ <enclosing function or class> when that context is not unique.
- Use file paths relative to the project root.`

const applyPatchParameters = `{
  "type": "object",
  "properties": {
    "input": {
      "type": "string",
      "description": "The full patch text, from *** Begin Patch to *** End Patch."
    }
  },
  "required": ["input"]
}`
