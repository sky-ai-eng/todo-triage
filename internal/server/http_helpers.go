package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// internalError logs the error with the given scope tag and writes a
// 500 to the client. In local mode the raw err.Error() is returned so a
// developer staring at their own machine can read it; in multi mode the
// client sees a generic message and the detail stays in server logs
// only — raw Go errors (driver messages, file paths, internal IDs)
// must not leak to other tenants' browsers.
//
// scope is the short subsystem tag that previously appeared in inline
// log.Printf calls (e.g. "tasks", "projects", "reviews").
func internalError(w http.ResponseWriter, scope string, err error) {
	log.Printf("[%s] %v", scope, err)
	msg := err.Error()
	if runmode.Current() == runmode.ModeMulti {
		msg = "internal server error"
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
}

// notFound writes a 404 with a "<thing> not found" message. Centralized
// so the wording stays consistent across handlers.
func notFound(w http.ResponseWriter, thing string) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": thing + " not found"})
}

// badRequest writes a 400 with the given message.
func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

// decodeJSON decodes the request body into v. On failure it writes a
// 400 with the given message (or "invalid request body" if msg is empty)
// and returns false; callers should `return` immediately.
//
// Use:
//
//	var req CreateFooReq
//	if !decodeJSON(w, r, &req, "") {
//	    return
//	}
//
// v must be a pointer. Anonymous-struct request shapes are supported by
// passing &req where req is the local var.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, msg string) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if msg == "" {
			msg = "invalid request body"
		}
		badRequest(w, msg)
		return false
	}
	if dec.More() {
		if msg == "" {
			msg = "invalid request body"
		}
		badRequest(w, msg)
		return false
	}
	return true
}
