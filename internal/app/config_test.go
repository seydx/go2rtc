package app

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func useConfig(t *testing.T, content string, mode os.FileMode) string {
	path := filepath.Join(t.TempDir(), "go2rtc.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
	configPath := ConfigPath
	ConfigPath = path
	t.Cleanup(func() { ConfigPath = configPath })
	return path
}

func save(t *testing.T, content string) {
	_, err := UpdateConfig(func([]byte) ([]byte, error) { return []byte(content), nil })
	require.NoError(t, err)
}

func skipWithoutFileModes(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs file modes that hold for this user")
	}
}

// A save puts a new file in the place of the old one: the content is new,
// the mode is the old one and nothing is left next to it.
func TestConfigSaveReplacesFile(t *testing.T) {
	path := useConfig(t, "streams:\n  cam: rtsp://old\n", 0600)
	// a second name of the old file: it keeps the old content when the file is replaced
	oldFile := filepath.Join(t.TempDir(), "old.yaml")
	require.NoError(t, os.Link(path, oldFile))

	save(t, "streams:\n  cam: rtsp://new\n")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "streams:\n  cam: rtsp://new\n", string(data))
	data, err = os.ReadFile(oldFile)
	require.NoError(t, err)
	require.Equal(t, "streams:\n  cam: rtsp://old\n", string(data))

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

// camera.ui saves the config with every start, mostly the one that is there.
func TestConfigSaveSkipsUnchanged(t *testing.T) {
	path := useConfig(t, "streams:\n  cam: rtsp://cam\n", 0644)
	before, err := os.Stat(path)
	require.NoError(t, err)

	save(t, "streams:\n  cam: rtsp://cam\n")
	require.NoError(t, PatchConfig([]string{"streams", "cam"}, "rtsp://cam"))

	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	require.Equal(t, before.ModTime(), after.ModTime())
}

func TestConfigPatchReplacesFile(t *testing.T) {
	path := useConfig(t, "streams:\n  cam: rtsp://old\n", 0600)

	require.NoError(t, PatchConfig([]string{"streams", "cam"}, "rtsp://new"))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "streams:\n  cam: rtsp://new\n", string(data))

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestConfigSaveCreatesFile(t *testing.T) {
	path := useConfig(t, "", 0644)
	require.NoError(t, os.Remove(path))

	save(t, "streams:\n  cam: rtsp://cam\n")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "streams:\n  cam: rtsp://cam\n", string(data))
}

// The config can be a link into another folder: the link stays, its file gets the content.
func TestConfigSaveFollowsLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need a privilege on Windows")
	}
	target := useConfig(t, "streams:\n  cam: rtsp://old\n", 0600)
	link := filepath.Join(t.TempDir(), "go2rtc.yaml")
	require.NoError(t, os.Symlink(target, link))
	ConfigPath = link

	save(t, "streams:\n  cam: rtsp://new\n")

	info, err := os.Lstat(link)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "streams:\n  cam: rtsp://new\n", string(data))
}

// A temp file left by a killed save doesn't pass its mode to the config.
func TestConfigSaveIgnoresLeftTempFile(t *testing.T) {
	skipWithoutFileModes(t)
	path := useConfig(t, "rtsp:\n  password: old\n", 0600)
	require.NoError(t, os.WriteFile(path+".tmp", []byte("left"), 0644))

	save(t, "rtsp:\n  password: new\n")

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	require.NoFileExists(t, path+".tmp")
}

// A config the admin made read-only is not replaced by a writable one.
func TestConfigSaveKeepsReadOnlyFile(t *testing.T) {
	skipWithoutFileModes(t)
	path := useConfig(t, "streams:\n  cam: rtsp://old\n", 0444)

	_, err := UpdateConfig(func([]byte) ([]byte, error) { return []byte("streams: {}\n"), nil })
	require.ErrorIs(t, err, os.ErrPermission)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "streams:\n  cam: rtsp://old\n", string(data))
	require.NoFileExists(t, path+".tmp")
}

// A folder that takes no new file: the config is written in place.
func TestConfigSaveInReadOnlyFolder(t *testing.T) {
	skipWithoutFileModes(t)
	path := useConfig(t, "streams:\n  cam: rtsp://old\n", 0644)
	dir := filepath.Dir(path)
	require.NoError(t, os.Chmod(dir, 0555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0755) })
	before, err := os.Stat(path)
	require.NoError(t, err)

	save(t, "streams:\n  cam: rtsp://new\n")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "streams:\n  cam: rtsp://new\n", string(data))
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
}
