package certs

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// HandleCerts handles GET (list) and POST (provision) for /api/v1/certs.
func (m *Manager) HandleCerts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m.ListCerts())
	case http.MethodPost:
		var body struct {
			Hostname string `json:"hostname"`
			Mode     string `json:"mode"` // "acme", "self-signed", "custom"
			CertPEM  string `json:"cert_pem"`
			KeyPEM   string `json:"key_pem"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Hostname == "" {
			writeErr(w, http.StatusBadRequest, "hostname is required")
			return
		}

		switch body.Mode {
		case "custom":
			if body.CertPEM == "" || body.KeyPEM == "" {
				writeErr(w, http.StatusBadRequest, "cert_pem and key_pem required for custom mode")
				return
			}
			if err := m.StoreCert(body.Hostname, []byte(body.CertPEM), []byte(body.KeyPEM)); err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
		case "self-signed":
			if err := m.ProvisionSelfSigned(body.Hostname); err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
		case "acme", "":
			if err := m.ProvisionACME(body.Hostname); err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
		default:
			writeErr(w, http.StatusBadRequest, "invalid mode: "+body.Mode)
			return
		}

		info, _ := m.GetCertInfo(body.Hostname)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(info)

	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		io.WriteString(w, `{"error":"method not allowed"}`)
	}
}

// HandleCert handles GET (info) and DELETE for /api/v1/certs/{hostname}.
func (m *Manager) HandleCert(w http.ResponseWriter, r *http.Request) {
	hostname := lastSeg(r.URL.Path)

	switch r.Method {
	case http.MethodGet:
		info, ok := m.GetCertInfo(hostname)
		if !ok {
			writeErr(w, http.StatusNotFound, "certificate not found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)

	case http.MethodDelete:
		m.DeleteCert(hostname)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"deleted"}`)

	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		io.WriteString(w, `{"error":"method not allowed"}`)
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func lastSeg(path string) string {
	path = strings.TrimRight(path, "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
