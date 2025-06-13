package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"encoding/base64"
	"encoding/json"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type Peer struct {
	peerConnection        *webrtc.PeerConnection
	localTrack            *webrtc.TrackLocalStaticRTP
	remoteVideoConnection *net.UDPConn
	remoteAudioConnection *net.UDPConn
	gatherComplete        <-chan struct{}
}

func startPeerConnection() (*webrtc.PeerConnection, *webrtc.TrackLocalStaticRTP, error) {
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	})
	if err != nil {
		return nil, nil, err
		// panic(fmt.Sprintf("problem with stun: %s", err))
	}

	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "video", "pion",
	)
	if err != nil {
		return nil, nil, err
		// panic(fmt.Sprintf("problem with NewTrackLocalStaticRTP: %s", err))
	}

	rtpSender, err := peerConnection.AddTrack(localTrack)
	if err != nil {
		return nil, nil, err
	}
	_ = rtpSender

	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion",
	)
	if err != nil {
		return nil, nil, err
	}

	rtpSender, err = peerConnection.AddTrack(audioTrack)
	if err != nil {
		return nil, nil, err
	}
	_ = rtpSender

	// // Read incoming RTCP packets
	// // Before these packets are returned they are processed by interceptors. For things
	// // like NACK this needs to be called.
	// go func() {
	// 	rtcpBuf := make([]byte, 1500)
	// 	for {
	// 		if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
	// 			return
	// 		}
	// 	}
	// }()
	return peerConnection, localTrack, nil
}

func newPeerConnection(peers *[]Peer) int {
	for {
		peerConnection, localTrack, err := startPeerConnection()

		if err != nil {
			fmt.Fprintf(os.Stderr, "error setting up peer connection. will retry: %s\n", err)
			continue
		}

		*peers = append(*peers, Peer{
			peerConnection:        peerConnection,
			localTrack:            localTrack,
			remoteVideoConnection: nil,
			remoteAudioConnection: nil,
		})

		peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
			// fmt.Fprintf(os.Stderr, "Connection State has changed %s \n", connectionState.String())
			peerIndex := 0
			for {
				if peerIndex == len(*peers) {
					panic("logic: unidentified peer connection")
				}
				if (*peers)[peerIndex].peerConnection == peerConnection {
					break
				}
				peerIndex++
			}

			fmt.Fprintf(os.Stderr, "peer %d: state - %s\n", peerIndex, connectionState.String())

			if connectionState == webrtc.ICEConnectionStateFailed || connectionState == webrtc.ICEConnectionStateDisconnected || connectionState == webrtc.ICEConnectionStateClosed {
				if (*peers)[peerIndex].peerConnection != nil {
					err := (*peers)[peerIndex].peerConnection.Close()
					if err != nil {
						fmt.Fprintf(os.Stderr, "peer %d: onclosed - %s\n", peerIndex, err)
					}

					(*peers)[peerIndex].peerConnection = nil
				}
				(*peers)[peerIndex].localTrack = nil

				if (*peers)[peerIndex].remoteAudioConnection != nil {
					(*peers)[peerIndex].remoteAudioConnection.Close()
					(*peers)[peerIndex].remoteAudioConnection = nil
				}
				if (*peers)[peerIndex].remoteVideoConnection != nil {
					(*peers)[peerIndex].remoteVideoConnection.Close()
					(*peers)[peerIndex].remoteVideoConnection = nil
				}
			}
		})

		return len(*peers) - 1
	}
}

func setupTrackHandler(peers []Peer, peerIndex int) {
	for index, peer := range peers {
		if index == peerIndex {
			continue
		}

		if peer.remoteAudioConnection != nil {
			err := peer.remoteAudioConnection.Close()
			if err != nil {
				fmt.Fprintf(os.Stderr, "peer %d: remoteAudioConnection.Close() - %s\n", peerIndex, err)
			}
			peer.remoteAudioConnection = nil
		}

		if peer.remoteVideoConnection != nil {
			err := peer.remoteVideoConnection.Close()
			if err != nil {
				fmt.Fprintf(os.Stderr, "peer %d: remoteAudioConnection.Close() - %s\n", peerIndex, err)
			}
			peer.remoteVideoConnection = nil
		}
	}

	var localAddress *net.UDPAddr
	var err error

	localAddress, err = net.ResolveUDPAddr("udp", "127.0.0.1:")
	if err != nil {
		panic(fmt.Sprintf("logic: net.ResolveUDPAddr for local - %s", err))
	}

	var remoteAddressAudio *net.UDPAddr
	remoteAddressAudio, err = net.ResolveUDPAddr("udp", "127.0.0.1:4001")
	if err != nil {
		panic(fmt.Sprintf("logic: net.ResolveUDPAddr for remote audio - %s", err))
	}

	peers[peerIndex].remoteAudioConnection, err = net.DialUDP("udp", localAddress, remoteAddressAudio)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer %d: audio - net.DialUDP - %s\n", peerIndex, err)
	}

	var remoteAddressVideo *net.UDPAddr
	remoteAddressVideo, err = net.ResolveUDPAddr("udp", "127.0.0.1:4002")
	if err != nil {
		panic(fmt.Sprintf("logic: net.ResolveUDPAddr for remote video - %s", err))
	}

	peers[peerIndex].remoteVideoConnection, err = net.DialUDP("udp", localAddress, remoteAddressVideo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer %d: video - net.DialUDP - %s\n", peerIndex, err)
	}

	peers[peerIndex].peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		connection, payloadType := func(track *webrtc.TrackRemote) (*net.UDPConn, uint8) {
			if track.Kind().String() == "video" {
				return peers[peerIndex].remoteVideoConnection, 111
			} else {
				return peers[peerIndex].remoteAudioConnection, 96
			}
		}(track)

		buf := make([]byte, 1500)
		rtpPacket := &rtp.Packet{}
		for {
			if peers[peerIndex].peerConnection == nil || peers[peerIndex].remoteAudioConnection == nil || peers[peerIndex].remoteVideoConnection == nil {
				break
			}

			n, _, err := track.Read(buf)
			if err != nil {
				fmt.Fprintf(os.Stderr, "peer %d: track read - %s\n", peerIndex, err)
				break
			}

			err = rtpPacket.Unmarshal(buf[:n])
			if err != nil {
				fmt.Fprintf(os.Stderr, "peer %d: rtp packet unmarshal - %s\n", peerIndex, err)
			}
			rtpPacket.PayloadType = payloadType

			n, err = rtpPacket.MarshalTo(buf)
			if err != nil {
				fmt.Fprintf(os.Stderr, "peer %d: rtp packet marshal - %s\n", peerIndex, err)
			}

			_, err = connection.Write(buf[:n])
			if err != nil {
				var opError *net.OpError
				if errors.As(err, &opError) && opError.Err.Error() == "write: connection refused" {
					continue
				}
				fmt.Fprintf(os.Stderr, "peer %d: rtp packet write - %s\n", peerIndex, err)
				break
			}
		}
	})
}

func signalHostSetup(signalServer string, hostId string, peers []Peer, peerIndex int) {
	for {
		hostSignal, err := http.Post(fmt.Sprintf("%s/api/host", signalServer), "application/json; charset=UTF-8", bytes.NewBufferString(fmt.Sprintf("{\"id\": \"%s\", \"description\": \"%s\"}", hostId, encode(peers[peerIndex].peerConnection.LocalDescription()))))

		if err != nil {
			fmt.Fprintf(os.Stderr, "peer %d: problem with setting up hostId with signalling server: %s", peerIndex, err)
			continue
		}
		defer hostSignal.Body.Close()

		if hostSignal.StatusCode == http.StatusOK {
			// var hostSignalBody map[string]interface{}

			// json.NewDecoder(hostSignal.Body).Decode(hostSignalBody)
		} else {
			fmt.Fprintf(os.Stderr, "peer %d: problem with setting up hostId with signalling server: %s", peerIndex, "response status")
			continue
		}

		break
	}
}

func signalWaitForGuest(signalServer string, hostId string, peers []Peer, peerIndex int) {
	for {
		time.Sleep(1 * time.Second)

		params := url.Values{}
		params.Add("hostId", hostId)

		guestSignal, err := http.Get(fmt.Sprintf("%s/api/guest?%s", signalServer, params.Encode()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "peer %d: problem with getting guest information with signalling server: %s\n", peerIndex, err)
			continue
		}
		defer guestSignal.Body.Close()

		if guestSignal.StatusCode == http.StatusOK {
			var guestDescriptionObject map[string]string
			json.NewDecoder(guestSignal.Body).Decode(&guestDescriptionObject)

			guestDescription := guestDescriptionObject["guestDescription"]
			if guestDescription != "" {
				guestOffer := webrtc.SessionDescription{}
				decode(guestDescription, &guestOffer)
				peers[peerIndex].peerConnection.SetRemoteDescription(guestOffer)
				break
			}
		} else {
			fmt.Fprintf(os.Stderr, "peer %d: problem with getting guest information with signalling server: %s\n", peerIndex, "response status")
			continue
		}
	}
}

func streamLocalTrack(peers []Peer, peerIndex int) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 4000})
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer %d: net.ListenUDP, %s\n", peerIndex, err)
	}

	defer func() {
		if err = listener.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "peer %d: listener.Close, %s\n", peerIndex, err)
		}
	}()

	// Increase the UDP receive buffer size
	// Default UDP buffer sizes vary on different operating systems
	bufferSize := 300000 // 300KB
	err = listener.SetReadBuffer(bufferSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer %d: listener.SetReadBuffer, %s\n", peerIndex, err)
	}

	inboundRTPPacket := make([]byte, 1600) // UDP MTU
	for {
		if peers[peerIndex].localTrack == nil {
			fmt.Fprintf(os.Stderr, "peer %d: stop writing to track\n", peerIndex)

			if peers[peerIndex].peerConnection != nil {
				peers[peerIndex].peerConnection.Close()
				peers[peerIndex].peerConnection = nil
			}
			peers[peerIndex].localTrack = nil

			if peers[peerIndex].remoteAudioConnection != nil {
				peers[peerIndex].remoteAudioConnection.Close()
				peers[peerIndex].remoteAudioConnection = nil
			}
			if peers[peerIndex].remoteVideoConnection != nil {
				peers[peerIndex].remoteVideoConnection.Close()
				peers[peerIndex].remoteVideoConnection = nil
			}

			break
		}

		listener.SetReadDeadline(time.Now().Add(time.Second))
		readBytes, _, err := listener.ReadFrom(inboundRTPPacket)
		if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "peer %d: error during read from listener: %s\n", peerIndex, err)
		}

		// fmt.Println(readBytes)
		_, err = peers[peerIndex].localTrack.Write(inboundRTPPacket[:readBytes])
		if err != nil {
			if errors.Is(err, io.ErrClosedPipe) {
				if peers[peerIndex].peerConnection != nil {
					peers[peerIndex].peerConnection.Close()
					peers[peerIndex].peerConnection = nil
				}
				peers[peerIndex].localTrack = nil

				if peers[peerIndex].remoteAudioConnection != nil {
					peers[peerIndex].remoteAudioConnection.Close()
					peers[peerIndex].remoteAudioConnection = nil
				}
				if peers[peerIndex].remoteVideoConnection != nil {
					peers[peerIndex].remoteVideoConnection.Close()
					peers[peerIndex].remoteVideoConnection = nil
				}

				break
			}

			fmt.Fprintf(os.Stderr, "peer %d: error during write to track: %s\n", peerIndex, err)
		}
		// fmt.Println(writtenBytes)
	}
}

func main() {
	if 3 != len(os.Args) {
		fmt.Fprintf(os.Stderr, "example usage: ./host-golang https://meetrostation.com \"secret host room id\"\n")
		return
	}
	signalServer := os.Args[1]
	hostId := os.Args[2]
	// signalServer := "https://meetrostation.com"
	// hostId := "secret host room id"

	var peers []Peer
	var peerIndex int

	for {
		peerIndex = newPeerConnection(&peers)

		var offerSessionDescription webrtc.SessionDescription
		var err error

		for {
			offerSessionDescription, err = peers[peerIndex].peerConnection.CreateOffer(nil)

			if err != nil {
				fmt.Fprintf(os.Stderr, "error creating offer: %s\n", err)
				continue
			}
			break
		}

		// later will be locking untill this channel completes
		peers[peerIndex].gatherComplete = webrtc.GatheringCompletePromise(peers[peerIndex].peerConnection)

		for {
			err = peers[peerIndex].peerConnection.SetLocalDescription(offerSessionDescription)
			if err != nil {
				fmt.Fprintf(os.Stderr, "problem setting local description: %s\n", err)
				continue
			}
			break
		}

		setupTrackHandler(peers, peerIndex)

		fmt.Fprintf(os.Stderr, "peer %d: waiting for ice stuff\n", peerIndex)
		<-peers[peerIndex].gatherComplete

		fmt.Fprintf(os.Stderr, "peer %d: ice stuff is ready\n", peerIndex)

		signalHostSetup(signalServer, hostId, peers, peerIndex)

		fmt.Fprintf(os.Stderr, "peer %d: waiting for the peer to join\n", peerIndex)

		signalWaitForGuest(signalServer, hostId, peers, peerIndex)

		fmt.Fprintf(os.Stderr, "peer %d: waiting for the peer connection\n", peerIndex)

		time.Sleep(3 * time.Second)

		streamLocalTrack(peers, peerIndex)
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
