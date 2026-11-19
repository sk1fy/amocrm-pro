package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

const companyCabinetHost = "rakurscrm.amocrm.ru"

func companyCabinetActivation(status func(context.Context) (bool, error)) http.Handler {
	const origin = "https://" + companyCabinetHost
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Add("Vary", "Origin")
		if origins := r.Header.Values("Origin"); len(origins) != 1 || origins[0] != origin {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		if r.URL.RawQuery != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		activated, err := status(ctx)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"activated": activated})
	})
}
