package tutk

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// Based on AlexxIT/go2rtc#2433. Unlike upstream, frames are only checked against
// the buffer bounds: a frame longer than announced (ex. padding in the last
// chunk) is still delivered, go2rtc feeds NVRs and must not drop decodable frames.

func session16Media(cmdType byte, frameSeq, chunkSeq, hdrSize uint16, payloadSize uint32, data []byte) []byte {
	cmd := make([]byte, cmdHdrSize, cmdHdrSize+len(data))
	cmd[0], cmd[1] = 0x01, cmdType
	binary.LittleEndian.PutUint16(cmd[4:], frameSeq)
	binary.LittleEndian.PutUint32(cmd[8:], payloadSize)
	binary.LittleEndian.PutUint16(cmd[12:], chunkSeq)
	binary.LittleEndian.PutUint16(cmd[14:], hdrSize)
	return append(cmd, data...)
}

func TestSession16RejectsFragmentWithPayloadPastBuffer(t *testing.T) {
	s := NewSession16(nil, make([]byte, 8))
	s.waitFSeq = 1
	s.waitCSeq = 1
	s.waitSize = 2048
	s.waitData = make([]byte, 2048)

	var got int
	require.NotPanics(t, func() {
		got = s.SessionRead(0, session16Media(0x03, 1, 1, 0, 2220, nil))
	})
	require.Equal(t, msgMediaLost, got)
}

func TestSession16RejectsTruncatedMediaHeaders(t *testing.T) {
	s := NewSession16(nil, make([]byte, 8))

	var got int
	require.NotPanics(t, func() { got = s.SessionRead(0, []byte{0x01, 0x03}) })
	require.Equal(t, msgMediaLost, got)

	// single packet frame announcing a header longer than its data
	require.NotPanics(t, func() { got = s.SessionRead(0, session16Media(0x04, 0, 0, 1, 0, nil)) })
	require.Equal(t, msgMediaLost, got)

	// an intact single packet frame still goes through
	require.Equal(t, msgMediaFrame, s.SessionRead(0, session16Media(0x04, 0, 0, 2, 0, []byte{0xa1, 0xa2, 9, 9})))
	frameInfo, frameData, err := s.RecvFrameData()
	require.NoError(t, err)
	require.Equal(t, []byte{0xa1, 0xa2}, frameInfo)
	require.Equal(t, []byte{9, 9}, frameData)
}

// The client start ack echoes the last 32 bytes of the message, a shorter one
// used to slice with a negative index.
func TestSession16RejectsTruncatedClientStart(t *testing.T) {
	s := NewSession16(nil, make([]byte, 8))

	var got int
	require.NotPanics(t, func() { got = s.SessionRead(1, make([]byte, cmdHdrSize+4)) })
	require.Equal(t, msgUnknown, got)
}

// A frame longer than announced, ex. padding in the last chunk, is delivered as
// before. Only sizes past the assembled buffer are rejected.
func TestSession16DeliversFrameWithPaddedLastChunk(t *testing.T) {
	s := NewSession16(nil, make([]byte, 8))

	payload := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	hdr := []byte{0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6}
	last := append(append([]byte{}, hdr...), 0, 0, 0) // header plus padding

	require.Equal(t, msgMediaChunk, s.SessionRead(0, session16Media(0x03, 7, 0, uint16(len(hdr)), uint32(len(payload)), payload)))
	require.Equal(t, msgMediaFrame, s.SessionRead(0, session16Media(0x03, 7, 1, uint16(len(hdr)), uint32(len(payload)), last)))

	frameInfo, frameData, err := s.RecvFrameData()
	require.NoError(t, err)
	require.Equal(t, payload, frameData)
	require.True(t, bytes.HasPrefix(frameInfo, hdr))
}
