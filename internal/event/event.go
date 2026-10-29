// Package event defines the progress stream shared by the pipeline and its
// renderers: the pipeline components emit events through an Observer
// callback and the caller decides how to render them. The package has no
// dependencies so both the engine and any UI code can rely on it.
package event

import "time"

// Kind enumerates the events a pipeline run produces.
type Kind uint8

const (
	// RunStart announces a run; Stats carries PartsTotal and GroupsTotal.
	RunStart Kind = iota
	// ResolveDone is a part resolved to a direct link; Filename is the
	// target file name the part will download to.
	ResolveDone
	// ResolveFail is a part that failed to resolve; Err is set.
	ResolveFail
	// DownloadStart is a target download beginning; ResumeAt is the byte
	// offset the download continues from (0 = fresh start).
	DownloadStart
	// Progress is bytes moved for one target. Emitters throttle it, so
	// it is a hint for display, not an accounting item.
	Progress
	// DownloadRetry is a failed attempt followed by another one after a
	// backoff; Attempt is the number of the attempt that failed.
	DownloadRetry
	// DownloadDone is a target that reached its server-reported size and
	// was renamed; Size is the final size.
	DownloadDone
	// DownloadSkipped is a target whose file was already present; Size
	// is its size.
	DownloadSkipped
	// DownloadFail is a target that gave up after all its attempts; Err
	// carries the last error, Attempt the attempts used.
	DownloadFail
	// RunPaused and RunResumed mark user pause/resume of the downloads.
	RunPaused
	RunResumed
	// RunDone ends a run; Stats carries the final summary.
	RunDone
)

// Stats describes a run: used by RunStart (totals only) and RunDone (full).
type Stats struct {
	PartsTotal  int
	GroupsTotal int
	Done        int
	Skipped     int
	Failed      int
	Bytes       int64
	Elapsed     time.Duration
}

// Event is one message on the pipeline's event stream.
type Event struct {
	Kind     Kind
	Group    string // archive group (directory) the part belongs to
	Part     string // stable identity of the part across phases
	Filename string // target file name where known (resolve events)
	Done     int64  // bytes downloaded so far (Progress)
	Total    int64  // target size (Progress)
	Size     int64  // final size (DownloadDone, DownloadSkipped)
	ResumeAt int64  // byte offset when resuming (DownloadStart)
	Attempt  int
	Err      error
	Stats    *Stats // RunStart and RunDone only
}

// Observer consumes events. Emitters call it from concurrent worker
// goroutines, so implementations must be safe for concurrent use.
type Observer func(Event)

// Nop is the no-op observer.
func Nop(Event) {}
