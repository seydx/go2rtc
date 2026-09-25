package tutk

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Based on AlexxIT/go2rtc#2498.

func videoFragment(frameType byte, total, index uint16, payload []byte, frameSize uint32) []byte {
	headerSize := 28
	if frameType&8 != 0 {
		headerSize = 36
	}
	b := make([]byte, headerSize)
	b[0], b[1] = ChannelPVideo, frameType
	inner := b[headerSize-20:]
	inner[0], inner[1] = ChannelPVideo, frameType&1
	binary.LittleEndian.PutUint16(inner[4:], total)
	if frameType&1 != 0 {
		index = frameInfoSize
	}
	binary.LittleEndian.PutUint16(inner[6:], index)
	binary.LittleEndian.PutUint32(inner[16:], 1)
	b = append(b, payload...)
	if frameType&1 != 0 {
		fi := make([]byte, frameInfoSize)
		fi[0] = CodecH264
		binary.LittleEndian.PutUint32(fi[8:], 50000)
		binary.LittleEndian.PutUint32(fi[16:], frameSize)
		binary.LittleEndian.PutUint32(fi[20:], 123)
		b = append(b, fi...)
	}
	binary.LittleEndian.PutUint16(b[headerSize-12:], uint16(len(b)-headerSize))
	return b
}

// withFrameNo sets the frame number of the outer header, videoFragment always
// uses frame 1.
func withFrameNo(b []byte, frameNo uint32) []byte {
	headerSize := 28
	if b[1]&8 != 0 {
		headerSize = 36
	}
	binary.LittleEndian.PutUint32(b[headerSize-4:], frameNo)
	return b
}

func TestExtendedEndFragment(t *testing.T) {
	// HL_BC sends 0x09 at the end of multi-packet P-frames too. The 0x28
	// field is the FRAMEINFO length, not packet index 40.
	for _, frameType := range []byte{FrameTypeEndSingle, FrameTypeEndMulti, FrameTypeStartAlt, FrameTypeEndExt} {
		t.Run(fmt.Sprintf("0x%02x", frameType), func(t *testing.T) {
			h := NewFrameHandler(false)
			defer h.Close()
			h.Handle(videoFragment(FrameTypeCont, 3, 0, []byte("first"), 0))
			h.Handle(videoFragment(FrameTypeStart, 3, 1, []byte("second"), 0))
			end := videoFragment(frameType, 3, 2, []byte("last"), 15)
			hdr := ParsePacketHeader(end)
			require.Equal(t, uint16(2), hdr.PktIdx)
			require.True(t, hdr.HasFrameInfo)
			h.Handle(end)
			select {
			case pkt := <-h.Recv():
				require.Equal(t, []byte("firstsecondlast"), pkt.Payload)
				require.Equal(t, uint32(123), pkt.FrameNo)
				require.Equal(t, CodecH264, pkt.Codec)
			default:
				t.Fatal("complete frame was dropped")
			}
		})
	}
}

func TestContinuationPacketIndex40(t *testing.T) {
	for _, frameType := range []byte{FrameTypeCont, FrameTypeContAlt, FrameTypeStart} {
		hdr := ParsePacketHeader(videoFragment(frameType, 100, 40, []byte("fragment"), 0))
		require.Equal(t, uint16(40), hdr.PktIdx)
		require.False(t, hdr.HasFrameInfo)
	}
}

func TestSingleExtendedFrame(t *testing.T) {
	h := NewFrameHandler(false)
	defer h.Close()
	h.Handle(videoFragment(FrameTypeStartAlt, 1, 0, []byte("single"), 6))
	select {
	case pkt := <-h.Recv():
		require.Equal(t, []byte("single"), pkt.Payload)
	default:
		t.Fatal("single packet frame was dropped")
	}

	// Fork: a single packet 0x09 frame carries a FRAMEINFO even without the
	// size in the packet index field, as before.
	pkt := withFrameNo(videoFragment(FrameTypeStartAlt, 1, 0, []byte("single"), 6), 2)
	binary.LittleEndian.PutUint16(pkt[22:], 0)
	h.Handle(pkt)
	select {
	case pkt := <-h.Recv():
		require.Equal(t, []byte("single"), pkt.Payload)
	default:
		t.Fatal("single packet frame without marker was dropped")
	}
}

// Fork: a recording must get every frame of an HL_BC stream, whichever end
// packet type the camera picks for it.
func TestExtendedEndStreamKeepsEveryFrame(t *testing.T) {
	h := NewFrameHandler(false)
	defer h.Close()

	var want [][]byte
	for frameNo := uint32(1); frameNo <= 20; frameNo++ {
		end := FrameTypeStartAlt
		if frameNo%3 == 0 {
			end = FrameTypeEndExt
		}
		a := bytes.Repeat([]byte{byte(frameNo)}, 700)
		b := bytes.Repeat([]byte{byte(frameNo + 100)}, 300)
		h.Handle(withFrameNo(videoFragment(FrameTypeStart, 2, 0, a, 0), frameNo))
		h.Handle(withFrameNo(videoFragment(end, 2, 1, b, 1000), frameNo))
		want = append(want, append(a, b...))
	}

	var got [][]byte
	for len(h.Recv()) > 0 {
		got = append(got, (<-h.Recv()).Payload)
	}
	require.Equal(t, want, got)
}

// Fork: upstream strips a FRAMEINFO from every 0x09 packet. Only a packet whose
// header announces one (or a single packet frame, as before) carries it, the
// payload of any other 0x09 packet must stay intact even if its bytes happen
// to look like a FRAMEINFO.
func TestExtendedFragmentWithoutFrameInfoKeepsPayload(t *testing.T) {
	h := NewFrameHandler(false)
	defer h.Close()

	mid := bytes.Repeat([]byte{0xaa}, 100)
	mid[len(mid)-frameInfoSize] = CodecH264 // looks like a FRAMEINFO codec ID

	pkt := videoFragment(FrameTypeStartAlt, 3, 1, nil, 0)
	pkt = pkt[:36] // drop the FRAMEINFO videoFragment adds for end types
	binary.LittleEndian.PutUint16(pkt[22:], 1)
	pkt = append(pkt, mid...)
	binary.LittleEndian.PutUint16(pkt[24:], uint16(len(mid)))

	hdr := ParsePacketHeader(pkt)
	require.Equal(t, uint16(1), hdr.PktIdx)
	require.False(t, hdr.HasFrameInfo)

	h.Handle(videoFragment(FrameTypeCont, 3, 0, []byte("first"), 0))
	h.Handle(pkt)
	h.Handle(videoFragment(FrameTypeEndExt, 3, 2, []byte("last"), uint32(5+len(mid)+4)))
	select {
	case got := <-h.Recv():
		require.Equal(t, append(append([]byte("first"), mid...), "last"...), got.Payload)
	default:
		t.Fatal("frame with a 0x09 middle packet was dropped")
	}
}
