# ONVIF

## ONVIF Client

[`new in v1.5.0`](https://github.com/AlexxIT/go2rtc/releases/tag/v1.5.0)

The source is not very useful if you already know RTSP and snapshot links for your camera. But it can be useful if you don't.

**WebUI > Add** webpage supports ONVIF autodiscovery. Your server must be on the same subnet as the camera. If you use Docker, you must use "network host".

```yaml
streams:
  dahua1: onvif://admin:password@192.168.1.123
  reolink1: onvif://admin:password@192.168.1.123:8000
  tapo1: onvif://admin:password@192.168.1.123:2020
```

## ONVIF Server

The ONVIF server runs on a listener of its own, plain HTTP, and is off until `listen` is set. It is not part of the API.

```yaml
onvif:
  listen: ":8888"            # all interfaces
  # listen: "127.0.0.1:8888" # this host only
```

Clients connect to `http://<host>:8888/onvif/device_service`. The stream URIs point to the RTSP server (`rtsp.listen`) and need its credentials. The snapshot URIs point to `/onvif/snapshot?src=<stream>` on the same listener, it serves the streams of the config only.

The ONVIF server has no login: whoever reaches the listener sees the names of all streams. `SystemReboot` is not supported, an ONVIF client cannot restart go2rtc. An action the server doesn't have, `SystemReboot` or `GetUsers` say, gets the fault `ter:ActionNotSupported`.

A regular camera has a single video source (`GetVideoSources`) and two profiles (`GetProfiles`), its main and its sub stream.

By default go2rtc has one video source and one profile per stream, in the order of the stream names. `video_sources` groups the streams of a camera and sets the order, then only the listed streams are profiles:

```yaml
onvif:
  listen: ":8888"
  video_sources:
    - name: Front Door                          # optional, the name of its profiles
      profiles: [front_door, front_door_sub]    # streams, the highest quality first
    - token: garage                             # optional, the first profile by default
      profiles: [garage]
```

`video_sources: []` has no profiles at all. The `video_sources` of a config saved through the API (`POST` or `PATCH /api/config`) apply right away, without a restart: camera.ui saves its cameras that way. The other settings of the ONVIF server take effect with the next start.

The token of a profile is the name of its stream. A profile tells the codec (H264 or H265) and the resolution of its stream: what the stream sends, or sent the last time. A stream whose resolution is not known yet is read for it, for at most 5 s: a running stream from its next keyframe, a stopped one is dialed. That is a stream that has not run since go2rtc started, or one whose source sends the resolution with its keyframes only (Tapo). The profile keeps the resolution until the stream gets other sources. A camera that does not answer, or sends no keyframe in time, gets placeholders (H264 1920x1080) and is read again a minute later at the earliest.

The ONVIF integration of Home Assistant takes H.264 profiles only, it leaves out an H.265 stream.

## Tested clients

Go2rtc works as ONVIF server:

- Happytime onvif client (windows)
- Home Assistant ONVIF integration (linux)
- Onvier (android)
- ONVIF Device Manager (windows)

PS. Supports only TCP transport for RTSP protocol. UDP and HTTP transports - unsupported yet.

## Tested cameras

Go2rtc works as ONVIF client:

- Dahua IPC-K42
- OpenIPC
- Reolink RLC-520A
- TP-Link Tapo TC60
