package task

const (
	TaskTypeFileSync   = "file_sync"
	TaskTypeSSHCommand = "ssh_command"
)

type FileSyncPayload struct {
	FolderPath string `json:"folder_path"`
	Prefix     string `json:"prefix"`
}

type SSHPayload struct {
	Command string `json:"command"`
}

type SyncResult struct {
	FolderPath    string   `json:"folder_path"`
	Prefix        string   `json:"prefix"`
	TotalFiles    int      `json:"total_files"`
	UploadedFiles int      `json:"uploaded_files"`
	FailedFiles   []string `json:"failed_files"`
	Duration      string   `json:"duration"`
}
