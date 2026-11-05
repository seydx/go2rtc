# Hass

Support import camera links from [Home Assistant](https://www.home-assistant.io/) config files:

- [Generic Camera](https://www.home-assistant.io/integrations/generic/), setup via GUI
- [HomeKit Camera](https://www.home-assistant.io/integrations/homekit_controller/)
- [ONVIF](https://www.home-assistant.io/integrations/onvif/)
- [Roborock](https://github.com/humbertogontijo/homeassistant-roborock) vacuums with camera

## Configuration

```yaml
hass:
  config: "/config"  # skip this setting if you are a Home Assistant add-on user

streams:
  generic_camera: hass:Camera1  # Settings > Integrations > Integration Name
  aqara_g3: hass:Camera-Hub-G3-AB12
```

### WebRTC Cameras

[`new in v1.6.0`](https://github.com/AlexxIT/go2rtc/releases/tag/v1.6.0)

Every camera that Home Assistant offers over WebRTC is supported: cameras with native WebRTC like [Nest](https://www.home-assistant.io/integrations/nest/), and every other streaming camera through Home Assistant's own go2rtc. It uses the WebRTC API of Home Assistant 2024.11 and newer, and the connection to Home Assistant stays open while the stream runs.

```yaml
streams:
  # link to Home Assistant Supervised
  hass-webrtc1: hass://supervisor?entity_id=camera.nest_doorbell
  # link to external Home Assistant with Long-Lived Access Tokens
  hass-webrtc2: hass://192.168.1.123:8123?entity_id=camera.nest_doorbell&token=eyXYZ...
  # Home Assistant behind HTTPS
  hass-webrtc3: hass://ha.example.com?entity_id=camera.nest_doorbell&token=eyXYZ...&tls=1
  # two-way audio, if the camera and its integration support it
  hass-webrtc4: hass://supervisor?entity_id=camera.front_door#backchannel=1
```

Two-way audio through Home Assistant came with 2026.11 and needs the Labs feature "Two-way audio".

### RTSP Cameras

By default, the Home Assistant API does not allow you to get a dynamic RTSP link to a camera stream. [This method](https://github.com/felipecrs/hass-expose-camera-stream-source#importing-cameras-from-home-assistant-to-go2rtc-or-frigate) can work around it.
