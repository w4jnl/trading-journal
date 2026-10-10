package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecoverAndLog(t *testing.T) {
	boom := recoverAndLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("nil map write")
	}))
	rec := httptest.NewRecorder()
	boom.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("panic -> %d, want 500", rec.Code)
	}
	// A handler that already wrote keeps its status; the panic is only logged.
	half := recoverAndLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("after write")
	}))
	rec = httptest.NewRecorder()
	half.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusAccepted {
		t.Errorf("post-write panic -> %d, want the 202 already sent", rec.Code)
	}
	// Normal responses pass through untouched, including Flush support.
	ok := recoverAndLog(noWriteDeadline(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, isFlusher := w.(http.Flusher); !isFlusher {
			t.Error("recorder must forward Flush for SSE")
		}
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("tea"))
	})))
	rec = httptest.NewRecorder()
	ok.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusTeapot || rec.Body.String() != "tea" {
		t.Errorf("pass-through = %d %q", rec.Code, rec.Body.String())
	}
	// http.ErrAbortHandler keeps its special meaning.
	abort := recoverAndLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }))
	func() {
		defer func() {
			if recover() != http.ErrAbortHandler {
				t.Error("ErrAbortHandler must be re-panicked")
			}
		}()
		abort.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
}
