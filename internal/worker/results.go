package worker

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"mactunnel/internal/protocol"
)

// archivesDir holds zipped folder outputs, beside the job's working folder so a
// folder can't end up zipping its own archive.
const archivesDir = "archives"

// outputCapture keeps a command's stdout for its result. Text keeps the end,
// which is where a build or test report says how it went. JSON keeps the start,
// and anything over the limit fails, since a cut-off JSON value is useless.
type outputCapture struct {
	limit   int
	keepEnd bool
	data    []byte
	total   int64
}

func (o *outputCapture) Write(p []byte) (int, error) {
	o.total += int64(len(p))
	switch {
	case o.keepEnd:
		o.data = append(o.data, p...)
		if len(o.data) > 2*o.limit {
			o.data = append(o.data[:0], o.data[len(o.data)-o.limit:]...)
		}
	case len(o.data) <= o.limit:
		o.data = append(o.data, p[:min(len(p), o.limit+1-len(o.data))]...)
	}
	return len(p), nil
}

// cut says whether some output didn't fit.
func (o *outputCapture) cut() bool { return o.total > int64(o.limit) }

const cutNote = "[earlier output was cut]\n"

// commandResult turns what a command printed into the job's result.
func commandResult(r *ResultConfig, data []byte, cut bool) (json.RawMessage, *protocol.Failure) {
	if r.Format == formatJSON {
		if cut {
			return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: fmt.Sprintf("the command printed more than the %d bytes a result can hold", maxResultBytes)}
		}
		return jsonResult(r, data)
	}
	text := string(data)
	if len(text) > maxResultBytes {
		text = text[len(text)-maxResultBytes:]
	}
	if cut {
		// Start at a whole line.
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	text = strings.TrimRight(text, "\r\n")
	if r.TailLines > 0 {
		lines := strings.Split(text, "\n")
		if len(lines) > r.TailLines {
			text = strings.Join(lines[len(lines)-r.TailLines:], "\n")
			cut = true
		}
	}
	if cut {
		text = cutNote + text
	}
	return textResult(text)
}

// textResult encodes text, trimming its start if escaping makes it too large.
func textResult(text string) (json.RawMessage, *protocol.Failure) {
	text = strings.ToValidUTF8(text, "�")
	for {
		encoded, err := json.Marshal(text)
		if err != nil {
			return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: err.Error()}
		}
		if len(encoded) <= maxEncodedResultBytes {
			return encoded, nil
		}
		keep := strings.TrimPrefix(text, cutNote)
		keep = strings.ToValidUTF8(keep[len(keep)/2:], "")
		text = cutNote + keep
	}
}

func jsonResult(r *ResultConfig, data []byte) (json.RawMessage, *protocol.Failure) {
	doc, err := decodeJSON(data)
	if err != nil {
		return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: "the command's result isn't valid JSON: " + err.Error()}
	}
	if r.Field != "" {
		var ok bool
		if doc, ok = fieldAt(doc, r.Field); !ok {
			return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: fmt.Sprintf("the command's result has no %q", r.Field)}
		}
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: err.Error()}
	}
	if len(encoded) > maxEncodedResultBytes {
		return nil, &protocol.Failure{Code: protocol.CodeBadResponse, Message: "the result is too large once encoded"}
	}
	return encoded, nil
}

// fileResult reads the result from a file the command wrote.
func fileResult(r *ResultConfig, work string) (json.RawMessage, *protocol.Failure) {
	root, err := os.OpenRoot(work)
	if err != nil {
		return nil, &protocol.Failure{Code: protocol.CodeOutputMissing, Message: err.Error()}
	}
	defer root.Close()
	file, err := root.Open(r.Path)
	if err != nil {
		return nil, &protocol.Failure{Code: protocol.CodeOutputMissing, Message: fmt.Sprintf("result file %q: %v", r.Path, err)}
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, &protocol.Failure{Code: protocol.CodeOutputMissing, Message: fmt.Sprintf("result file %q isn't a regular file", r.Path)}
	}
	capture := &outputCapture{limit: maxResultBytes, keepEnd: r.Format == formatText}
	if _, err := io.Copy(capture, file); err != nil {
		return nil, &protocol.Failure{Code: protocol.CodeOutputMissing, Message: err.Error()}
	}
	return commandResult(r, capture.data, capture.cut())
}

// archiveOutput zips a folder or file the command made, and returns where the
// zip is. Links inside are left out rather than followed, so a link can't pull
// in a file from elsewhere on this machine.
func archiveOutput(work *os.Root, rel, dir, name string) (*os.Root, string, error) {
	info, err := work.Lstat(rel)
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, "", errors.New("not a folder or regular file")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	out, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", err
	}
	zipName := name + ".zip"
	if err := writeZip(work, rel, info, out, zipName); err != nil {
		out.Close()
		return nil, "", err
	}
	return out, zipName, nil
}

func writeZip(work *os.Root, rel string, info fs.FileInfo, out *os.Root, zipName string) error {
	file, err := out.Create(zipName)
	if err != nil {
		return err
	}
	defer file.Close()
	zw := zip.NewWriter(file)

	add := func(fsys fs.FS, path, name string, info fs.FileInfo) error {
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = name
		header.Method = zip.Deflate
		w, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		src, err := fsys.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(w, src)
		return err
	}

	if !info.IsDir() {
		if err := add(work.FS(), rel, info.Name(), info); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		return file.Close()
	}

	folder, err := work.OpenRoot(rel)
	if err != nil {
		return err
	}
	defer folder.Close()
	fsys := folder.FS()
	err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == "." {
			return err
		}
		if d.IsDir() {
			_, err := zw.Create(path + "/")
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return add(fsys, path, path, info)
	})
	if err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return file.Close()
}
