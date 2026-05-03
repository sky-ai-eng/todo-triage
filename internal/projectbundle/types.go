package projectbundle

// ExportPreview is the structured file list shown before export.
type ExportPreview struct {
	Files     []ExportPreviewFile `json:"files"`
	TotalSize int64               `json:"total_size"`
}

type ExportPreviewFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

// ImportWarning is a non-fatal issue surfaced after import (for example a
// clone failure after DB/filesystem state has already been materialized).
type ImportWarning struct {
	Code    string `json:"code"`
	Repo    string `json:"repo,omitempty"`
	Message string `json:"message"`
}
