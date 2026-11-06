package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// A module takes over from the config the API saved what it can change
// without a restart: camera.ui saves the config with every camera it adds,
// go2rtc doesn't restart for it.
func TestOnConfigSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "go2rtc.yaml")
	require.NoError(t, os.WriteFile(path, []byte("streams:\n  cam: rtsp://cam\n"), 0644))
	configPath := app.ConfigPath
	app.ConfigPath = path
	t.Cleanup(func() { app.ConfigPath = configPath })

	var saved []string
	handlers := configSaved
	OnConfigSave(func(data []byte) { saved = append(saved, string(data)) })
	t.Cleanup(func() { configSaved = handlers })

	send := func(method, body string) int {
		w := httptest.NewRecorder()
		configHandler(w, httptest.NewRequest(method, "/api/config", strings.NewReader(body)))
		return w.Code
	}

	// POST: the config as sent
	config := "streams:\n  cam: rtsp://cam\nonvif:\n  video_sources:\n    - profiles: [cam]\n"
	require.Equal(t, http.StatusOK, send("POST", config))
	require.Equal(t, []string{config}, saved)

	// PATCH: the config file with the patch
	require.Equal(t, http.StatusOK, send("PATCH", "onvif:\n  video_sources: []\n"))
	require.Len(t, saved, 2)
	var merged struct {
		Streams map[string]string `yaml:"streams"`
		Onvif   struct {
			VideoSources []any `yaml:"video_sources"`
		} `yaml:"onvif"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(saved[1]), &merged))
	require.Equal(t, "rtsp://cam", merged.Streams["cam"])
	require.NotNil(t, merged.Onvif.VideoSources)
	require.Empty(t, merged.Onvif.VideoSources)

	// a config that is no YAML is not saved, nobody is told
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, send("POST", "streams: ["))
	require.Len(t, saved, 2)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

// The modules read the saved config like at the start, with ${VAR} replaced.
// The file keeps the variable.
func TestOnConfigSaveReplacesVariables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "go2rtc.yaml")
	configPath := app.ConfigPath
	app.ConfigPath = path
	t.Cleanup(func() { app.ConfigPath = configPath })
	t.Setenv("GO2RTC_TEST_RTSP_PASSWORD", "secret")

	var saved []string
	handlers := configSaved
	OnConfigSave(func(data []byte) { saved = append(saved, string(data)) })
	t.Cleanup(func() { configSaved = handlers })

	send := func(method, body string) int {
		w := httptest.NewRecorder()
		configHandler(w, httptest.NewRequest(method, "/api/config", strings.NewReader(body)))
		return w.Code
	}

	// PATCH without a config file: the patch is the config
	require.Equal(t, http.StatusOK, send("PATCH", "rtsp:\n  password: ${GO2RTC_TEST_RTSP_PASSWORD}\n"))
	require.Equal(t, []string{"rtsp:\n    password: secret\n"}, saved)
	file, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(file), "${GO2RTC_TEST_RTSP_PASSWORD}")

	// a patch that is no YAML: refused, the file stays
	require.Equal(t, http.StatusBadRequest, send("PATCH", "rtsp: ["))
	require.Len(t, saved, 1)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, file, after)
}
