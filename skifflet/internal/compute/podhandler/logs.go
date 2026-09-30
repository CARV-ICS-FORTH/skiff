package podhandler

import (
	"bytes"
	"fmt"
	"os"
	"sync"
	"time"

	"skifflet/internal/compute/endpoint"
)

// Container logs.
//
// Apptainer has no logging of its own for the containers Skifflet runs, so the
// output of a container is passed through Skifflet: the launcher's standard output
// and error are pipes, and one copier per stream writes what comes out of them to
// the attempt's log, as CRI log records:
//
//	<RFC 3339 time> stdout|stderr P|F <content>
//
// F ends a line (the record's own newline is the line's), P is a piece of a line
// whose end has not been seen: it is written as soon as it is read, so output that
// never ends a line (a prompt, a progress bar) is not held back, and a line that
// is still open when the container ends stays open, without a newline invented for
// it. A record carries at most maxLogRecord bytes of content.
//
// Every attempt of a container has its own file, logs/<container>/<attempt>.log,
// so a reader can never mix two attempts. The log of the attempt before the
// current one is kept and older ones are removed when an attempt starts; all of
// them go with the pod. Anything the container leaves in the launcher's streams
// after it has ended is drained for a short time and then dropped.

const (
	maxLogRecord = 16 * 1024
	logDrain     = 2 * time.Second
)

// AttemptLog is the log of one attempt, from the pipes to the file.
type AttemptLog struct {
	path string
	file *os.File

	outR, outW, errR, errW *os.File

	mu     sync.Mutex // one record at a time in the file
	failed bool       // a write failed and was reported; the output is drained and dropped
	copies sync.WaitGroup
	done   chan struct{} // closed once every record has been written and the file closed
}

// OpenLog creates the log of the attempt and the pipes the container's output
// goes through, and removes the logs of attempts older than the previous one. The
// launcher's Stdout and Stderr are to be set to the write ends; Begin starts the
// copying once it has been started, Abort undoes it when it could not start.
func (a *Attempt) OpenLog(container endpoint.ContainerPath) (*AttemptLog, error) {
	if err := os.MkdirAll(container.LogDir(), 0o755); err != nil {
		return nil, err
	}
	pruneAttemptLogs(container, a.ID-1)
	path := container.AttemptLogPath(a.ID)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cannot open the container log %s: %w", path, err)
	}
	l := &AttemptLog{path: path, file: file, done: make(chan struct{})}
	if l.outR, l.outW, err = os.Pipe(); err != nil {
		file.Close()
		return nil, err
	}
	if l.errR, l.errW, err = os.Pipe(); err != nil {
		l.outR.Close()
		l.outW.Close()
		file.Close()
		return nil, err
	}
	a.pl.mu.Lock()
	a.log = l
	a.pl.mu.Unlock()
	return l, nil
}

// pruneAttemptLogs removes the logs of the attempts before keepFrom.
func pruneAttemptLogs(container endpoint.ContainerPath, keepFrom int) {
	attempts, _ := container.AttemptLogs()
	for _, attempt := range attempts {
		if attempt < keepFrom {
			_ = os.Remove(container.AttemptLogPath(attempt))
		}
	}
}

// Stdout and Stderr are the write ends of the pipes for the launcher.
func (l *AttemptLog) Stdout() *os.File { return l.outW }
func (l *AttemptLog) Stderr() *os.File { return l.errW }

// Begin starts copying. Skifflet's own copies of the write ends are closed, so
// that the pipes end when the container's processes have gone.
func (l *AttemptLog) Begin() {
	l.outW.Close()
	l.errW.Close()
	l.copies.Add(2)
	go l.copy("stdout", l.outR)
	go l.copy("stderr", l.errR)
}

// Abort discards the log of an attempt that did not start.
func (l *AttemptLog) Abort() {
	for _, f := range []*os.File{l.outR, l.outW, l.errR, l.errW} {
		f.Close()
	}
	l.file.Close()
	close(l.done)
}

// Finish waits for the output the container left in the pipes to be written (a
// bounded wait: a process the container left behind may hold them open), then
// closes the log. Nothing is written after it, and readers following the attempt
// see it end.
func (l *AttemptLog) Finish() {
	drained := make(chan struct{})
	go func() { l.copies.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(logDrain):
		l.outR.Close() // unblocks the copiers; what is still in the pipes is dropped
		l.errR.Close()
		<-drained
	}
	l.outR.Close()
	l.errR.Close()
	l.file.Close()
	close(l.done)
}

// Done is closed when the log is complete.
func (l *AttemptLog) Done() <-chan struct{} { return l.done }

func (l *AttemptLog) copy(stream string, r *os.File) {
	defer l.copies.Done()
	buf := make([]byte, maxLogRecord)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			l.record(stream, buf[:n], time.Now())
		}
		if err != nil {
			return
		}
	}
}

// record writes what one read returned as records: a full record for every
// line that ended, and a partial one for the rest.
func (l *AttemptLog) record(stream string, data []byte, now time.Time) {
	stamp := now.UTC().Format(time.RFC3339Nano)
	var out bytes.Buffer
	for len(data) > 0 {
		tag, content := "F", data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			content, data = data[:i], data[i+1:]
		} else {
			tag, data = "P", nil
		}
		fmt.Fprintf(&out, "%s %s %s ", stamp, stream, tag)
		out.Write(content)
		out.WriteByte('\n')
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failed {
		return
	}
	if _, err := l.file.Write(out.Bytes()); err != nil {
		// Keep taking the container's output, so that it never blocks on a full pipe.
		l.failed = true
		fmt.Fprintf(os.Stderr, "skifflet: writing the container log %s failed, the rest of its output is dropped: %v\n", l.path, err)
	}
}
