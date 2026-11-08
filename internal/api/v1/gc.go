package v1

// GCRequest is POST /v1/gc. With DryRun the Controller deletes nothing and
// reports what the same pass would delete.
type GCRequest struct {
	DryRun bool `json:"dry_run"`
}

// GCCount is how many rows (or blobs) a pass deletes, and the bytes where the
// thing is a file.
type GCCount struct {
	Kind  string `json:"kind"`
	Count int64  `json:"count"`
	Bytes int64  `json:"bytes,omitempty"`
}

// GCReport is what a pass of the garbage collector did, or with DryRun what it
// would do. Items are in the order the pass runs them: retention of the
// history first, then the blobs nothing refers to any more.
//
// HeldBack counts the events past their retention that stay because a Run that
// has not ended, or an event before them that has not expired, holds the purge:
// events go in a contiguous prefix of their sequence.
type GCReport struct {
	DryRun   bool      `json:"dry_run"`
	Items    []GCCount `json:"items"`
	HeldBack int64     `json:"events_held_back"`
}

// StorageStats is GET /v1/storage/stats: what the Controller keeps, by
// category, and how much of it a pass of the GC would free. It changes nothing.
//
// Blobs are the content-addressed files: the output of the Runs, the files of
// the Workflows, and the ones nothing refers to (yet: the GC waits a grace
// period before deleting them). The database and the relay are measured on
// disk. MaxStorageBytes is max_storage, 0 if none; it only measures.
type StorageStats struct {
	OutputBlobBytes       int64    `json:"output_blob_bytes"`
	WorkflowBlobBytes     int64    `json:"workflow_blob_bytes"`
	UnreferencedBlobBytes int64    `json:"unreferenced_blob_bytes"`
	DatabaseBytes         int64    `json:"database_bytes"`
	RelayBytes            int64    `json:"relay_bytes"`
	TotalBytes            int64    `json:"total_bytes"`
	MaxStorageBytes       int64    `json:"max_storage_bytes,omitempty"`
	OverMaxStorage        bool     `json:"over_max_storage"`
	Reclaimable           GCReport `json:"reclaimable"`
}
