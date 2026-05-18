package main

import (
	"bufio"
	"io"
	"os"
	"time"
)

// tailWorker re-reads the log file as it grows, populating the in-memory
// ring buffer used to serve cheap tail requests. Runs until the job's
// log file is closed (which happens after Wait returns in the reaper).
func (j *Job) tailWorker() {
	f, err := os.Open(j.LogPath)
	if err != nil {
		return
	}
	defer f.Close()

	br := bufio.NewReader(f)
	var pending []byte

	for {
		// Read whatever's currently available.
		for {
			b, err := br.ReadBytes('\n')
			if len(b) > 0 {
				pending = append(pending, b...)
				if len(pending) > 0 && pending[len(pending)-1] == '\n' {
					line := string(pending[:len(pending)-1])
					j.appendLine(line)
					pending = pending[:0]
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return
			}
		}

		// Are we done? If the job is finished and we've drained, flush any
		// partial line and stop.
		j.mu.Lock()
		finished := j.State != JobStateRunning
		j.mu.Unlock()

		if finished {
			// Try once more to drain anything the kernel hadn't flushed yet.
			for {
				b, err := br.ReadBytes('\n')
				if len(b) > 0 {
					pending = append(pending, b...)
					if len(pending) > 0 && pending[len(pending)-1] == '\n' {
						line := string(pending[:len(pending)-1])
						j.appendLine(line)
						pending = pending[:0]
					}
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					break
				}
			}
			if len(pending) > 0 {
				j.appendLine(string(pending))
			}
			return
		}

		time.Sleep(150 * time.Millisecond)
	}
}

// appendLine adds a single line to the in-memory ring buffer and bumps the
// LinesLogged counter.
func (j *Job) appendLine(line string) {
	j.tailMu.Lock()
	j.tailLines = append(j.tailLines, line)
	if len(j.tailLines) > j.tailMax {
		// Drop oldest. We could use a real ring buffer, but slicing is
		// fine for tailMax in the hundreds.
		drop := len(j.tailLines) - j.tailMax
		j.tailLines = append([]string(nil), j.tailLines[drop:]...)
	}
	j.tailMu.Unlock()
	j.LinesLogged.Add(1)
}

// readLinesRange opens the on-disk log and returns lines [start, end]
// (1-based, inclusive). Used as the cold path when the requested range
// has aged out of the in-memory buffer.
func readLinesRange(path string, start, end int) []string {
	if start < 1 {
		start = 1
	}
	if end < start {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	out := make([]string, 0, end-start+1)
	n := 0
	for scanner.Scan() {
		n++
		if n < start {
			continue
		}
		if n > end {
			break
		}
		out = append(out, scanner.Text())
	}
	return out
}
