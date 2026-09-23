package taskqueues

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// The trusted launcher supplies exact files. No arbitrary filesystem paths or
// shell commands are accepted from the reviewer.
func readReviewFile(manifest map[string]string, args json.RawMessage) (any, error) {
	var request struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
	}
	if e := json.Unmarshal(args, &request); e != nil {
		return nil, e
	}
	if request.Offset < 0 {
		return nil, errors.New("offset must be nonnegative")
	}
	if request.Path == "" {
		names := []string{}
		for name := range manifest {
			names = append(names, name)
		}
		return map[string]any{"files": names}, nil
	}
	path, ok := manifest[request.Path]
	if !ok {
		return nil, errors.New("file is not in this review's approved manifest")
	}
	resolved, e := filepath.EvalSymlinks(path)
	if e != nil {
		return nil, e
	}
	if !filepath.IsAbs(path) || resolved != filepath.Clean(path) {
		return nil, errors.New("review file path changed")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		return nil, errors.New("review input must be a regular file")
	}
	data, e := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if e != nil {
		return nil, e
	}
	if len(data) > 16<<20 || !utf8.Valid(data) {
		return nil, errors.New("review input must be UTF-8 and at most 16 MiB")
	}
	chars := []rune(string(data))
	start := min(request.Offset, len(chars))
	end := min(start+20000, len(chars))
	return map[string]any{"path": request.Path, "text": string(chars[start:end]), "next_offset": end, "has_more": end < len(chars)}, nil
}
