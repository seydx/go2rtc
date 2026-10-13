package tuya

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSmartApiRegions(t *testing.T) {
	for _, host := range []string{"protect-eu.ismartlife.me", "protect-in.ismartlife.me", "protect-sg.ismartlife.me"} {
		client, err := NewTuyaSmartApiClient(nil, host, "user@example.com", "secret", "device")
		require.NoError(t, err, host)
		require.NotEmpty(t, client.countryCode)
	}

	_, err := NewTuyaSmartApiClient(nil, "protect-xx.ismartlife.me", "user@example.com", "secret", "device")
	require.EqualError(t, err, "invalid region: protect-xx.ismartlife.me")
}
