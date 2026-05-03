package projectbundle

import (
	"context"
	"database/sql"
)

// Preview returns the exact file list and aggregate size that Export would
// include for the given project.
func Preview(ctx context.Context, database *sql.DB, projectID string) (*ExportPreview, error) {
	state, err := collectExportState(ctx, database, projectID)
	if err != nil {
		return nil, err
	}
	out := &ExportPreview{
		Files: make([]ExportPreviewFile, 0, len(state.artifacts)),
	}
	for _, a := range state.artifacts {
		out.Files = append(out.Files, ExportPreviewFile{
			Path:      a.bundlePath,
			SizeBytes: a.size,
		})
		out.TotalSize += a.size
	}
	return out, nil
}
