package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"encoding/base64"
	"encoding/json"

	"github.com/pion/webrtc/v4"
)

func main() {
	if 3 != len(os.Args) {
		fmt.Fprintf(os.Stderr, "example usage: ffmpeg | ./host-golang https://meetrostation.com \"secret host room id\" | ffplay\n")
		return
	}
	signalServer := os.Args[1]
	hostId := os.Args[2]

	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	})
	if err != nil {
		panic(fmt.Sprintf("problem with stun: %s", err))
	}

	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion",
	)
	if err != nil {
		panic(fmt.Sprintf("problem with NewTrackLocalStaticRTP: %s", err))
	}

	rtpSender, err := peerConnection.AddTrack(videoTrack)
	if err != nil {
		panic(fmt.Sprintf("problem with AddTrack: %s", err))
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Fprintf(os.Stderr, "Connection State has changed %s \n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateFailed {
			if closeErr := peerConnection.Close(); closeErr != nil {
				panic(closeErr)
			}
		}
	})

	// Create offer
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		panic(fmt.Sprintf("problem with CreateOffer: %s", err))
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	if err = peerConnection.SetLocalDescription(offer); err != nil {
		panic(fmt.Sprintf("problem with SetLocalDescription: %s", err))
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	fmt.Fprintln(os.Stderr, "waiting for ice stuff")
	<-gatherComplete

	fmt.Fprintln(os.Stderr, "ice stuff is ready")
	// Output the offer in base64 so we can paste it in browser
	// fmt.Fprintln(os.Stderr, encode(peerConnection.LocalDescription()))

	hostSignal, err := http.Post(fmt.Sprintf("%s/api/host", signalServer), "application/json; charset=UTF-8", bytes.NewBufferString(fmt.Sprintf("{\"id\": \"%s\", \"description\": \"%s\"}", hostId, encode(peerConnection.LocalDescription()))))

	if err != nil {
		panic(fmt.Sprintf("problem with setting up hostId with signalling server: %s", err))
	}
	defer hostSignal.Body.Close()

	if hostSignal.StatusCode == http.StatusOK {
		// var hostSignalBody map[string]interface{}

		// json.NewDecoder(hostSignal.Body).Decode(hostSignalBody)
	} else {
		panic("another problem with setting up hostId with signalling server")
	}

	fmt.Fprintln(os.Stderr, "waiting for the peer to join")
	for {
		time.Sleep(1 * time.Second)

		params := url.Values{}
		params.Add("hostId", hostId)

		guestSignal, err := http.Get(fmt.Sprintf("%s/api/guest?%s", signalServer, params.Encode()))
		if err != nil {
			panic(fmt.Sprintf("problem with getting guest information with signalling server: %s", err))
		}
		defer guestSignal.Body.Close()

		if guestSignal.StatusCode == http.StatusOK {
			var guestDescriptionObject map[string]string
			json.NewDecoder(guestSignal.Body).Decode(&guestDescriptionObject)

			guestDescription := guestDescriptionObject["guestDescription"]
			if guestDescription != "" {
				guestOffer := webrtc.SessionDescription{}
				decode(guestDescription, &guestOffer)
				peerConnection.SetRemoteDescription(guestOffer)
				break
			}
		} else {
			panic("another problem with getting guest information with signalling server")
		}
	}
	fmt.Fprintf(os.Stderr, "waiting for the peer connection")

	time.Sleep(3 * time.Second)

	// Read RTP packets forever and send them to the WebRTC Client
	buffer := make([]byte, 1600) // UDP MTU
	// index := 0
	for {
		n, err := os.Stdin.Read(buffer)
		if err != nil {
			if err == io.EOF {
				fmt.Fprintf(os.Stderr, "stdin EOF")
				break
			}
			fmt.Fprintf(os.Stderr, "stdin error: %s", err)
			continue
		}

		if _, err = videoTrack.Write(buffer[:n]); err != nil {
			if errors.Is(err, io.ErrClosedPipe) {
				fmt.Fprintf(os.Stderr, "peerConnection closed")
				return
			}

			if err != nil {
				fmt.Fprintf(os.Stderr, "write track error: %s\n", err.Error())
			} else {
				// index = n
			}
		}
	}
}

// JSON encode + base64 a SessionDescription.
func encode(obj *webrtc.SessionDescription) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(b)
}

// Decode a base64 and unmarshal JSON into a SessionDescription.
func decode(in string, obj *webrtc.SessionDescription) {
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		panic(err)
	}

	if err = json.Unmarshal(b, obj); err != nil {
		panic(err)
	}
}
