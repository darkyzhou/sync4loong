package task

const (
	TaskTypeFileSyncSingle = "file_sync_single"
	TaskTypeSSHCommand     = "ssh_command"
)

type SyncItem struct {
	From            string `json:"from"`
	To              string `json:"to"`
	DeleteAfterSync bool   `json:"delete_after_sync,omitempty"`
	Overwrite       bool   `json:"overwrite,omitempty"`
}

type FileSyncSinglePayload struct {
	FilePath        string `json:"file_path"`
	S3Key           string `json:"s3_key"`
	DeleteAfterSync bool   `json:"delete_after_sync,omitempty"`
	Overwrite       bool   `json:"overwrite,omitempty"`
}

type SSHPayload struct {
	Command string `json:"command"`
}

type SyncResult struct {
	Items         []SyncItemResult `json:"items"`
	TotalFiles    int              `json:"total_files"`
	UploadedFiles int              `json:"uploaded_files"`
	FailedFiles   []string         `json:"failed_files"`
	Duration      string           `json:"duration"`
}

type SyncItemResult struct {
	From          string   `json:"from"`
	To            string   `json:"to"`
	TotalFiles    int      `json:"total_files"`
	UploadedFiles int      `json:"uploaded_files"`
	FailedFiles   []string `json:"failed_files"`
}
