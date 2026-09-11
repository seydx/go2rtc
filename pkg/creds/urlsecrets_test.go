package creds

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Based on AlexxIT/go2rtc#2455. Registered secrets are global, so every test
// uses values that appear nowhere else.

func TestAddURLSecrets(t *testing.T) {
	// cloud-style source: credentials as query parameters
	src := "nest:?client_id=CLIENT-ID-ABC&client_secret=GOCSPX-verysecretvalue&refresh_token=1//0refreshTOKEN&project_id=proj-123"
	AddURLSecrets(src)

	got := SecretString("[streams] start producer url=" + src)
	require.NotContains(t, got, "GOCSPX-verysecretvalue")
	require.NotContains(t, got, "1//0refreshTOKEN")
	// identifying detail survives so diagnostics still make sense
	require.Contains(t, got, "client_id=CLIENT-ID-ABC")
	require.Contains(t, got, "project_id=proj-123")
	require.Contains(t, got, "client_secret=***")
	require.Contains(t, got, "refresh_token=***")

	// JSON serialisation of the same URL (what /api/streams emits) is masked too
	require.NotContains(t, SecretString(`{"url":"`+src+`"}`), "GOCSPX-verysecretvalue")
}

func TestAddURLSecretsShortValueIgnored(t *testing.T) {
	// a short value must never become a secret: it would mask innocent text
	AddURLSecrets("roborock://?key=1234&password=ab")
	require.Equal(t, "port 1234 password ab", SecretString("port 1234 password ab"))
}

func TestAddURLSecretsNoQueryOrGarbage(t *testing.T) {
	// no-ops, and no panics
	AddURLSecrets("rtsp://192.168.1.10/stream")
	AddURLSecrets("")
	AddURLSecrets("nest:?%zz=bad&client_secret=stillregisteredvalue&novalue&=empty")
	// the malformed pair is skipped, the rest is still registered
	require.NotContains(t, SecretString("x=stillregisteredvalue"), "stillregisteredvalue")
}

// Fork specific tests.

// go2rtc params follow the query after #, they are not part of the value.
func TestAddURLSecretsStripsFragment(t *testing.T) {
	AddURLSecrets("ring:?refresh_token=RING-REFRESH-TOKEN-1#media=video#noBackchannel")
	require.Equal(t, "token ***", SecretString("token RING-REFRESH-TOKEN-1"))
}

// exec and ffmpeg sources are command lines that can carry several URLs.
func TestAddURLSecretsCommandLine(t *testing.T) {
	AddURLSecrets(`exec:ffmpeg -i "rtsp://cam/live?token=EXEC-TOKEN-ONE" -i http://cam/snap?api_key=EXEC-API-KEY-TWO -c copy -f rtsp {output}`)
	require.Equal(t, "*** ***", SecretString("EXEC-TOKEN-ONE EXEC-API-KEY-TWO"))
}

// Device keys of the fork's sources, while identifiers and public keys stay.
func TestAddURLSecretsDeviceKeys(t *testing.T) {
	wyze := "wyze://192.168.1.5?uid=WYZE-UID-123456&enr=WYZE-ENR-SECRET&mac=AABBCCDDEEFF&model=HL_CAM4&dtls=true"
	AddURLSecrets(wyze)
	got := SecretString(wyze)
	require.NotContains(t, got, "WYZE-ENR-SECRET")
	require.Contains(t, got, "uid=WYZE-UID-123456")
	require.Contains(t, got, "mac=AABBCCDDEEFF")

	xiaomi := "xiaomi://1234567890@192.168.1.6?client_public=XIAOMI-CLIENT-PUBLIC&client_private=XIAOMI-CLIENT-PRIVATE&device_public=XIAOMI-DEVICE-PUBLIC&sign=XIAOMI-LOGIN-SIGN"
	AddURLSecrets(xiaomi)
	got = SecretString(xiaomi)
	require.NotContains(t, got, "XIAOMI-CLIENT-PRIVATE")
	require.NotContains(t, got, "XIAOMI-LOGIN-SIGN")
	require.Contains(t, got, "client_public=XIAOMI-CLIENT-PUBLIC")
	require.Contains(t, got, "device_public=XIAOMI-DEVICE-PUBLIC")
}

// Logs show the URL as written, so the encoded form must be masked as well as
// the decoded one.
func TestAddURLSecretsEncodedValue(t *testing.T) {
	AddURLSecrets("nest:?Client_Secret=ENC%2FSECRET%2Bvalue")
	require.Equal(t, "*** ***", SecretString("ENC%2FSECRET%2Bvalue ENC/SECRET+value"))
}

// Single letter keys are credentials for roborock only.
func TestAddURLSecretsSchemeSpecificKeys(t *testing.T) {
	AddURLSecrets("roborock://?u=ROBOROCK-USER-ID&s=ROBOROCK-S-SECRET&k=ROBOROCK-K-SECRET")
	require.Equal(t, "u=ROBOROCK-USER-ID s=*** k=***", SecretString("u=ROBOROCK-USER-ID s=ROBOROCK-S-SECRET k=ROBOROCK-K-SECRET"))

	AddURLSecrets("http://cam/api?s=stream_main_hd&k=keyframe_every_2s")
	require.Equal(t, "stream_main_hd keyframe_every_2s", SecretString("stream_main_hd keyframe_every_2s"))
}
