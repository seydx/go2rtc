package streams

import (
	"sync"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

// Modules register their schemes at startup, but tests (and any late
// registration) do it while streams already resolve sources. A concurrent
// map read/write is a hard crash, so the registry has to be guarded.
// Meaningful under -race.
func TestHandlerRegistryIsConcurrencySafe(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		for range 500 {
			HandleFunc("stubscheme", func(string) (core.Producer, error) { return nil, nil })
		}
	}()
	go func() {
		defer wg.Done()
		for range 500 {
			_, _ = GetProducer("stubscheme://host")
		}
	}()
	go func() {
		defer wg.Done()
		for range 500 {
			_ = SupportedSchemes()
		}
	}()

	wg.Wait()
	require.Contains(t, SupportedSchemes(), "stubscheme")
}

// A handler that returns neither a producer nor an error must not hand a nil
// producer to a reconnect, which would dereference it.
func TestGetProducerRejectsNilProducer(t *testing.T) {
	HandleFunc("nilproducer", func(string) (core.Producer, error) { return nil, nil })

	prod, err := GetProducer("nilproducer://camera")
	require.Nil(t, prod)
	require.Error(t, err)
}
