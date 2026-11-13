package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/AlexxIT/go2rtc/pkg/creds"
	"github.com/AlexxIT/go2rtc/pkg/yaml"
)

func LoadConfig(v any) {
	for _, data := range configs {
		if err := yaml.Unmarshal(data, v); err != nil {
			Logger.Warn().Err(err).Send()
		}
	}
}

var configMu sync.Mutex

func PatchConfig(path []string, value any) error {
	if ConfigPath == "" {
		return errors.New("config file disabled")
	}

	configMu.Lock()
	defer configMu.Unlock()

	// empty config is OK
	old, readErr := os.ReadFile(ConfigPath)

	b, err := yaml.Patch(old, path, value)
	if err != nil {
		// deleting a path that doesn't exist is a no-op, not an error
		if value == nil && errors.Is(err, yaml.ErrPathNotExist) {
			return nil
		}
		return err
	}

	if readErr == nil && bytes.Equal(old, b) {
		return nil
	}

	return writeConfig(b)
}

// UpdateConfig writes what update makes of the config file, under the lock of
// PatchConfig, and returns the written config
func UpdateConfig(update func(data []byte) ([]byte, error)) ([]byte, error) {
	if ConfigPath == "" {
		return nil, errors.New("config file disabled")
	}

	configMu.Lock()
	defer configMu.Unlock()

	// empty config is OK
	old, readErr := os.ReadFile(ConfigPath)

	b, err := update(old)
	if err != nil {
		return nil, err
	}

	if readErr == nil && bytes.Equal(old, b) {
		return b, nil
	}

	return b, writeConfig(b)
}

func writeConfig(data []byte) error {
	path := ConfigPath
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}

	err := replaceFile(path, data)
	if errors.Is(err, errReplaceRefused) {
		return os.WriteFile(path, data, 0644)
	}
	return err
}

var errReplaceRefused = errors.New("config: file can't be replaced")

// a crash leaves the old file or the new one, never an empty one
func replaceFile(path string, data []byte) error {
	mode := os.FileMode(0644)
	old, _ := os.Stat(path)
	if old != nil {
		mode = old.Mode().Perm()

		// a file that can't be written in place is not replaced either
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return errReplaceRefused
		}
		_ = f.Close()
	}

	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		// a folder without write access
		if errors.Is(err, os.ErrPermission) {
			return errReplaceRefused
		}
		return err
	}

	if old != nil {
		// the umask cuts the mode of OpenFile, and the file of another owner stays theirs
		if f.Chmod(mode) != nil || keepOwner(f, old) != nil {
			err = errReplaceRefused
		}
	}

	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	// a file mounted on its own can't be renamed over
	if err == nil && os.Rename(tmp, path) != nil {
		err = errReplaceRefused
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

type flagConfig []string

func (c *flagConfig) String() string {
	return strings.Join(*c, " ")
}

func (c *flagConfig) Set(value string) error {
	*c = append(*c, value)
	return nil
}

var configs [][]byte

func initConfig(confs flagConfig) {
	if confs == nil {
		confs = []string{"go2rtc.yaml"}
	}

	for _, conf := range confs {
		if len(conf) == 0 {
			continue
		}
		if conf[0] == '{' {
			// config as raw YAML or JSON
			configs = append(configs, []byte(conf))
		} else if data := parseConfString(conf); data != nil {
			configs = append(configs, data)
		} else {
			// config as file
			if ConfigPath == "" {
				ConfigPath = conf
				initStorage()
			}

			if data, _ = os.ReadFile(conf); data == nil {
				continue
			}

			loadEnv(data)
			data = creds.ReplaceVars(data)
			configs = append(configs, data)
		}
	}

	if ConfigPath != "" {
		if !filepath.IsAbs(ConfigPath) {
			if cwd, err := os.Getwd(); err == nil {
				ConfigPath = filepath.Join(cwd, ConfigPath)
			}
		}
		Info["config_path"] = ConfigPath
	}
}

func parseConfString(s string) []byte {
	i := strings.IndexByte(s, '=')
	if i < 0 {
		return nil
	}

	items := strings.Split(s[:i], ".")
	if len(items) < 2 {
		return nil
	}

	// `log.level=trace` => `{log: {level: trace}}`
	var pre string
	var suf = s[i+1:]
	for _, item := range items {
		pre += "{" + item + ": "
		suf += "}"
	}

	return []byte(pre + suf)
}
