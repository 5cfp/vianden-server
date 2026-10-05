package voice

import (
	"context"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// How the voice network works (WebRTC basics, short):
//
//   - Each client opens ONE WebRTC connection to the server (an SFU: "selective forwarding
//     unit"). It sends its own microphone audio once; the server forwards it to everyone
//     else in the channel without decoding or mixing it.
//   - Before audio flows, both sides exchange "ICE candidates": addresses where they can
//     be reached. The server's are its public IP (or LAN IP) + our one UDP port, and the
//     main TCP port as a fallback for networks that block UDP.
//   - Audio is always encrypted (DTLS-SRTP). The keys are agreed through the signaling,
//     which itself runs over the (TLS) WebSocket. Unencrypted voice is impossible.
//   - One UDP port for everyone ("UDP mux"): the server tells connections apart by the
//     ICE user name inside each packet, so hosts only open one port in the router.

// addressRefresh: how often host names in VIANDEN_VOICE_PUBLIC_ADDRESS are looked up
// again. A home internet connection can get a new IP at any time (dynamic DNS).
const addressRefresh = 5 * time.Minute

// newAPI builds the Pion API: which codecs we speak (only Opus, the standard voice codec)
// and how we reach the network (our sockets, which addresses to tell clients).
func (s *Service) newAPI(publicIPs []string) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	// Opus at 48 kHz: what every WebRTC client uses for voice. "useinbandfec" lets the
	// receiver repair small packet losses.
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			// maxaveragebitrate: what each app may send. WebRTC's own default for voice
			// is 32 kbit/s; 64 kbit/s is near-transparent speech (still tiny: 25 people
			// in a channel stay far below 2 Mbit/s per listener).
			SDPFmtpLine: "minptime=10;useinbandfec=1;maxaveragebitrate=64000",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	// Default helpers: RTCP reports and packet-loss feedback.
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}

	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(s.udpMux)
	networks := []webrtc.NetworkType{webrtc.NetworkTypeUDP4}
	if s.tcpMux != nil {
		se.SetICETCPMux(s.tcpMux)
		networks = append(networks, webrtc.NetworkTypeTCP4)
	}
	se.SetNetworkTypes(networks)
	se.SetIncludeLoopbackCandidate(s.cfg.Loopback)
	if len(publicIPs) > 0 {
		// Tell clients these addresses instead of the machine's own ones (which may be a
		// private address behind a router, or a Docker-internal one). With both the public
		// and the LAN IP listed, people at home and outside can all connect.
		if err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External:        publicIPs,
			AsCandidateType: webrtc.ICECandidateTypeHost,
			Mode:            webrtc.ICEAddressRewriteReplace,
		}); err != nil {
			return nil, err
		}
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se)), nil
}

// resolve turns the configured addresses into IPv4 addresses (host names are looked up).
func resolve(ctx context.Context, addrs []string) ([]string, error) {
	var ips []string
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil {
			if ip.To4() == nil {
				return nil, fmt.Errorf("voice address %s: only IPv4 is supported for now", a)
			}
			ips = append(ips, ip.String())
			continue
		}
		lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		found, err := net.DefaultResolver.LookupIP(lookupCtx, "ip4", a)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("looking up voice address %s: %w", a, err)
		}
		for _, ip := range found {
			ips = append(ips, ip.String())
		}
	}
	slices.Sort(ips)
	return slices.Compact(ips), nil
}
