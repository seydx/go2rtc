package api

import (
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/pkg/creds"
	"gopkg.in/yaml.v3"
)

func configHandler(w http.ResponseWriter, r *http.Request) {
	if app.ConfigPath == "" {
		http.Error(w, "", http.StatusGone)
		return
	}

	switch r.Method {
	case "GET":
		data, err := os.ReadFile(app.ConfigPath)
		if err != nil {
			http.Error(w, "", http.StatusNotFound)
			return
		}
		// Mask credentials/secrets like /api/streams does, so the raw config
		// (RTSP passwords, API password) isn't returned in plaintext.
		w = creds.SecretResponse(w)
		// https://www.ietf.org/archive/id/draft-ietf-httpapi-yaml-mediatypes-00.html
		Response(w, data, "application/yaml")

	case "POST", "PATCH":
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var update func(config []byte) ([]byte, error)
		if r.Method == "PATCH" {
			var patch map[string]any
			if err = yaml.Unmarshal(data, &patch); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			update = func(config []byte) ([]byte, error) {
				return mergeYAML(config, patch)
			}
		} else {
			// validate config
			if err = yaml.Unmarshal(data, map[string]any{}); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			update = func([]byte) ([]byte, error) {
				return data, nil
			}
		}

		// the modules take the saves in their order
		saveMu.Lock()
		defer saveMu.Unlock()

		if data, err = app.UpdateConfig(update); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// the modules read ${VAR} replaced, like at the start
		data = creds.ReplaceVars(data)

		configSavedMu.Lock()
		handlers := configSaved
		configSavedMu.Unlock()
		for _, handler := range handlers {
			handler(data)
		}
	}
}

var saveMu sync.Mutex

var configSaved []func(data []byte)
var configSavedMu sync.Mutex

// OnConfigSave calls handler with the config file the API saved (POST or
// PATCH api/config): a module takes over what it can change without a
// restart. The config file is the main one, not the others of the command line.
func OnConfigSave(handler func(data []byte)) {
	configSavedMu.Lock()
	configSaved = append(configSaved, handler)
	configSavedMu.Unlock()
}

func mergeYAML(data []byte, patch map[string]any) ([]byte, error) {
	config := map[string]any{}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	config = merge(config, patch)

	return yaml.Marshal(&config)
}

func merge(dst, src map[string]any) map[string]any {
	for k, v := range src {
		if vv, ok := dst[k]; ok {
			switch vv := vv.(type) {
			case map[string]any:
				v := v.(map[string]any)
				dst[k] = merge(vv, v)
			case []any:
				v := v.([]any)
				dst[k] = v
			default:
				dst[k] = v
			}
		} else {
			dst[k] = v
		}
	}
	return dst
}
