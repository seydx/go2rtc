package onvif

import (
	"bytes"
	"fmt"
	"html"
	"regexp"
	"time"
)

const ServiceGetServiceCapabilities = "GetServiceCapabilities"

const (
	DeviceGetCapabilities          = "GetCapabilities"
	DeviceGetDeviceInformation     = "GetDeviceInformation"
	DeviceGetDiscoveryMode         = "GetDiscoveryMode"
	DeviceGetDNS                   = "GetDNS"
	DeviceGetHostname              = "GetHostname"
	DeviceGetNetworkDefaultGateway = "GetNetworkDefaultGateway"
	DeviceGetNetworkInterfaces     = "GetNetworkInterfaces"
	DeviceGetNetworkProtocols      = "GetNetworkProtocols"
	DeviceGetNTP                   = "GetNTP"
	DeviceGetScopes                = "GetScopes"
	DeviceGetServices              = "GetServices"
	DeviceGetSystemDateAndTime     = "GetSystemDateAndTime"
	DeviceSetSystemDateAndTime     = "SetSystemDateAndTime"
	DeviceSystemReboot             = "SystemReboot"
)

const (
	MediaGetAudioEncoderConfigurations       = "GetAudioEncoderConfigurations"
	MediaGetAudioSources                     = "GetAudioSources"
	MediaGetAudioSourceConfigurations        = "GetAudioSourceConfigurations"
	MediaGetProfile                          = "GetProfile"
	MediaGetProfiles                         = "GetProfiles"
	MediaGetSnapshotUri                      = "GetSnapshotUri"
	MediaGetStreamUri                        = "GetStreamUri"
	MediaGetVideoEncoderConfiguration        = "GetVideoEncoderConfiguration"
	MediaGetVideoEncoderConfigurations       = "GetVideoEncoderConfigurations"
	MediaGetVideoEncoderConfigurationOptions = "GetVideoEncoderConfigurationOptions"
	MediaGetVideoSources                     = "GetVideoSources"
	MediaGetVideoSourceConfiguration         = "GetVideoSourceConfiguration"
	MediaGetVideoSourceConfigurations        = "GetVideoSourceConfigurations"
)

func GetRequestAction(b []byte) string {
	// <soap-env:Body><ns0:GetCapabilities xmlns:ns0="http://www.onvif.org/ver10/device/wsdl">
	// <v:Body><GetSystemDateAndTime xmlns="http://www.onvif.org/ver10/device/wsdl" /></v:Body>
	re := regexp.MustCompile(`Body[^<]+<([^ />]+)`)
	m := re.FindSubmatch(b)
	if len(m) != 2 {
		return ""
	}
	if i := bytes.IndexByte(m[1], ':'); i > 0 {
		return string(m[1][i+1:])
	}
	return string(m[1])
}

func GetCapabilitiesResponse(protocol string, host string) []byte {
	e := NewEnvelope()
	e.Appendf(`<tds:GetCapabilitiesResponse>
	<tds:Capabilities>
		<tt:Device>
			<tt:XAddr>%s://%s/onvif/device_service</tt:XAddr>
		</tt:Device>
		<tt:Media>
			<tt:XAddr>%s://%s/onvif/media_service</tt:XAddr>
			<tt:StreamingCapabilities>
				<tt:RTPMulticast>false</tt:RTPMulticast>
				<tt:RTP_TCP>true</tt:RTP_TCP>
				<tt:RTP_RTSP_TCP>true</tt:RTP_RTSP_TCP>
			</tt:StreamingCapabilities>
		</tt:Media>
	</tds:Capabilities>
</tds:GetCapabilitiesResponse>`, protocol, host, protocol, host)
	return e.Bytes()
}

func GetServicesResponse(protocol string, host string) []byte {
	e := NewEnvelope()
	e.Appendf(`<tds:GetServicesResponse>
	<tds:Service>
		<tds:Namespace>http://www.onvif.org/ver10/device/wsdl</tds:Namespace>
		<tds:XAddr>%s://%s/onvif/device_service</tds:XAddr>
		<tds:Version><tt:Major>2</tt:Major><tt:Minor>5</tt:Minor></tds:Version>
	</tds:Service>
	<tds:Service>
		<tds:Namespace>http://www.onvif.org/ver10/media/wsdl</tds:Namespace>
		<tds:XAddr>%s://%s/onvif/media_service</tds:XAddr>
		<tds:Version><tt:Major>2</tt:Major><tt:Minor>5</tt:Minor></tds:Version>
	</tds:Service>
</tds:GetServicesResponse>`, protocol, host, protocol, host)
	return e.Bytes()
}

func GetSystemDateAndTimeResponse() []byte {
	loc := time.Now()
	utc := loc.UTC()

	e := NewEnvelope()
	e.Appendf(`<tds:GetSystemDateAndTimeResponse>
	<tds:SystemDateAndTime>
		<tt:DateTimeType>NTP</tt:DateTimeType>
		<tt:DaylightSavings>true</tt:DaylightSavings>
		<tt:TimeZone>
			<tt:TZ>%s</tt:TZ>
		</tt:TimeZone>
		<tt:UTCDateTime>
			<tt:Time><tt:Hour>%d</tt:Hour><tt:Minute>%d</tt:Minute><tt:Second>%d</tt:Second></tt:Time>
			<tt:Date><tt:Year>%d</tt:Year><tt:Month>%d</tt:Month><tt:Day>%d</tt:Day></tt:Date>
		</tt:UTCDateTime>
		<tt:LocalDateTime>
			<tt:Time><tt:Hour>%d</tt:Hour><tt:Minute>%d</tt:Minute><tt:Second>%d</tt:Second></tt:Time>
			<tt:Date><tt:Year>%d</tt:Year><tt:Month>%d</tt:Month><tt:Day>%d</tt:Day></tt:Date>
		</tt:LocalDateTime>
	</tds:SystemDateAndTime>
</tds:GetSystemDateAndTimeResponse>`,
		GetPosixTZ(loc),
		utc.Hour(), utc.Minute(), utc.Second(), utc.Year(), utc.Month(), utc.Day(),
		loc.Hour(), loc.Minute(), loc.Second(), loc.Year(), loc.Month(), loc.Day(),
	)
	return e.Bytes()
}

func GetDeviceInformationResponse(manuf, model, firmware, serial string) []byte {
	e := NewEnvelope()
	e.Appendf(`<tds:GetDeviceInformationResponse>
	<tds:Manufacturer>%s</tds:Manufacturer>
	<tds:Model>%s</tds:Model>
	<tds:FirmwareVersion>%s</tds:FirmwareVersion>
	<tds:SerialNumber>%s</tds:SerialNumber>
	<tds:HardwareId>1.00</tds:HardwareId>
</tds:GetDeviceInformationResponse>`, manuf, model, firmware, serial)
	return e.Bytes()
}

func GetProfilesResponse(profiles []*MediaProfile) []byte {
	e := NewEnvelope()
	e.Append(`<trt:GetProfilesResponse>`)
	for _, p := range profiles {
		appendProfile(e, "Profiles", p)
	}
	e.Append(`</trt:GetProfilesResponse>`)
	return e.Bytes()
}

func GetProfileResponse(p *MediaProfile) []byte {
	e := NewEnvelope()
	e.Append(`<trt:GetProfileResponse>`)
	appendProfile(e, "Profile", p)
	e.Append(`</trt:GetProfileResponse>`)
	return e.Bytes()
}

func appendProfile(e *Envelope, tag string, p *MediaProfile) {
	// go2rtc name = ONVIF Profile token
	// a name is any text: one with & or < made the whole response invalid XML
	e.Appendf(`<trt:%s token="%s" fixed="true">`, tag, html.EscapeString(p.Token))
	e.Appendf(`<tt:Name>%s</tt:Name>`, html.EscapeString(p.Name))
	appendVideoSourceConfiguration(e, "tt:VideoSourceConfiguration", p.Source)
	appendVideoEncoderConfiguration(e, "tt:VideoEncoderConfiguration", p)
	e.Appendf(`</trt:%s>`, tag)
}

func GetVideoSourcesResponse(sources []*VideoSource) []byte {
	e := NewEnvelope()
	e.Append(`<trt:GetVideoSourcesResponse>`)
	for _, s := range sources {
		width, height := s.size()
		e.Appendf(`<trt:VideoSources token="%s">
	<tt:Framerate>%d</tt:Framerate>
	<tt:Resolution><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:Resolution>
</trt:VideoSources>`, html.EscapeString(s.Token), defaultFrameRate, width, height)
	}
	e.Append(`</trt:GetVideoSourcesResponse>`)
	return e.Bytes()
}

func GetVideoSourceConfigurationsResponse(sources []*VideoSource) []byte {
	e := NewEnvelope()
	e.Append(`<trt:GetVideoSourceConfigurationsResponse>`)
	for _, s := range sources {
		appendVideoSourceConfiguration(e, "trt:Configurations", s)
	}
	e.Append(`</trt:GetVideoSourceConfigurationsResponse>`)
	return e.Bytes()
}

func GetVideoSourceConfigurationResponse(s *VideoSource) []byte {
	e := NewEnvelope()
	e.Append(`<trt:GetVideoSourceConfigurationResponse>`)
	appendVideoSourceConfiguration(e, "trt:Configuration", s)
	e.Append(`</trt:GetVideoSourceConfigurationResponse>`)
	return e.Bytes()
}

// appendVideoSourceConfiguration: the token of the configuration of a video
// source is the token of the source, all of its profiles use it
func appendVideoSourceConfiguration(e *Envelope, tag string, s *VideoSource) {
	token := html.EscapeString(s.Token)
	width, height := s.size()
	e.Appendf(`<%s token="%s" fixed="true">
	<tt:Name>VSC</tt:Name>
	<tt:UseCount>%d</tt:UseCount>
	<tt:SourceToken>%s</tt:SourceToken>
	<tt:Bounds x="0" y="0" width="%d" height="%d"></tt:Bounds>
</%s>`, tag, token, len(s.Profiles), token, width, height, tag)
}

func GetVideoEncoderConfigurationsResponse(profiles []*MediaProfile) []byte {
	e := NewEnvelope()
	e.Append(`<trt:GetVideoEncoderConfigurationsResponse>`)
	for _, p := range profiles {
		appendVideoEncoderConfiguration(e, "trt:Configurations", p)
	}
	e.Append(`</trt:GetVideoEncoderConfigurationsResponse>`)
	return e.Bytes()
}

func GetVideoEncoderConfigurationResponse(p *MediaProfile) []byte {
	e := NewEnvelope()
	e.Append(`<trt:GetVideoEncoderConfigurationResponse>`)
	appendVideoEncoderConfiguration(e, "trt:Configuration", p)
	e.Append(`</trt:GetVideoEncoderConfigurationResponse>`)
	return e.Bytes()
}

// appendVideoEncoderConfiguration: the token of the encoder configuration of a
// profile is the token of the profile. Important for UniFi Protect: RateControl,
// and H265 as the encoding of an H.265 stream (else its live view stays black).
func appendVideoEncoderConfiguration(e *Envelope, tag string, p *MediaProfile) {
	v := p.Video
	e.Appendf(`<%s token="%s">
	<tt:Name>VEC</tt:Name>
	<tt:UseCount>1</tt:UseCount>
	<tt:Encoding>%s</tt:Encoding>
	<tt:Resolution><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:Resolution>
	<tt:Quality>0</tt:Quality>
	<tt:RateControl><tt:FrameRateLimit>%d</tt:FrameRateLimit><tt:EncodingInterval>1</tt:EncodingInterval><tt:BitrateLimit>%d</tt:BitrateLimit></tt:RateControl>`,
		tag, html.EscapeString(p.Token), v.Encoding, v.Width, v.Height, defaultFrameRate, defaultBitrate)
	if v.Encoding == "H264" {
		e.Appendf(`<tt:H264><tt:GovLength>10</tt:GovLength><tt:H264Profile>%s</tt:H264Profile></tt:H264>`, v.Profile)
	}
	e.Appendf(`<tt:SessionTimeout>PT10S</tt:SessionTimeout>
</%s>`, tag)
}

// GetVideoEncoderConfigurationOptionsResponse: the only option of a profile is
// its configuration, go2rtc doesn't change how a stream is encoded
func GetVideoEncoderConfigurationOptionsResponse(p *MediaProfile) []byte {
	v := p.Video
	options := fmt.Sprintf(`<tt:ResolutionsAvailable><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:ResolutionsAvailable>
	<tt:GovLengthRange><tt:Min>10</tt:Min><tt:Max>10</tt:Max></tt:GovLengthRange>
	<tt:FrameRateRange><tt:Min>%d</tt:Min><tt:Max>%d</tt:Max></tt:FrameRateRange>
	<tt:EncodingIntervalRange><tt:Min>1</tt:Min><tt:Max>1</tt:Max></tt:EncodingIntervalRange>`,
		v.Width, v.Height, defaultFrameRate, defaultFrameRate)

	e := NewEnvelope()
	e.Append(`<trt:GetVideoEncoderConfigurationOptionsResponse><trt:Options>
	<tt:QualityRange><tt:Min>0</tt:Min><tt:Max>0</tt:Max></tt:QualityRange>`)
	if v.Encoding == "H264" {
		e.Append(`<tt:H264>`, options, `<tt:H264ProfilesSupported>`, v.Profile, `</tt:H264ProfilesSupported></tt:H264>`)
	} else {
		// Media1 has no H265 options, UniFi Protect reads them from Extension
		e.Append(`<tt:Extension><tt:H265>`, options, `<tt:H265ProfilesSupported>Main</tt:H265ProfilesSupported></tt:H265></tt:Extension>`)
	}
	e.Append(`</trt:Options></trt:GetVideoEncoderConfigurationOptionsResponse>`)
	return e.Bytes()
}

// FaultResponse answers a request for something the server doesn't have, ex.
// the subcode NoProfile for an unknown profile token. SOAP 1.2 sends a fault
// of the sender with HTTP 400.
func FaultResponse(subcode, reason string) []byte {
	e := NewEnvelope()
	e.Appendf(`<s:Fault xmlns:ter="http://www.onvif.org/ver10/error">
	<s:Code><s:Value>s:Sender</s:Value><s:Subcode><s:Value>ter:InvalidArgVal</s:Value><s:Subcode><s:Value>ter:%s</s:Value></s:Subcode></s:Subcode></s:Code>
	<s:Reason><s:Text xml:lang="en">%s</s:Text></s:Reason>
</s:Fault>`, subcode, html.EscapeString(reason))
	return e.Bytes()
}

func GetStreamUriResponse(uri string) []byte {
	e := NewEnvelope()
	e.Appendf(`<trt:GetStreamUriResponse><trt:MediaUri><tt:Uri>%s</tt:Uri></trt:MediaUri></trt:GetStreamUriResponse>`, html.EscapeString(uri))
	return e.Bytes()
}

func GetSnapshotUriResponse(uri string) []byte {
	e := NewEnvelope()
	e.Appendf(`<trt:GetSnapshotUriResponse><trt:MediaUri><tt:Uri>%s</tt:Uri></trt:MediaUri></trt:GetSnapshotUriResponse>`, html.EscapeString(uri))
	return e.Bytes()
}

func StaticResponse(operation string) []byte {
	switch operation {
	case DeviceGetSystemDateAndTime:
		return GetSystemDateAndTimeResponse()
	}

	e := NewEnvelope()
	e.Append(responses[operation])
	return e.Bytes()
}

var responses = map[string]string{
	ServiceGetServiceCapabilities: `<trt:GetServiceCapabilitiesResponse>
	<trt:Capabilities SnapshotUri="true" Rotation="false" VideoSourceMode="false" OSD="false" TemporaryOSDText="false" EXICompression="false">
		<trt:StreamingCapabilities RTPMulticast="false" RTP_TCP="true" RTP_RTSP_TCP="true" NonAggregateControl="false" NoRTSPStreaming="false" />
	</trt:Capabilities>
</trt:GetServiceCapabilitiesResponse>`,

	DeviceGetDiscoveryMode:         `<tds:GetDiscoveryModeResponse><tds:DiscoveryMode>Discoverable</tds:DiscoveryMode></tds:GetDiscoveryModeResponse>`,
	DeviceGetDNS:                   `<tds:GetDNSResponse><tds:DNSInformation /></tds:GetDNSResponse>`,
	DeviceGetHostname:              `<tds:GetHostnameResponse><tds:HostnameInformation /></tds:GetHostnameResponse>`,
	DeviceGetNetworkDefaultGateway: `<tds:GetNetworkDefaultGatewayResponse><tds:NetworkGateway /></tds:GetNetworkDefaultGatewayResponse>`,
	DeviceGetNTP:                   `<tds:GetNTPResponse><tds:NTPInformation /></tds:GetNTPResponse>`,
	DeviceSetSystemDateAndTime:     `<tds:SetSystemDateAndTimeResponse />`,
	DeviceSystemReboot:             `<tds:SystemRebootResponse><tds:Message>OK</tds:Message></tds:SystemRebootResponse>`,

	DeviceGetNetworkInterfaces: `<tds:GetNetworkInterfacesResponse />`,
	DeviceGetNetworkProtocols:  `<tds:GetNetworkProtocolsResponse />`,
	DeviceGetScopes: `<tds:GetScopesResponse>
	<tds:Scopes><tt:ScopeDef>Fixed</tt:ScopeDef><tt:ScopeItem>onvif://www.onvif.org/name/go2rtc</tt:ScopeItem></tds:Scopes>
	<tds:Scopes><tt:ScopeDef>Fixed</tt:ScopeDef><tt:ScopeItem>onvif://www.onvif.org/location/github</tt:ScopeItem></tds:Scopes>
	<tds:Scopes><tt:ScopeDef>Fixed</tt:ScopeDef><tt:ScopeItem>onvif://www.onvif.org/Profile/Streaming</tt:ScopeItem></tds:Scopes>
	<tds:Scopes><tt:ScopeDef>Fixed</tt:ScopeDef><tt:ScopeItem>onvif://www.onvif.org/type/Network_Video_Transmitter</tt:ScopeItem></tds:Scopes>
</tds:GetScopesResponse>`,

	MediaGetAudioEncoderConfigurations: `<trt:GetAudioEncoderConfigurationsResponse />`,
	MediaGetAudioSources:               `<trt:GetAudioSourcesResponse />`,
	MediaGetAudioSourceConfigurations:  `<trt:GetAudioSourceConfigurationsResponse />`,
}
