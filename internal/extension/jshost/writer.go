package jshost

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
)

type jsonLineWriter struct {
	mu  sync.Mutex
	w   *bufio.Writer
	err error
}

func newJSONLineWriter(w io.Writer) *jsonLineWriter {
	return &jsonLineWriter{w: bufio.NewWriter(w)}
}

func (w *jsonLineWriter) WriteJSON(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return w.setErr(err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.err != nil {
		return w.err
	}
	if _, err := w.w.Write(data); err != nil {
		w.err = err
		return err
	}
	if err := w.w.WriteByte('\n'); err != nil {
		w.err = err
		return err
	}
	if err := w.w.Flush(); err != nil {
		w.err = err
		return err
	}
	return nil
}

func (w *jsonLineWriter) setErr(err error) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.err == nil {
		w.err = err
	}
	return w.err
}
