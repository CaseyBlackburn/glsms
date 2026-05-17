package glsms

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// ServerConfig configures the REST API server.
type ServerConfig struct {
	// AuthToken, if set, requires callers to send
	// "Authorization: Bearer <token>" on every /api/ request.
	AuthToken string
	// CallTimeout bounds each upstream router call (default 45s).
	CallTimeout time.Duration
}

// Handler returns an http.Handler exposing the SMS API as JSON over REST:
//
//	GET    /api/status            -> modem/SIM status + new message count
//	GET    /api/sms               -> list all stored messages
//	POST   /api/sms               -> send {"to","body","timeout"}
//	POST   /api/sms/read          -> mark read {"name":"..."}
//	POST   /api/sms/unread        -> mark unread {"name":"..."}
//	DELETE /api/sms?name=NAME     -> delete the message with that storage name
//	GET    /healthz               -> liveness (no auth, no router call)
func Handler(s *SMS, cfg ServerConfig) http.Handler {
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 120 * time.Second
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), cfg.CallTimeout)
		defer cancel()
		st, err := s.Status(ctx)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})

	mux.HandleFunc("GET /api/sms", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), cfg.CallTimeout)
		defer cancel()
		msgs, err := s.List(ctx)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if d := r.URL.Query().Get("direction"); d != "" {
			if d != string(Sent) && d != string(Received) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "direction must be 'sent' or 'received'"})
				return
			}
			filtered := make([]Message, 0, len(msgs))
			for _, m := range msgs {
				if string(m.Direction) == d {
					filtered = append(filtered, m)
				}
			}
			msgs = filtered
		}
		writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
	})

	mux.HandleFunc("POST /api/sms", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			To      string `json:"to"`
			Body    string `json:"body"`
			Timeout *int   `json:"timeout"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if strings.TrimSpace(body.To) == "" || body.Body == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "both 'to' and 'body' are required"})
			return
		}
		timeout := 60
		if body.Timeout != nil {
			timeout = *body.Timeout
		}
		// The send call blocks until the modem confirms (up to `timeout`
		// seconds), so the request context must outlast it.
		sendDeadline := cfg.CallTimeout
		if d := time.Duration(timeout+60) * time.Second; d > sendDeadline {
			sendDeadline = d
		}
		ctx, cancel := context.WithTimeout(r.Context(), sendDeadline)
		defer cancel()
		if err := s.Send(ctx, body.To, body.Body, timeout); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"sent": true, "to": body.To})
	})

	mux.HandleFunc("POST /api/sms/read", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if body.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "'name' is required"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), cfg.CallTimeout)
		defer cancel()
		if err := s.MarkRead(ctx, body.Name); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"read": body.Name})
	})

	mux.HandleFunc("POST /api/sms/unread", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if body.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "'name' is required"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), cfg.CallTimeout)
		defer cancel()
		if err := s.MarkUnread(ctx, body.Name); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"unread": body.Name})
	})

	mux.HandleFunc("DELETE /api/sms", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query param 'name' is required"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), cfg.CallTimeout)
		defer cancel()
		remaining, err := s.Delete(ctx, name)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": name, "messages": remaining})
	})

	return withAuth(cfg.AuthToken, mux)
}

func withAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && strings.HasPrefix(r.URL.Path, "/api/") {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtleCompare(got, token) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// subtleCompare returns 1 iff a and b are equal, in constant time.
func subtleCompare(a, b string) int {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b))
}
