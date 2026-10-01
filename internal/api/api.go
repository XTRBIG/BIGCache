// Package api exposes statistics and control operations over HTTP/JSON.
//
//	GET  /api/v1/stats                    global and per-volume statistics
//	GET  /api/v1/volumes                  volume list
//	POST /api/v1/flush[?volume=NAME]      write dirty blocks back now
//	POST /api/v1/drop[?volume=NAME]       drop clean cached blocks
//	GET  /healthz
package api

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/xtrbig/bigcache/internal/cache"
)

// Handler builds the HTTP mux for a cache.
func Handler(c *cache.Cache, logger *log.Logger) http.Handler {
	if logger == nil {
		logger = log.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /api/v1/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, c.Stats())
	})
	mux.HandleFunc("GET /api/v1/volumes", func(w http.ResponseWriter, r *http.Request) {
		vols := c.Volumes()
		out := make([]cache.VolumeStats, 0, len(vols))
		for _, v := range vols {
			out = append(out, v.Stats())
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /api/v1/flush", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("volume")
		var n int
		var err error
		if name == "" {
			n, err = c.Flush()
		} else {
			v := c.Volume(name)
			if v == nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown volume " + name})
				return
			}
			n, err = v.Flush()
		}
		if err != nil {
			logger.Printf("api: flush: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"flushed": n, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"flushed": n})
	})
	mux.HandleFunc("POST /api/v1/drop", func(w http.ResponseWriter, r *http.Request) {
		n, err := c.DropClean(r.URL.Query().Get("volume"))
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"dropped": n})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
