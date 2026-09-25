package api

import (
	"net/http"
	"strings"

	"rssfeeder/internal/voice"
)

// handleAsk runs one voice turn. Body: {"text": "...", "lang": "en-NZ", "context": {...}}
// Clients send back the context from the previous response for follow-ups.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	var req voice.Request
	if !decode(w, r, &req) {
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" || len(req.Text) > 500 {
		writeError(w, http.StatusBadRequest, "text must be 1–500 characters")
		return
	}
	resp, err := s.agent.Handle(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "voice agent error")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleBriefing is the spoken digest: GET /briefing?scope=<name>&period=today|yesterday|week&lang=
func (s *Server) handleBriefing(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cmd := voice.Command{Intent: voice.Brief, Scope: q.Get("scope"), Period: q.Get("period")}
	switch cmd.Period {
	case "", "today", "yesterday", "week":
	default:
		writeError(w, http.StatusBadRequest, "period must be today, yesterday or week")
		return
	}
	resp, err := s.agent.Run(r.Context(), cmd, voice.Request{Lang: q.Get("lang")})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "briefing failed")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
