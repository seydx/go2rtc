# Ring

[`new in v1.9.13`](https://github.com/AlexxIT/go2rtc/releases/tag/v1.9.13) by [@seydx](https://github.com/seydx)

This source type supports Ring cameras with [two-way audio](../../README.md#two-way-audio) support.

## Configuration

If you have a `refresh_token`, `camera_id` and `device_id`, you can use them in the `go2rtc.yaml` config file.

Otherwise, you can use the go2rtc web interface and add your Ring account (WebUI > Add > Ring). Once added, it will list all your Ring cameras.

```yaml
streams:
  ring: ring:?camera_id=XXX&device_id=XXX&refresh_token=XXX
  ring_snapshot: ring:?camera_id=XXX&device_id=XXX&refresh_token=XXX&snapshot
```

An app that keeps its own Ring session can pass `access_token` and `hardware_id` instead of `refresh_token`. go2rtc then uses the token as it is and creates no session of its own. A second session on the same hardware id can replace the app's session and stop its push notifications.

```yaml
streams:
  ring: ring:?camera_id=XXX&device_id=XXX&access_token=XXX&hardware_id=XXX
```
