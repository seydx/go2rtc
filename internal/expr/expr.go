package expr

import (
	"errors"

	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/expr"
	"github.com/AlexxIT/go2rtc/pkg/shell"
)

func Init() {
	log := app.GetLogger("expr")

	streams.RedirectFunc("expr", func(url string) (string, error) {
		v, err := expr.Eval(url[5:], nil)
		if err != nil {
			return "", err
		}

		log.Debug().Msgf("[expr] url=%s", shell.Redact(url))

		if url = v.(string); url == "" {
			return "", errors.New("expr: result is empty")
		}

		return url, nil
	})
}
