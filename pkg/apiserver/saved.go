package apiserver

import (
	"context"
	"log/slog"
	"net/http"
)

// answerWhenSaved holds back the first byte of every answer until the store
// has saved the changes made so far.
//
// The store may run ahead of its file (store.Run), and a crash takes back
// what the file did not have. An answer says what is there, or that a
// change was made; neither must be taken back once a client has read it.
// What follows the first byte needs no waiting: a watch sends changes, and
// the store tells of a change only when it is saved.
//
// The wait ends early when the client has gone: nobody is left to promise
// anything to.
func answerWhenSaved(next http.Handler, saved func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&heldBack{ResponseWriter: w, request: r, saved: saved}, r)
	})
}

// heldBack is a ResponseWriter that waits for the store before it begins.
type heldBack struct {
	http.ResponseWriter
	request *http.Request
	saved   func(context.Context) error
	begun   bool
}

func (w *heldBack) WriteHeader(status int) {
	w.begin()
	w.ResponseWriter.WriteHeader(status)
}

func (w *heldBack) Write(p []byte) (int, error) {
	w.begin()
	return w.ResponseWriter.Write(p)
}

// Flush is here for the handlers that send as they go: logs and watches.
func (w *heldBack) Flush() {
	w.begin()
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach what the server's writer can do.
func (w *heldBack) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// begin waits for the store, once. If the store cannot save, the answer
// would promise what may not last, so there is none: the connection ends.
func (w *heldBack) begin() {
	if w.begun {
		return
	}
	w.begun = true
	if err := w.saved(w.request.Context()); err != nil {
		slog.Error("no answer: the store cannot save", "error", err)
		panic(http.ErrAbortHandler)
	}
}
