package tool

import (
	"github.com/MMinasyan/lightcode/internal/editpreview"
)

// ApplyPatchPreviewMetadata converts captured per-file previews into the
// public edit_preview_files metadata map (nil when there are no previews).
func ApplyPatchPreviewMetadata(previews []AppliedFilePreview) map[string]any {
	if len(previews) == 0 {
		return nil
	}
	files := make([]editpreview.FileEntry, 0, len(previews))
	for _, p := range previews {
		files = append(files, appliedPreviewToFileEntry(p))
	}
	return map[string]any{"edit_preview_files": files}
}

// appliedPreviewToFileEntry converts the engine's captured per-file
// pre/post/hunks data into the public editpreview.FileEntry shape.
// Add produces a synthetic "all add" hunk from the post content;
// Update / Move destination builds hunks from the captured classified
// lines with StartLine as the 1-based anchor; Delete / Move source
// produces no hunks (just the D tag, which the renderer uses as a
// header).
func appliedPreviewToFileEntry(p AppliedFilePreview) editpreview.FileEntry {
	return editpreview.FileEntry{
		Path:    p.Path,
		Op:      p.Op,
		Preview: buildPreviewFromCaptured(p),
	}
}

func buildPreviewFromCaptured(p AppliedFilePreview) editpreview.Preview {
	if len(p.Hunks) == 0 {
		if p.Op != "A" {
			return editpreview.Preview{}
		}
		// Add: build a synthetic single hunk with all post lines as add.
		if len(p.Post) == 0 {
			return editpreview.Preview{}
		}
		rows := make([]editpreview.Row, 0, len(p.Post))
		for i, l := range p.Post {
			rows = append(rows, editpreview.Row{
				Kind: editpreview.KindAdd, NewLine: i + 1, Text: l,
			})
		}
		return editpreview.Preview{Hunks: []editpreview.Hunk{{Rows: rows}}}
	}
	hunks := make([]editpreview.Hunk, 0, len(p.Hunks))
	for _, h := range p.Hunks {
		rows := make([]editpreview.Row, 0, len(h.Lines))
		oldLine := h.StartLine
		newLine := h.StartLine
		for _, l := range h.Lines {
			switch l.Kind {
			case lineContext:
				rows = append(rows, editpreview.Row{
					Kind: editpreview.KindContext, OldLine: oldLine, NewLine: newLine, Text: l.Text,
				})
				oldLine++
				newLine++
			case lineRemove:
				rows = append(rows, editpreview.Row{
					Kind: editpreview.KindRemove, OldLine: oldLine, Text: l.Text,
				})
				oldLine++
			case lineAdd:
				rows = append(rows, editpreview.Row{
					Kind: editpreview.KindAdd, NewLine: newLine, Text: l.Text,
				})
				newLine++
			}
		}
		hunks = append(hunks, editpreview.Hunk{Rows: rows})
	}
	return editpreview.Preview{Hunks: hunks}
}
