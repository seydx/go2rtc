# Streams

This core module is responsible for managing the stream list.

## Stream to camera

[`new in v1.3.0`](https://github.com/AlexxIT/go2rtc/releases/tag/v1.3.0)

go2rtc supports playing audio files (ex. music or [TTS](https://www.home-assistant.io/integrations/#text-to-speech)) and live streams (ex. radio) on cameras with [two-way audio](../../README.md#two-way-audio) support.

API example:

```text
POST http://localhost:1984/api/streams?dst=camera1&src=ffmpeg:http://example.com/song.mp3#audio=pcma#input=file
```

- you can stream: local files, web files, live streams or any format, supported by FFmpeg
- you should use [ffmpeg source](../ffmpeg/README.md) for transcoding audio to codec, that your camera supports
- you can check camera codecs on the go2rtc WebUI info page when the stream is active
- some cameras support only low quality `PCMA/8000` codec (ex. [Tapo](../tapo/README.md))
- it is recommended to choose higher quality formats if your camera supports them (ex. `PCMA/48000` for some Dahua cameras)
- if you play files over `http` link, you need to add `#input=file` params for transcoding, so the file will be transcoded and played in real time
- if you play live streams, you should skip `#input` param, because it is already in real time
- you can stop active playback by calling the API with the empty `src` parameter
- you will see one active producer and one active consumer in go2rtc WebUI info page during streaming

## Publish stream

[`new in v1.8.0`](https://github.com/AlexxIT/go2rtc/releases/tag/v1.8.0)

You can publish any stream to streaming services (YouTube, Telegram, etc.) via RTMP/RTMPS. Important:

- Supported codecs: H264 for video and AAC for audio
- AAC audio is required for YouTube; videos without audio will not work
- You don't need to enable [RTMP module](../rtmp/README.md) listening for this task

You can use the API:

```text
POST http://localhost:1984/api/streams?src=camera1&dst=rtmps://...
```

Or config file:

```yaml
publish:
  # publish stream "video_audio_transcode" to Telegram
  video_audio_transcode:
    - rtmps://xxx-x.rtmp.t.me/s/xxxxxxxxxx:xxxxxxxxxxxxxxxxxxxxxx
  # publish stream "audio_transcode" to Telegram and YouTube
  audio_transcode:
    - rtmps://xxx-x.rtmp.t.me/s/xxxxxxxxxx:xxxxxxxxxxxxxxxxxxxxxx
    - rtmp://xxx.rtmp.youtube.com/live2/xxxx-xxxx-xxxx-xxxx-xxxx

streams:
  video_audio_transcode:
    - ffmpeg:rtsp://user:pass@192.168.1.123/stream1#video=h264#hardware#audio=aac
  audio_transcode:
    - ffmpeg:rtsp://user:pass@192.168.1.123/stream1#video=copy#audio=aac
```

- **Telegram Desktop App** > Any public or private channel or group (where you admin) > Live stream > Start with... > Start streaming.
- **YouTube** > Create > Go live > Stream latency: Ultra low-latency > Copy: Stream URL + Stream key.

## Preload stream

[`new in v1.9.11`](https://github.com/AlexxIT/go2rtc/releases/tag/v1.9.11)

You can preload any stream on go2rtc start. This is useful for cameras that take a long time to start up.

A preload is always the first consumer of its stream: whenever a client starts the source, the preload is attached first, so the camera session is negotiated from the preload's query and the client reuses those tracks. The preload owns the source: while it can't attach (camera unreachable), clients get that error instead of dialing the camera themselves. A preload is also supervised: if the camera is unreachable when go2rtc starts, or the producer is lost later, go2rtc keeps re-attaching the preload with the same backoff as source reconnects. `GET /api/preload?src=camera1` reports `status` (`started` while registered, `stopped` otherwise), `attached` (currently holding live tracks) and the last `error`.

```yaml
preload:
  camera1:                                     # default: video&audio = ANY
  camera2: "video"                             # preload only video track
  camera3: "video=h264&audio=opus"             # preload H264 video and OPUS audio

streams:
  camera1: 
    - rtsp://192.168.1.100/stream
  camera2: 
    - rtsp://192.168.1.101/stream  
  camera3: 
    - rtsp://192.168.1.102/h265stream
    - ffmpeg:camera3#video=h264#audio=opus#hardware
```

## GOP Cache

go2rtc has a built-in [GOP cache](https://en.wikipedia.org/wiki/Group_of_pictures) for video tracks. It caches the current GOP (Group of Pictures) starting from the most recent keyframe. A new client receives the cached frames first, replayed at up to 100 fps with matching timestamps, so it can start decoding immediately instead of waiting for the next keyframe: the picture fast-forwards through the cached part and then continues live. Audio is never cached and stays in sync.

The GOP cache is **disabled by default** and only applies to **video codecs** (not audio). Enable it per stream:

```yaml
streams:
  unifi_camera: rtspx://192.168.1.123:7441/fD6ouM72bWoFijxK#gop=1
```

**All clients** (WebRTC, MSE, MP4, HLS, MJPEG, RTSP) can override the stream configuration using the `?gop=0` query parameter to disable GOP cache for that specific connection:

```
http://localhost:1984/stream.html?src=unifi_camera&gop=0
```

## Codec change

A client negotiates its codecs once (MSE init segment, WebRTC answer, RTSP DESCRIBE). When a camera is reconfigured to another codec at runtime (ex. H264 to H265, PCMU to AAC), the source reconnects and drops the tracks it can no longer serve. Every client that received one of them is disconnected, so it reconnects and negotiates the new codec: websockets close with code `1012`, HTTP, RTSP and RTMP connections end, HLS sessions are withdrawn. Clients without an affected track keep playing, ex. a video-only viewer on an audio codec change. A reconnect with unchanged codecs, or a camera coming back without audio, disconnects nobody.

## Offers

The stream info (`/api/streams`, `/api/streams?src=...`, a probe) carries `offers` next to `producers` and `consumers`: what a client can get from the stream across all of its sources, without matching the producers itself.

```json
"offers": {
  "state": "live",
  "video": [{ "codec": "H264", "rate": 90000, "fmtp": "packetization-mode=1;profile-level-id=640033", "payload_type": 96, "ffmpeg": "h264", "profile": "High", "level": 51, "native": true }],
  "audio": [
    { "codec": "MPEG4-GENERIC", "rate": 16000, "channels": 1, "fmtp": "config=1408", "payload_type": 97, "ffmpeg": "aac", "native": true },
    { "codec": "OPUS", "rate": 48000, "channels": 2, "payload_type": 98, "ffmpeg": "opus", "native": false }
  ],
  "backchannel": { "codecs": [{ "codec": "PCMA", "rate": 8000, "channels": 1, "payload_type": 8, "ffmpeg": "pcm_alaw", "native": true }], "transcode": true }
}
```

- Every source adds what it offers, in the order of the stream, the way a client is matched: a second source can bring the talk channel (`isapi`, `multitrans`, `doorbird`) or audio the first one lacks. A source with `#requirePrevAudio`/`#requirePrevVideo` counts only when an earlier source has that kind. `#noVideo`, `#noAudio` and `#noBackchannel` hide a kind of a source. Codecs are listed without duplicates. A source that is not running reports what its options produce, ex. `ffmpeg:...#audio=opus`.
- Audio always carries rate and channels, filled in from RTP where the source leaves them out (opus 48000/2, G.711 8000/1, one channel by default), and the static payload types of PCMA and G722. `profile` and `level` (times ten, 51 for 5.1) are set for H264 and H265, read from the SPS. `native` marks codecs of the stream's own sources, the others come from a source tied to an earlier one, which converts what that one sends.
- `transcode`: the backchannel accepts any codec from the client, the mixer converts it.
- `state`: `live` while the first source without `#requirePrev*` is connected, `cached` with the medias of its last session after it stopped, `unknown` if it never connected, then nothing else is listed either. Changing the source forgets the last session.

## Examples

```yaml
streams:
  # known RTSP sources
  rtsp-dahua1:   rtsp://admin:password@192.168.10.90/cam/realmonitor?channel=1&subtype=0&unicast=true&proto=Onvif
  rtsp-dahua2:   rtsp://admin:password@192.168.10.90/cam/realmonitor?channel=1&subtype=1
  rtsp-tplink1:  rtsp://admin:password@192.168.10.91/stream1
  rtsp-tplink2:  rtsp://admin:password@192.168.10.91/stream2
  rtsp-reolink1: rtsp://admin:password@192.168.10.92/h264Preview_01_main
  rtsp-reolink2: rtsp://admin:password@192.168.10.92/h264Preview_01_sub
  rtsp-sonoff1:  rtsp://admin:password@192.168.10.93/av_stream/ch0
  rtsp-sonoff2:  rtsp://admin:password@192.168.10.93/av_stream/ch1

  # known RTMP sources
  rtmp-reolink1: rtmp://192.168.10.92/bcs/channel0_main.bcs?channel=0&stream=0&user=admin&password=password
  rtmp-reolink2: rtmp://192.168.10.92/bcs/channel0_sub.bcs?channel=0&stream=1&user=admin&password=password
  rtmp-reolink3: rtmp://192.168.10.92/bcs/channel0_ext.bcs?channel=0&stream=1&user=admin&password=password

  # known HTTP sources
  http-reolink1: http://192.168.10.92/flv?port=1935&app=bcs&stream=channel0_main.bcs&user=admin&password=password
  http-reolink2: http://192.168.10.92/flv?port=1935&app=bcs&stream=channel0_sub.bcs&user=admin&password=password
  http-reolink3: http://192.168.10.92/flv?port=1935&app=bcs&stream=channel0_ext.bcs&user=admin&password=password

  # known ONVIF sources
  onvif-dahua1:   onvif://admin:password@192.168.10.90?subtype=MediaProfile00000
  onvif-dahua2:   onvif://admin:password@192.168.10.90?subtype=MediaProfile00001
  onvif-dahua3:   onvif://admin:password@192.168.10.90?subtype=MediaProfile00000&snapshot
  onvif-tplink1:  onvif://admin:password@192.168.10.91:2020?subtype=profile_1
  onvif-tplink2:  onvif://admin:password@192.168.10.91:2020?subtype=profile_2
  onvif-reolink1: onvif://admin:password@192.168.10.92:8000?subtype=000
  onvif-reolink2: onvif://admin:password@192.168.10.92:8000?subtype=001
  onvif-reolink3: onvif://admin:password@192.168.10.92:8000?subtype=000&snapshot
  onvif-openipc1: onvif://admin:password@192.168.10.95:80?subtype=PROFILE_000
  onvif-openipc2: onvif://admin:password@192.168.10.95:80?subtype=PROFILE_001

  # some EXEC examples
  exec-h264-pipe:   exec:ffmpeg -re -i bbb.mp4 -c copy -f h264 -
  exec-flv-pipe:    exec:ffmpeg -re -i bbb.mp4 -c copy -f flv -
  exec-mpegts-pipe: exec:ffmpeg -re -i bbb.mp4 -c copy -f mpegts -
  exec-adts-pipe:   exec:ffmpeg -re -i bbb.mp4 -c copy -f adts -
  exec-mjpeg-pipe:  exec:ffmpeg -re -i bbb.mp4 -c mjpeg -f mjpeg -
  exec-hevc-pipe:   exec:ffmpeg -re -i bbb.mp4 -c libx265 -preset superfast -tune zerolatency -f hevc -
  exec-wav-pipe:    exec:ffmpeg -re -i bbb.mp4 -c pcm_alaw -ar 8000 -ac 1 -f wav -
  exec-y4m-pipe:    exec:ffmpeg -re -i bbb.mp4 -c rawvideo -f yuv4mpegpipe -
  exec-pcma-pipe:   exec:ffmpeg -re -i numb.mp3 -c:a pcm_alaw -ar:a 8000 -ac:a 1 -f wav -
  exec-pcmu-pipe:   exec:ffmpeg -re -i numb.mp3 -c:a pcm_mulaw -ar:a 8000 -ac:a 1 -f wav -
  exec-s16le-pipe:  exec:ffmpeg -re -i numb.mp3 -c:a pcm_s16le -ar:a 16000 -ac:a 1 -f wav -

  # some FFmpeg examples
  ffmpeg-video-h264: ffmpeg:virtual?video#video=h264
  ffmpeg-video-4K:   ffmpeg:virtual?video&size=4K#video=h264
  ffmpeg-video-10s:  ffmpeg:virtual?video&duration=10#video=h264
  ffmpeg-video-src2: ffmpeg:virtual?video=testsrc2&size=2K#video=h264
```
