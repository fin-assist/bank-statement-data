package importing

import "time"

// Header is transport-independent upload metadata. FileName is preserved exactly.
type Header struct {
	FileName    string
	ContentType string
	SizeBytes   int64
}

// Accepted identifies the same admitted upload on every successful key replay.
type Accepted struct {
	UploadID string `json:"upload_id"`
	FileName string `json:"file_name"`
}

// Upload is a durable import and its cleanup state. Failed/deleted rows remain
// addressable for cancellation, repeat deletion and original-key replay.
type Upload struct {
	ID                   string
	UserID               string
	FileName             string
	ObjectKey            string
	SizeBytes            int64
	Digest               [32]byte
	Status               string
	ErrorCode            string
	Percent              int
	UploadedAt           time.Time
	FinishedAt           *time.Time
	FileDeletedAt        *time.Time
	OperationCount       int64
	AccountIDs           []int64
	Period               *Period
	Result               *Result
	UnreadCount          int
	ResolvedUnreadCount  int
	ResolvedByUploadedAt *time.Time
}

// Unread retains only the allowed source metadata, never row contents.
type Unread struct {
	UploadID         string
	FileName         string
	Number           int
	Occurrence       int
	OccurredAt       *time.Time
	Reason           string
	AccountID        *int64
	FITID            string
	ResolvedByUpload string
	Resolution       string
}

// Scope selects unread rows by exactly one upload or a half-open date range.
type Scope struct {
	UploadID string
	Period   *TimeRange
}

// Source proves that a particular completed upload contained an account/FITID key,
// including a readable duplicate of an operation created by another upload.
type Source struct {
	UploadID  string
	AccountID int64
	FITID     string
}

// Snapshot is coherent user data read under the usecase's transaction boundary.
type Snapshot struct {
	TimeZone   string
	Uploads    []Upload
	Operations []Operation
	Categories map[string]Category
	Unread     []Unread
	Sources    []Source
}

// Stage contains newly linked source data before atomic accounting completion.
type Stage struct {
	Snapshot   Snapshot
	NewIDs     map[string]bool
	AccountIDs []int64
}

// Event is the current durable progress or a terminal import result.
type Event struct {
	TimeZone string
	Percent  int
	Result   *Result
	Error    string
}

// Config contains the import links, recommended range and actual loaded summary.
type Config struct {
	DownloadURL string
	LoginURL    string
	TimeZone    string
	From        time.Time
	To          time.Time
	Loaded      *Loaded
}

// Loaded summarizes actual readable operations across completed imports.
type Loaded struct {
	First time.Time
	Last  time.Time
	Count int64
}

// History contains visible uploads and their actual union coverage.
type History struct {
	TimeZone string
	Uploads  []Upload
	Loaded   *Loaded
	Gaps     []TimeRange
}
