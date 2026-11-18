package sfu

import (
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/webrtc/v4"
)

var videoFeedback = []webrtc.RTCPFeedback{
	{Type: webrtc.TypeRTCPFBNACK}, {Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"},
	{Type: webrtc.TypeRTCPFBCCM, Parameter: "fir"}, {Type: webrtc.TypeRTCPFBTransportCC},
}

func video(mime, fmtp string, pt webrtc.PayloadType) webrtc.RTPCodecParameters {
	return webrtc.RTPCodecParameters{PayloadType: pt, RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: mime, ClockRate: 90000, SDPFmtpLine: fmtp, RTCPFeedback: videoFeedback}}
}

// codecsByName are the codecs the SFU can receive from a streamer. It never transcodes, so players
// are offered exactly the codec the streamer chose.
var codecsByName = map[string][]webrtc.RTPCodecParameters{
	"opus": {{PayloadType: 111, RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1",
		RTCPFeedback: []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBTransportCC}}}}},
	"vp8": {video(webrtc.MimeTypeVP8, "", 96)},
	"vp9": {video(webrtc.MimeTypeVP9, "profile-id=0", 98), video(webrtc.MimeTypeVP9, "profile-id=2", 100)},
	"h264": {
		video(webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f", 102),
		video(webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", 104),
	},
	"av1": {video(webrtc.MimeTypeAV1, "", 45)},
}

func kindOf(mime string) webrtc.RTPCodecType {
	if strings.HasPrefix(strings.ToLower(mime), "audio/") {
		return webrtc.RTPCodecTypeAudio
	}
	return webrtc.RTPCodecTypeVideo
}

// settingEngine builds the ICE settings shared by every peer connection.
func settingEngine(c ICEConfig) (webrtc.SettingEngine, error) {
	var s webrtc.SettingEngine
	// No mDNS at all: no multicast listener, no queries on the LAN. Browsers hide their host
	// addresses behind .local names the SFU then cannot resolve, but that costs nothing: the
	// browser's checks reach the SFU's candidates and the SFU learns its address from them
	// (verified with Chrome and Firefox).
	s.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	networks := []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6}
	if c.TCP {
		networks = append(networks, webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6)
	}
	s.SetNetworkTypes(networks)
	s.SetIncludeLoopbackCandidate(c.IncludeLoopback)
	var ifaceFilter func(string) bool
	if len(c.Interfaces) > 0 {
		ifaceFilter = func(name string) bool { return slices.Contains(c.Interfaces, name) }
		s.SetInterfaceFilter(ifaceFilter)
	}
	if len(c.PublicIPs) > 0 {
		for _, ip := range c.PublicIPs {
			if net.ParseIP(ip) == nil {
				return s, fmt.Errorf("sfu: public ip %q is not an IP address", ip)
			}
		}
		if err := s.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External: c.PublicIPs, AsCandidateType: webrtc.ICECandidateTypeHost,
		}); err != nil {
			return s, err
		}
	}
	switch {
	case c.UDPPort > 0:
		opts := []ice.UDPMuxFromPortOption{ice.UDPMuxFromPortWithNetworks(ice.NetworkTypeUDP4, ice.NetworkTypeUDP6)}
		if ifaceFilter != nil {
			opts = append(opts, ice.UDPMuxFromPortWithInterfaceFilter(ifaceFilter))
		}
		if c.IncludeLoopback {
			opts = append(opts, ice.UDPMuxFromPortWithLoopback())
		}
		mux, err := ice.NewMultiUDPMuxFromPort(c.UDPPort, opts...)
		if err != nil {
			return s, fmt.Errorf("sfu: udp port %d: %w", c.UDPPort, err)
		}
		s.SetICEUDPMux(mux)
	case c.PortMin > 0 && c.PortMax >= c.PortMin:
		if err := s.SetEphemeralUDPPortRange(c.PortMin, c.PortMax); err != nil {
			return s, err
		}
	}
	return s, nil
}

// streamerAPI builds the API for the connection to the streamer: every configured codec, simulcast
// header extensions, NACK, RTCP reports, and transport-cc feedback so the streamer's own bandwidth
// estimation works.
func streamerAPI(se webrtc.SettingEngine, codecs []string) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	for _, name := range codecs {
		for _, c := range codecsByName[strings.ToLower(name)] {
			if err := m.RegisterCodec(c, kindOf(c.MimeType)); err != nil {
				return nil, err
			}
		}
	}
	if err := webrtc.ConfigureSimulcastExtensionHeaders(m); err != nil {
		return nil, err
	}
	reg := &interceptor.Registry{}
	if err := webrtc.ConfigureNack(m, reg); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureRTCPReports(reg); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureTWCCSender(m, reg); err != nil {
		return nil, err
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se)), nil
}

// newPlayerPC builds a peer connection for one player, offering only the codecs the streamer is
// sending. Each player gets its own API so its bandwidth estimator is unambiguously its own. The
// estimator is nil when BWE is disabled.
func newPlayerPC(se webrtc.SettingEngine, cfg webrtc.Configuration, codecs []webrtc.RTPCodecParameters, bwe BWEConfig) (*webrtc.PeerConnection, cc.BandwidthEstimator, error) {
	m := &webrtc.MediaEngine{}
	for _, c := range codecs {
		if err := m.RegisterCodec(c, kindOf(c.MimeType)); err != nil {
			return nil, nil, err
		}
	}
	reg := &interceptor.Registry{}
	if err := webrtc.ConfigureNack(m, reg); err != nil {
		return nil, nil, err
	}
	if err := webrtc.ConfigureRTCPReports(reg); err != nil {
		return nil, nil, err
	}
	got := make(chan cc.BandwidthEstimator, 1)
	if !bwe.Disabled {
		ccf, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
			// No pacer: packets are forwarded as they arrive; the estimate only drives layer choice.
			return gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(bwe.InitialBitrate),
				gcc.SendSideBWEMinBitrate(bwe.MinBitrate), gcc.SendSideBWEMaxBitrate(bwe.MaxBitrate),
				gcc.SendSideBWEPacer(gcc.NewNoOpPacer()))
		})
		if err != nil {
			return nil, nil, err
		}
		ccf.OnNewPeerConnection(func(_ string, e cc.BandwidthEstimator) {
			select {
			case got <- e:
			default:
			}
		})
		reg.Add(ccf)
		if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, reg); err != nil {
			return nil, nil, err
		}
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, nil, err
	}
	var est cc.BandwidthEstimator
	select { // the interceptor is built inside NewPeerConnection, so this is already filled
	case est = <-got:
	default:
	}
	return pc, est, nil
}
