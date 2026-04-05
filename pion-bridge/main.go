// pion-bridge — WebRTC-to-RTP bridge for Nest cameras via Google Foyer API.
// Connects to one camera, forwards H.264 RTP to a local UDP port for ffmpeg.
//
// Usage: pion-bridge -cookies cookies.json -device DEVICE_XXX -port 15000 [-rtcp-port 15001]
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

func main() {
	cookiesFile := flag.String("cookies", "./data/cookies.json", "Path to cookies.json")
	deviceID := flag.String("device", "", "Foyer device ID (DEVICE_XXX)")
	videoPort := flag.Int("port", 0, "UDP port for video RTP output")
	audioPort := flag.Int("audio-port", 0, "UDP port for audio RTP output (0=no audio)")
	resolution := flag.Int("resolution", 3, "Video resolution (0=low, 1=SD, 2=HD, 3=Full)")
	apiKey := flag.String("api-key", "AIzaSyCMqap8NH88PrhvoBwY1W8ChRUJRjIOJXM", "Google API key")
	flag.Parse()

	if *deviceID == "" || *videoPort == 0 {
		fmt.Fprintf(os.Stderr, "Usage: pion-bridge -device DEVICE_XXX -port 15000 [-cookies cookies.json]\n")
		os.Exit(1)
	}

	// Load cookies
	cookieData, err := os.ReadFile(*cookiesFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: cannot read cookies: %v\n", err)
		os.Exit(1)
	}
	var cookies map[string]string
	json.Unmarshal(cookieData, &cookies)

	if cookies["SAPISID"] == "" {
		fmt.Fprintf(os.Stderr, "ERROR: no SAPISID in cookies\n")
		os.Exit(1)
	}

	// Auth
	origin := "https://home.google.com"
	ts := time.Now().Unix()
	h := sha1.New()
	fmt.Fprintf(h, "%d %s %s", ts, cookies["SAPISID"], origin)
	hash := fmt.Sprintf("%d_%x", ts, h.Sum(nil))
	auth := fmt.Sprintf("SAPISIDHASH %s SAPISID1PHASH %s SAPISID3PHASH %s", hash, hash, hash)
	cookieStr := ""
	for k, v := range cookies {
		if cookieStr != "" {
			cookieStr += "; "
		}
		cookieStr += k + "=" + v
	}

	// Media engine with full Chrome-like codec support
	m := &webrtc.MediaEngine{}
	m.RegisterDefaultCodecs()

	// Register Chrome's header extensions for bandwidth estimation
	videoExts := []string{
		"urn:ietf:params:rtp-hdrext:toffset",
		"http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time",
		"urn:3gpp:video-orientation",
		"http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01",
		"http://www.webrtc.org/experiments/rtp-hdrext/playout-delay",
		"http://www.webrtc.org/experiments/rtp-hdrext/video-content-type",
		"http://www.webrtc.org/experiments/rtp-hdrext/video-timing",
		"http://www.webrtc.org/experiments/rtp-hdrext/color-space",
		"urn:ietf:params:rtp-hdrext:sdes:mid",
		"urn:ietf:params:rtp-hdrext:sdes:rtp-stream-id",
		"urn:ietf:params:rtp-hdrext:sdes:repaired-rtp-stream-id",
	}
	audioExts := []string{
		"urn:ietf:params:rtp-hdrext:ssrc-audio-level",
		"http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time",
		"http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01",
		"urn:ietf:params:rtp-hdrext:sdes:mid",
	}
	for _, ext := range videoExts {
		m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: ext}, webrtc.RTPCodecTypeVideo)
	}
	for _, ext := range audioExts {
		m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: ext}, webrtc.RTPCodecTypeAudio)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: PeerConnection: %v\n", err)
		os.Exit(1)
	}

	pc.CreateDataChannel("dc", &webrtc.DataChannelInit{ID: uint16Ptr(1)})
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})

	// UDP output for video
	videoConn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: *videoPort})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: UDP video: %v\n", err)
		os.Exit(1)
	}

	// Optional UDP output for audio
	var audioConn *net.UDPConn
	if *audioPort > 0 {
		audioConn, _ = net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: *audioPort})
	}

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		fmt.Fprintf(os.Stderr, "STATE %s\n", state)
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			os.Exit(1)
		}
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		codec := track.Codec()
		fmt.Fprintf(os.Stderr, "TRACK %s codec=%s pt=%d\n", track.Kind(), codec.MimeType, codec.PayloadType)

		if track.Kind() == webrtc.RTPCodecTypeVideo {
			// Send PLI for keyframe
			pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{
				MediaSSRC: uint32(track.SSRC()),
			}})

			// Print codec info to stdout for the Node.js parent
			info := map[string]interface{}{
				"type":        "video_info",
				"codec":       codec.MimeType,
				"payloadType": codec.PayloadType,
				"clockRate":   codec.ClockRate,
			}
			infoJSON, _ := json.Marshal(info)
			fmt.Println(string(infoJSON))

			for {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					fmt.Fprintf(os.Stderr, "ERROR: ReadRTP: %v\n", err)
					return
				}
				if len(pkt.Payload) < 2 {
					continue
				}
				buf, _ := pkt.Marshal()
				videoConn.Write(buf)
			}
		}

		if track.Kind() == webrtc.RTPCodecTypeAudio && audioConn != nil {
			fmt.Fprintf(os.Stderr, "TRACK audio forwarding to port %d\n", *audioPort)
			info := map[string]interface{}{
				"type":        "audio_info",
				"codec":       codec.MimeType,
				"payloadType": codec.PayloadType,
			}
			infoJSON, _ := json.Marshal(info)
			fmt.Println(string(infoJSON))

			for {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				buf, _ := pkt.Marshal()
				audioConn.Write(buf)
			}
		}
	})

	// Create offer with ICE
	offer, _ := pc.CreateOffer(nil)
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	pc.SetLocalDescription(offer)
	<-gatherComplete

	sdp := pc.LocalDescription().SDP

	// Call Foyer API
	reqBody, _ := json.Marshal(map[string]interface{}{
		"action":                   "offer",
		"deviceId":                 *deviceID,
		"sdp":                      sdp,
		"requestedVideoResolution": *resolution,
	})

	req, _ := http.NewRequest("POST", "https://googlehomefoyer-pa.clients6.google.com/v1/join_stream", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", auth)
	req.Header.Set("Cookie", cookieStr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", *apiKey)
	req.Header.Set("x-goog-authuser", "0")
	req.Header.Set("x-foyer-client-environment", "CAc=")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Foyer API: %v\n", err)
		os.Exit(1)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "ERROR: Foyer %d: %s\n", resp.StatusCode, string(body[:min(len(body), 200)]))
		os.Exit(1)
	}

	var answer struct{ SDP string `json:"sdp"` }
	json.Unmarshal(body, &answer)

	// Check what Foyer selected
	for _, line := range strings.Split(answer.SDP, "\n") {
		if strings.Contains(line, "rtpmap") && !strings.Contains(line, "opus") && !strings.Contains(line, "rtx") &&
			!strings.Contains(line, "red") && !strings.Contains(line, "ulpfec") {
			fmt.Fprintf(os.Stderr, "ANSWER %s\n", strings.TrimSpace(line))
		}
	}

	err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: SetRemoteDescription: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "CONNECTED\n")

	// Block forever (parent process manages lifecycle)
	select {}
}

func uint16Ptr(v uint16) *uint16 { return &v }
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
