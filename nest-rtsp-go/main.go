// nest-rtsp — Single binary: WebRTC from Google Foyer → RTSP server.
// No ffmpeg, no MediaMTX, no Node.js. One process for all cameras.
//
// Usage: nest-rtsp -config config.yaml
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"gopkg.in/yaml.v3"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
)

type CameraConfig struct {
	DeviceID   string `yaml:"device_id"`
	Resolution int    `yaml:"resolution"`
}

type Config struct {
	CookiesFile string                  `yaml:"cookies_file"`
	RTSPPort    int                     `yaml:"rtsp_port"`
	APIKey      string                  `yaml:"api_key"`
	Cameras     map[string]CameraConfig `yaml:"cameras"`
}

// activeWriter is atomically swapped to switch which WebRTC connection
// feeds the RTSP stream. Only the holder of the current generation writes.
type cameraStream struct {
	name   string
	config CameraConfig
	media  *description.Media
	stream *gortsplib.ServerStream
	gen    atomic.Int64 // current writer generation
	mu     sync.RWMutex
}

type rtspHandler struct {
	cameras map[string]*cameraStream
	server  *gortsplib.Server
	mu      sync.RWMutex
}

func (h *rtspHandler) OnConnOpen(_ *gortsplib.ServerHandlerOnConnOpenCtx)   {}
func (h *rtspHandler) OnConnClose(_ *gortsplib.ServerHandlerOnConnCloseCtx) {}
func (h *rtspHandler) OnSessionOpen(_ *gortsplib.ServerHandlerOnSessionOpenCtx) {
}
func (h *rtspHandler) OnSessionClose(_ *gortsplib.ServerHandlerOnSessionCloseCtx) {}

func (h *rtspHandler) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	name := strings.TrimPrefix(ctx.Path, "/")
	h.mu.RLock()
	cam, ok := h.cameras[name]
	h.mu.RUnlock()
	if !ok || cam.stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, cam.stream, nil
}

func (h *rtspHandler) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	name := strings.TrimPrefix(ctx.Path, "/")
	h.mu.RLock()
	cam, ok := h.cameras[name]
	h.mu.RUnlock()
	if !ok || cam.stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, cam.stream, nil
}

func (h *rtspHandler) OnPlay(_ *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// getNALType extracts the NAL unit type from an RTP H.264 payload
func getNALType(payload []byte) byte {
	if len(payload) < 2 {
		return 0
	}
	typ := payload[0] & 0x1F
	if typ == 28 { // FU-A: real type in second byte
		return payload[1] & 0x1F
	}
	return typ
}

// extractSPSPPS pulls SPS and PPS NAL units from RTP packets.
// Handles single NAL (type 7/8), STAP-A (type 24 containing multiple NALs),
// and FU-A (type 28 fragmented NALs).
func extractSPSPPS(packets []*pionrtp.Packet) (sps, pps []byte) {
	for _, pkt := range packets {
		if len(pkt.Payload) < 2 {
			continue
		}
		typ := pkt.Payload[0] & 0x1F

		switch {
		case typ == 7: // Single SPS NAL
			sps = make([]byte, len(pkt.Payload))
			copy(sps, pkt.Payload)
		case typ == 8: // Single PPS NAL
			pps = make([]byte, len(pkt.Payload))
			copy(pps, pkt.Payload)
		case typ == 24: // STAP-A — aggregation of multiple NAL units
			// Format: [STAP-A header(1)] [size(2) NAL(N)] [size(2) NAL(N)] ...
			offset := 1
			for offset+2 < len(pkt.Payload) {
				nalSize := int(pkt.Payload[offset])<<8 | int(pkt.Payload[offset+1])
				offset += 2
				if offset+nalSize > len(pkt.Payload) {
					break
				}
				nalData := pkt.Payload[offset : offset+nalSize]
				if len(nalData) > 0 {
					nalType := nalData[0] & 0x1F
					if nalType == 7 && sps == nil {
						sps = make([]byte, nalSize)
						copy(sps, nalData)
					} else if nalType == 8 && pps == nil {
						pps = make([]byte, nalSize)
						copy(pps, nalData)
					}
				}
				offset += nalSize
			}
		}
	}
	return
}

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config file")
	flag.Parse()

	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("Cannot read config: %v", err)
	}
	var config Config
	yaml.Unmarshal(data, &config)

	if config.RTSPPort == 0 {
		config.RTSPPort = 8554
	}
	if config.APIKey == "" {
		config.APIKey = "AIzaSyCMqap8NH88PrhvoBwY1W8ChRUJRjIOJXM"
	}
	if config.CookiesFile == "" {
		config.CookiesFile = "data/cookies.json"
	}

	cookieData, err := os.ReadFile(config.CookiesFile)
	if err != nil {
		log.Fatalf("Cannot read cookies: %v", err)
	}
	var cookies map[string]string
	json.Unmarshal(cookieData, &cookies)
	if cookies["SAPISID"] == "" {
		log.Fatal("No SAPISID in cookies")
	}

	handler := &rtspHandler{cameras: make(map[string]*cameraStream)}
	handler.server = &gortsplib.Server{
		Handler:        handler,
		RTSPAddress:    fmt.Sprintf(":%d", config.RTSPPort),
		UDPRTPAddress:  ":28000",
		UDPRTCPAddress: ":28001",
	}
	err = handler.server.Start()
	if err != nil {
		log.Fatalf("RTSP server: %v", err)
	}
	log.Printf("nest-rtsp started — %d cameras, RTSP on :%d", len(config.Cameras), config.RTSPPort)

	cameraIdx := 0
	totalCams := len(config.Cameras)
	for name, cam := range config.Cameras {
		if cam.DeviceID == "" {
			continue
		}
		cs := &cameraStream{name: name, config: cam}
		handler.mu.Lock()
		handler.cameras[name] = cs
		handler.mu.Unlock()

		stagger := time.Duration(cameraIdx) * 5 * time.Minute / time.Duration(totalCams)
		go startCamera(cs, handler.server, cookies, config.APIKey, stagger)
		cameraIdx++
		time.Sleep(500 * time.Millisecond)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("Shutting down...")
	handler.server.Close()
}

// webrtcConn holds a live WebRTC connection and its packet channel
type webrtcConn struct {
	pc   *webrtc.PeerConnection
	pkts chan *pionrtp.Packet
	done chan error
}

func startCamera(cs *cameraStream, server *gortsplib.Server, cookies map[string]string, apiKey string, initialStagger time.Duration) {
	reconnectInterval := 5 * time.Minute
	firstRun := true
	failures := 0

	for {
		// Establish first/new WebRTC connection
		conn, err := dialWebRTC(cs, cookies, apiKey)
		if err != nil {
			failures++
			delay := min(2*time.Second*time.Duration(1<<min(failures-1, 7)), 5*time.Minute)
			log.Printf("[%s] error: %v — retry in %v", cs.name, err, delay)
			time.Sleep(delay)
			continue
		}
		failures = 0

		// Wait for first IDR with SPS/PPS before creating/starting the stream
		idrPkts := waitForIDR(conn.pkts, 15*time.Second)
		if idrPkts == nil {
			log.Printf("[%s] no IDR received — retrying", cs.name)
			conn.pc.Close()
			continue
		}

		// Create RTSP stream once — with SPS/PPS for proper DESCRIBE
		cs.mu.Lock()
		if cs.stream == nil {
			sps, pps := extractSPSPPS(idrPkts)
			log.Printf("[%s] extracted SPS=%d PPS=%d from %d IDR packets", cs.name, len(sps), len(pps), len(idrPkts))
			forma := &format.H264{
				PayloadTyp:        96,
				PacketizationMode: 1,
				SPS:               sps,
				PPS:               pps,
			}
			media := &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{forma}}
			cs.media = media
			stream := &gortsplib.ServerStream{Server: server, Desc: &description.Session{Medias: []*description.Media{media}}}
			if err := stream.Initialize(); err != nil {
				cs.mu.Unlock()
				log.Printf("[%s] stream init error: %v", cs.name, err)
				conn.pc.Close()
				continue
			}
			cs.stream = stream
			log.Printf("[%s] RTSP stream created", cs.name)
		}
		cs.mu.Unlock()

		// Become the active writer
		myGen := cs.gen.Add(1)
		log.Printf("[%s] streaming (gen %d)", cs.name, myGen)

		// Write the buffered IDR packets first
		cs.mu.RLock()
		s := cs.stream
		m := cs.media
		cs.mu.RUnlock()
		for _, pkt := range idrPkts {
			func() {
				defer func() { recover() }()
				s.WritePacketRTP(m, pkt)
			}()
		}

		// Forward packets from this connection to RTSP stream
		// Stop if: connection dies OR we're no longer the active writer
		go forwardPackets(cs, conn, myGen)

		// Decide when to reconnect
		waitTime := reconnectInterval
		if firstRun && initialStagger > 0 && initialStagger < reconnectInterval {
			waitTime = initialStagger
			firstRun = false
		}

		select {
		case err := <-conn.done:
			// Connection died — loop will reconnect
			log.Printf("[%s] connection dropped: %v", cs.name, err)
			conn.pc.Close()

		case <-time.After(waitTime):
			// Seamless handoff: start new connection while old is still streaming
			log.Printf("[%s] seamless handoff starting", cs.name)

			newConn, err := dialWebRTC(cs, cookies, apiKey)
			if err != nil {
				log.Printf("[%s] handoff failed: %v — old still streaming", cs.name, err)
				// Old connection will die on its own, loop will catch it
				<-conn.done
				conn.pc.Close()
				continue
			}

			// Wait for IDR keyframe on the new connection before swapping
			idrPkts := waitForIDR(newConn.pkts, 10*time.Second)
			if idrPkts == nil {
				log.Printf("[%s] no IDR on new connection — aborting handoff", cs.name)
				newConn.pc.Close()
				<-conn.done
				conn.pc.Close()
				continue
			}

			// ATOMIC SWAP: increment generation (old writer stops), write IDR, new writer starts
			newGen := cs.gen.Add(1)

			// Write buffered IDR packets (includes SPS/PPS) to the persistent stream
			cs.mu.RLock()
			s := cs.stream
			m := cs.media
			cs.mu.RUnlock()
			for _, pkt := range idrPkts {
				func() {
					defer func() { recover() }()
					s.WritePacketRTP(m, pkt)
				}()
			}

			// Start forwarding from new connection
			go forwardPackets(cs, newConn, newGen)
			log.Printf("[%s] seamless handoff done (gen %d → %d)", cs.name, myGen, newGen)

			// Close old connection (its forwardPackets goroutine will exit because gen changed)
			conn.pc.Close()

			// Loop with new connection
			conn = newConn
			myGen = newGen
			continue
		}
	}
}

// forwardPackets reads from a WebRTC connection and writes to the RTSP stream.
// Stops when the connection dies or the generation changes (another writer took over).
func forwardPackets(cs *cameraStream, conn *webrtcConn, myGen int64) {
	var packets, totalBytes, frames uint64
	var lastTimestamp uint32
	start := time.Now()
	statsTicker := time.NewTicker(10 * time.Second)
	defer statsTicker.Stop()

	go func() {
		for range statsTicker.C {
			if cs.gen.Load() != myGen {
				return
			}
			elapsed := time.Since(start).Seconds()
			if elapsed > 0 && frames > 0 {
				log.Printf("[%s] gen%d — %.1ffps %.2fMbps (%d frames)",
					cs.name, myGen, float64(frames)/elapsed,
					float64(totalBytes)*8/elapsed/1e6, frames)
			}
		}
	}()

	for pkt := range conn.pkts {
		// Check if we're still the active writer
		if cs.gen.Load() != myGen {
			return // Another connection took over — exit silently
		}

		packets++
		totalBytes += uint64(len(pkt.Payload))
		if pkt.Timestamp != lastTimestamp {
			frames++
			lastTimestamp = pkt.Timestamp
		}

		cs.mu.RLock()
		s := cs.stream
		m := cs.media
		cs.mu.RUnlock()

		if s != nil && m != nil {
			func() {
				defer func() { recover() }()
				s.WritePacketRTP(m, pkt)
			}()
		}
	}
}

// waitForIDR reads packets from the channel until it finds a keyframe.
// Looks for SPS (7), PPS (8), IDR (5), or STAP-A (24) containing SPS.
// Returns buffered packets from the keyframe. Returns nil on timeout.
func waitForIDR(pkts chan *pionrtp.Packet, timeout time.Duration) []*pionrtp.Packet {
	deadline := time.After(timeout)
	var buf []*pionrtp.Packet
	collecting := false

	for {
		select {
		case pkt, ok := <-pkts:
			if !ok {
				return nil
			}
			if len(pkt.Payload) < 2 {
				continue
			}
			nalType := getNALType(pkt.Payload)

			// Start collecting at SPS, STAP-A, or IDR
			if nalType == 7 || nalType == 24 || nalType == 5 {
				if !collecting {
					collecting = true
					buf = buf[:0]
				}
			}
			if collecting {
				buf = append(buf, pkt)
			}
			// Return once we have an IDR (or enough packets after SPS)
			if collecting && (nalType == 5 || (len(buf) > 3 && nalType != 7 && nalType != 8)) {
				return buf
			}
		case <-deadline:
			// Timeout — return whatever we have if we collected anything
			if len(buf) > 0 {
				return buf
			}
			return nil
		}
	}
}

// dialWebRTC establishes a WebRTC connection to Google Foyer and returns
// a packet channel. Does NOT touch the RTSP stream.
func dialWebRTC(cs *cameraStream, cookies map[string]string, apiKey string) (*webrtcConn, error) {
	log.Printf("[%s] connecting to %s", cs.name, cs.config.DeviceID)

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

	m := &webrtc.MediaEngine{}
	m.RegisterDefaultCodecs()
	for _, ext := range []string{
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
	} {
		m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: ext}, webrtc.RTPCodecTypeVideo)
	}
	for _, ext := range []string{
		"urn:ietf:params:rtp-hdrext:ssrc-audio-level",
		"http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time",
		"http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01",
		"urn:ietf:params:rtp-hdrext:sdes:mid",
	} {
		m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: ext}, webrtc.RTPCodecTypeAudio)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		return nil, fmt.Errorf("PeerConnection: %w", err)
	}

	pc.CreateDataChannel("dc", &webrtc.DataChannelInit{ID: uint16Ptr(1)})
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})

	pkts := make(chan *pionrtp.Packet, 300)
	done := make(chan error, 1)
	trackReady := make(chan struct{}, 1)

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeVideo {
			return
		}
		log.Printf("[%s] track: %s codec=%s", cs.name, track.Kind(), track.Codec().MimeType)

		// Send PLI for fast keyframe
		pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})

		select {
		case trackReady <- struct{}{}:
		default:
		}

		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				close(pkts)
				done <- fmt.Errorf("ReadRTP: %w", err)
				return
			}
			if len(pkt.Payload) < 2 {
				continue
			}
			select {
			case pkts <- pkt:
			default:
				// Drop packet if channel full (reader too slow)
			}
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[%s] webrtc: %s", cs.name, state)
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			done <- fmt.Errorf("connection %s", state)
		}
	})

	offer, _ := pc.CreateOffer(nil)
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	pc.SetLocalDescription(offer)
	<-gatherComplete

	resolution := cs.config.Resolution
	if resolution == 0 {
		resolution = 3
	}
	reqBody, _ := json.Marshal(map[string]interface{}{
		"action": "offer", "deviceId": cs.config.DeviceID,
		"sdp": pc.LocalDescription().SDP, "requestedVideoResolution": resolution,
	})
	req, _ := http.NewRequest("POST", "https://googlehomefoyer-pa.clients6.google.com/v1/join_stream", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", auth)
	req.Header.Set("Cookie", cookieStr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)
	req.Header.Set("x-goog-authuser", "0")
	req.Header.Set("x-foyer-client-environment", "CAc=")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("Foyer API: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		pc.Close()
		return nil, fmt.Errorf("Foyer %d: %s", resp.StatusCode, string(body[:min(len(body), 100)]))
	}

	var answer struct{ SDP string `json:"sdp"` }
	json.Unmarshal(body, &answer)

	hasTWCC := strings.Contains(answer.SDP, "transport-wide-cc")
	log.Printf("[%s] foyer answered: twcc=%v", cs.name, hasTWCC)

	err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP})
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("SetRemoteDescription: %w", err)
	}

	// Wait for video track
	select {
	case <-trackReady:
	case err := <-done:
		pc.Close()
		return nil, err
	case <-time.After(15 * time.Second):
		pc.Close()
		return nil, fmt.Errorf("timeout waiting for video track")
	}

	return &webrtcConn{pc: pc, pkts: pkts, done: done}, nil
}

func uint16Ptr(v uint16) *uint16 { return &v }
