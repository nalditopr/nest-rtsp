// nest-rtsp — Single binary: WebRTC from Google Foyer → RTSP server.
// Seamless make-before-break reconnection using h264 depacketizer.
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

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"gopkg.in/yaml.v3"
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

type cameraStream struct {
	name   string
	config CameraConfig
	media  *description.Media
	forma  *format.H264
	stream *gortsplib.ServerStream
	enc    *rtph264.Encoder
	gen    atomic.Int64 // active writer generation
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

// webrtcConn holds a live WebRTC connection
type webrtcConn struct {
	pc   *webrtc.PeerConnection
	pkts chan *pionrtp.Packet
	done chan error
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
	if err := handler.server.Start(); err != nil {
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

// depacketizeUntilIDR reads RTP packets, depacketizes into H.264 Access Units,
// and returns the first Access Unit that contains an IDR (keyframe) with SPS/PPS.
func depacketizeUntilIDR(pkts chan *pionrtp.Packet, timeout time.Duration) (au [][]byte, err error) {
	dec := &rtph264.Decoder{}
	if err := dec.Init(); err != nil {
		return nil, fmt.Errorf("decoder init: %w", err)
	}
	deadline := time.After(timeout)
	for {
		select {
		case pkt, ok := <-pkts:
			if !ok {
				return nil, fmt.Errorf("channel closed")
			}
			nalus, err := dec.Decode(pkt)
			if err != nil {
				continue // incomplete fragment, need more packets
			}
			// nalus is a complete Access Unit (slice of NAL unit byte slices)
			if h264.IsRandomAccess(nalus) {
				return nalus, nil
			}
		case <-deadline:
			return nil, fmt.Errorf("timeout")
		}
	}
}

func startCamera(cs *cameraStream, server *gortsplib.Server, cookies map[string]string, apiKey string, initialStagger time.Duration) {
	reconnectInterval := 5 * time.Minute
	firstRun := true
	failures := 0

	for {
		conn, err := dialWebRTC(cs, cookies, apiKey)
		if err != nil {
			failures++
			delay := min(2*time.Second*time.Duration(1<<min(failures-1, 7)), 5*time.Minute)
			log.Printf("[%s] error: %v — retry in %v", cs.name, err, delay)
			time.Sleep(delay)
			continue
		}
		failures = 0

		// Depacketize until first IDR — gives us clean SPS/PPS
		idr, err := depacketizeUntilIDR(conn.pkts, 15*time.Second)
		if err != nil {
			log.Printf("[%s] no keyframe: %v — retrying", cs.name, err)
			conn.pc.Close()
			continue
		}

		// Extract SPS and PPS from the Access Unit
		var sps, pps []byte
		for _, nalu := range idr {
			if len(nalu) == 0 {
				continue
			}
			switch h264.NALUType(nalu[0] & 0x1F) {
			case h264.NALUTypeIDR:
				// IDR — we have the keyframe
			case 7: // SPS
				sps = nalu
			case 8: // PPS
				pps = nalu
			}
		}
		log.Printf("[%s] got IDR: %d NALUs, SPS=%d PPS=%d", cs.name, len(idr), len(sps), len(pps))

		// Create persistent RTSP stream on first connection
		cs.mu.Lock()
		if cs.stream == nil {
			cs.forma = &format.H264{
				PayloadTyp:        96,
				PacketizationMode: 1,
				SPS:               sps,
				PPS:               pps,
			}
			cs.media = &description.Media{
				Type:    description.MediaTypeVideo,
				Formats: []format.Format{cs.forma},
			}
			stream := &gortsplib.ServerStream{
				Server: server,
				Desc:   &description.Session{Medias: []*description.Media{cs.media}},
			}
			if err := stream.Initialize(); err != nil {
				cs.mu.Unlock()
				log.Printf("[%s] stream init error: %v", cs.name, err)
				conn.pc.Close()
				continue
			}
			cs.stream = stream
			cs.enc = &rtph264.Encoder{PayloadType: 96}
			cs.enc.Init()
			log.Printf("[%s] RTSP stream created (SPS=%d PPS=%d)", cs.name, len(sps), len(pps))
		}
		cs.mu.Unlock()

		// Write the IDR access unit to the stream
		myGen := cs.gen.Add(1)
		writeAU(cs, idr, myGen)
		log.Printf("[%s] streaming (gen %d)", cs.name, myGen)

		// Start forwarding in background
		go forwardLoop(cs, conn, myGen)

		// Wait for reconnect time or connection death
		waitTime := reconnectInterval
		if firstRun && initialStagger > 0 && initialStagger < reconnectInterval {
			waitTime = initialStagger
			firstRun = false
		}

		select {
		case err := <-conn.done:
			log.Printf("[%s] connection dropped: %v", cs.name, err)
			conn.pc.Close()
			// Don't close the stream — just reconnect

		case <-time.After(waitTime):
			// Seamless handoff: old keeps streaming while new connects
			log.Printf("[%s] seamless handoff starting", cs.name)

			newConn, err := dialWebRTC(cs, cookies, apiKey)
			if err != nil {
				log.Printf("[%s] handoff dial failed: %v", cs.name, err)
				<-conn.done
				conn.pc.Close()
				continue
			}

			// Wait for IDR on new connection (old still streaming)
			newIDR, err := depacketizeUntilIDR(newConn.pkts, 10*time.Second)
			if err != nil {
				log.Printf("[%s] handoff no IDR: %v", cs.name, err)
				newConn.pc.Close()
				<-conn.done
				conn.pc.Close()
				continue
			}

			// ATOMIC SWAP: bump gen (old writer stops), write new IDR, start new writer
			newGen := cs.gen.Add(1)
			writeAU(cs, newIDR, newGen)
			go forwardLoop(cs, newConn, newGen)
			log.Printf("[%s] seamless handoff done (gen %d)", cs.name, newGen)

			// Close old
			conn.pc.Close()
			conn = newConn
			myGen = newGen
			continue
		}
	}
}

// writeAU encodes an Access Unit (slice of NALUs) into RTP and writes to the RTSP stream
func writeAU(cs *cameraStream, au [][]byte, gen int64) {
	if cs.gen.Load() != gen {
		return
	}
	cs.mu.RLock()
	s := cs.stream
	m := cs.media
	e := cs.enc
	cs.mu.RUnlock()
	if s == nil || m == nil || e == nil {
		return
	}
	pkts, err := e.Encode(au)
	if err != nil {
		return
	}
	for _, pkt := range pkts {
		func() {
			defer func() { recover() }()
			s.WritePacketRTP(m, pkt)
		}()
	}
}

// forwardLoop depacketizes RTP from WebRTC and writes Access Units to the RTSP stream
func forwardLoop(cs *cameraStream, conn *webrtcConn, myGen int64) {
	dec := &rtph264.Decoder{}
	dec.Init()

	var frames uint64
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
				log.Printf("[%s] gen%d — %.1ffps (%d frames)", cs.name, myGen, float64(frames)/elapsed, frames)
			}
		}
	}()

	for pkt := range conn.pkts {
		if cs.gen.Load() != myGen {
			return // another connection took over
		}
		nalus, err := dec.Decode(pkt)
		if err != nil {
			continue // incomplete, need more packets
		}
		frames++
		writeAU(cs, nalus, myGen)
	}
}

// dialWebRTC establishes a WebRTC connection and returns a packet channel
func dialWebRTC(cs *cameraStream, cookies map[string]string, apiKey string) (*webrtcConn, error) {
	log.Printf("[%s] connecting to %s", cs.name, cs.config.DeviceID)

	origin := "https://home.google.com"
	ts := time.Now().Unix()
	hh := sha1.New()
	fmt.Fprintf(hh, "%d %s %s", ts, cookies["SAPISID"], origin)
	hash := fmt.Sprintf("%d_%x", ts, hh.Sum(nil))
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
	log.Printf("[%s] foyer answered: twcc=%v", cs.name, strings.Contains(answer.SDP, "transport-wide-cc"))

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		pc.Close()
		return nil, fmt.Errorf("SetRemoteDescription: %w", err)
	}

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
