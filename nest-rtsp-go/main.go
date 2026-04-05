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
	"syscall"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"gopkg.in/yaml.v3"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
)

// Config
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

// Camera stream state
type cameraStream struct {
	name   string
	config CameraConfig
	pc     *webrtc.PeerConnection
	media  *description.Media
	stream *gortsplib.ServerStream
	mu     sync.RWMutex
}

// RTSP server handler — serves all camera streams
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
	// Extract camera name from path (e.g., "/culdesac")
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

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config file")
	flag.Parse()

	// Load config
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

	// Load cookies
	cookieData, err := os.ReadFile(config.CookiesFile)
	if err != nil {
		log.Fatalf("Cannot read cookies: %v", err)
	}
	var cookies map[string]string
	json.Unmarshal(cookieData, &cookies)
	if cookies["SAPISID"] == "" {
		log.Fatal("No SAPISID in cookies")
	}

	// Create RTSP server
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

	// Start each camera
	for name, cam := range config.Cameras {
		if cam.DeviceID == "" {
			continue
		}
		cs := &cameraStream{name: name, config: cam}
		handler.mu.Lock()
		handler.cameras[name] = cs
		handler.mu.Unlock()

		go startCamera(cs, handler.server, cookies, config.APIKey)
		time.Sleep(500 * time.Millisecond)
	}

	// Wait for signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("Shutting down...")
	handler.server.Close()
}

func startCamera(cs *cameraStream, server *gortsplib.Server, cookies map[string]string, apiKey string) {
	failures := 0
	for {
		err := connectCamera(cs, server, cookies, apiKey)
		if err != nil {
			failures++
			delay := min(2*time.Second*time.Duration(1<<min(failures-1, 7)), 5*time.Minute)
			log.Printf("[%s] error: %v — retry in %v", cs.name, err, delay)
			time.Sleep(delay)
			continue
		}
		failures = 0
	}
}

func connectCamera(cs *cameraStream, server *gortsplib.Server, cookies map[string]string, apiKey string) error {
	log.Printf("[%s] connecting to %s", cs.name, cs.config.DeviceID)

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

	// Pion MediaEngine with Chrome-like codecs + extensions
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
		return fmt.Errorf("PeerConnection: %w", err)
	}
	cs.pc = pc

	pc.CreateDataChannel("dc", &webrtc.DataChannelInit{ID: uint16Ptr(1)})
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})

	// Wait for tracks and feed into RTSP stream
	done := make(chan error, 1)
	trackReady := make(chan struct{}, 1)

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[%s] recovered from panic in track handler: %v", cs.name, r)
				done <- fmt.Errorf("panic: %v", r)
			}
		}()

		codec := track.Codec()
		log.Printf("[%s] track: %s codec=%s pt=%d", cs.name, track.Kind(), codec.MimeType, codec.PayloadType)

		if track.Kind() == webrtc.RTPCodecTypeVideo {
			// Create RTSP stream with H.264 format
			forma := &format.H264{
				PayloadTyp:        uint8(codec.PayloadType),
				PacketizationMode: 1,
			}
			media := &description.Media{
				Type:    description.MediaTypeVideo,
				Formats: []format.Format{forma},
			}
			cs.mu.Lock()
			cs.media = media
			stream := &gortsplib.ServerStream{
				Server: server,
				Desc:   &description.Session{Medias: []*description.Media{media}},
			}
			err := stream.Initialize()
			if err != nil {
				cs.mu.Unlock()
				log.Printf("[%s] stream init error: %v", cs.name, err)
				return
			}
			cs.stream = stream
			cs.mu.Unlock()

			log.Printf("[%s] RTSP stream ready", cs.name)

			// Signal ready
			select {
			case trackReady <- struct{}{}:
			default:
			}

			// Send PLI
			pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})

			// Forward RTP packets and log stats
			var packets, totalBytes uint64
			var frames uint64
			var lastTimestamp uint32
			var firstPkt time.Time
			statsTimer := time.NewTicker(10 * time.Second)
			defer statsTimer.Stop()

			go func() {
				for range statsTimer.C {
					elapsed := time.Since(firstPkt).Seconds()
					if elapsed > 0 && frames > 0 {
						fps := float64(frames) / elapsed
						mbps := float64(totalBytes) * 8 / elapsed / 1e6
						log.Printf("[%s] %s %dx — %.1ffps %.2fMbps (%d frames, %d pkts)",
							cs.name, codec.MimeType, 0, fps, mbps, frames, packets)
					}
				}
			}()

			for {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					done <- fmt.Errorf("ReadRTP: %w", err)
					return
				}
				if len(pkt.Payload) < 2 {
					continue
				}

				if firstPkt.IsZero() {
					firstPkt = time.Now()
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
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[%s] webrtc: %s", cs.name, state)
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			done <- fmt.Errorf("connection %s", state)
		}
	})

	// Create offer
	offer, _ := pc.CreateOffer(nil)
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	pc.SetLocalDescription(offer)
	<-gatherComplete

	// Call Foyer API
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
		return fmt.Errorf("Foyer API: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		pc.Close()
		return fmt.Errorf("Foyer %d: %s", resp.StatusCode, string(body[:min(len(body), 100)]))
	}

	var answer struct{ SDP string `json:"sdp"` }
	json.Unmarshal(body, &answer)

	// Log what Foyer offered
	var answerCodecs []string
	for _, line := range strings.Split(answer.SDP, "\n") {
		if strings.Contains(line, "rtpmap") && !strings.Contains(line, "rtx") &&
			!strings.Contains(line, "red") && !strings.Contains(line, "ulpfec") {
			answerCodecs = append(answerCodecs, strings.TrimSpace(line))
		}
	}
	hasTWCC := strings.Contains(answer.SDP, "transport-wide-cc")
	log.Printf("[%s] foyer answered: %d codecs, twcc=%v", cs.name, len(answerCodecs), hasTWCC)
	for _, c := range answerCodecs {
		log.Printf("[%s]   %s", cs.name, c)
	}

	err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP})
	if err != nil {
		pc.Close()
		return fmt.Errorf("SetRemoteDescription: %w", err)
	}

	log.Printf("[%s] negotiated, waiting for video...", cs.name)

	// Wait for either track ready or failure
	select {
	case <-trackReady:
		log.Printf("[%s] streaming → rtsp://localhost%s/%s", cs.name, server.RTSPAddress, cs.name)
	case err := <-done:
		pc.Close()
		return err
	case <-time.After(15 * time.Second):
		pc.Close()
		return fmt.Errorf("timeout waiting for video track")
	}

	// Proactive reconnect before Google kills us (~5.5min session limit)
	// Reconnect at 4.5 minutes to avoid disruption
	reconnectTimer := time.NewTimer(4*time.Minute + 30*time.Second)
	defer reconnectTimer.Stop()

	select {
	case err = <-done:
		// Connection dropped by Google or network error
	case <-reconnectTimer.C:
		// Proactive reconnect — prevents the 5.5min Google timeout
		log.Printf("[%s] proactive reconnect (4m30s)", cs.name)
		err = nil
	}

	cs.mu.Lock()
	if cs.stream != nil {
		cs.stream.Close()
		cs.stream = nil
	}
	cs.mu.Unlock()
	pc.Close()
	return err
}

func uint16Ptr(v uint16) *uint16 { return &v }
