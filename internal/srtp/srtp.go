package srtp

import (
	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/pkg/srtp"
	"github.com/AlexxIT/go2rtc/pkg/yaml"
	"github.com/rs/zerolog"
)

func Init() {
	cfg := defaultConfig()

	// load config from YAML
	app.LoadConfig(&cfg)

	log = app.GetLogger("srtp")

	if cfg.Mod.Listen == "" {
		return
	}

	// create SRTP server (endpoint) for receiving video from HomeKit cameras
	Server = srtp.NewServer(cfg.Mod.Listen)

	api.OnConfigSave(reloadAddress)
}

var Server *srtp.Server

var log zerolog.Logger

type config struct {
	Mod struct {
		Listen string `yaml:"listen"`
	} `yaml:"srtp"`
}

func defaultConfig() config {
	var cfg config
	cfg.Mod.Listen = ":8443"
	return cfg
}

// reloadAddress takes the address of the config the API saved for the next
// sessions. Turning the server on or off takes a restart.
func reloadAddress(data []byte) {
	cfg := defaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Warn().Err(err).Msg("[srtp] listen of the saved config")
		return
	}
	if cfg.Mod.Listen != "" {
		Server.SetAddress(cfg.Mod.Listen)
	}
}
