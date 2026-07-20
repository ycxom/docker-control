package controller

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type websocketConn struct {
	conn    net.Conn
	reader  *bufio.Reader
	writeMu sync.Mutex
	closed  atomic.Bool
}

func upgradeWebSocket(response http.ResponseWriter, request *http.Request) (*websocketConn, error) {
	if !headerContains(request.Header, "Connection", "upgrade") || !headerContains(request.Header, "Upgrade", "websocket") {
		return nil, errors.New("websocket upgrade required")
	}
	if request.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, errors.New("websocket version 13 required")
	}
	key := strings.TrimSpace(request.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, errors.New("Sec-WebSocket-Key is required")
	}
	hijacker, ok := response.(http.Hijacker)
	if !ok {
		return nil, errors.New("HTTP server does not support websocket hijacking")
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + websocketGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	if _, err := fmt.Fprintf(buffered, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := buffered.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &websocketConn{conn: conn, reader: buffered.Reader}, nil
}

func headerContains(header http.Header, name, wanted string) bool {
	for _, value := range header.Values(name) {
		for _, item := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(item), wanted) {
				return true
			}
		}
	}
	return false
}

func (ws *websocketConn) readText(maxPayload int64) ([]byte, error) {
	for {
		header := make([]byte, 2)
		if _, err := io.ReadFull(ws.reader, header); err != nil {
			return nil, err
		}
		if header[0]&0x80 == 0 {
			return nil, errors.New("fragmented websocket frames are not supported")
		}
		opcode := header[0] & 0x0f
		masked := header[1]&0x80 != 0
		if !masked {
			return nil, errors.New("client websocket frames must be masked")
		}
		length := int64(header[1] & 0x7f)
		switch length {
		case 126:
			var value uint16
			if err := binary.Read(ws.reader, binary.BigEndian, &value); err != nil {
				return nil, err
			}
			length = int64(value)
		case 127:
			var value uint64
			if err := binary.Read(ws.reader, binary.BigEndian, &value); err != nil {
				return nil, err
			}
			if value > uint64(maxPayload) {
				return nil, errors.New("websocket frame exceeds limit")
			}
			length = int64(value)
		}
		if length > maxPayload {
			return nil, errors.New("websocket frame exceeds limit")
		}
		mask := make([]byte, 4)
		if _, err := io.ReadFull(ws.reader, mask); err != nil {
			return nil, err
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(ws.reader, payload); err != nil {
			return nil, err
		}
		for index := range payload {
			payload[index] ^= mask[index%4]
		}
		switch opcode {
		case 0x1:
			return payload, nil
		case 0x8:
			return nil, io.EOF
		case 0x9:
			if err := ws.writeFrame(0xA, payload); err != nil {
				return nil, err
			}
		case 0xA:
			continue
		default:
			return nil, fmt.Errorf("unsupported websocket opcode %d", opcode)
		}
	}
}

func (ws *websocketConn) writeJSON(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return ws.writeFrame(0x1, payload)
}

func (ws *websocketConn) writeFrame(opcode byte, payload []byte) error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	if ws.closed.Load() {
		return net.ErrClosed
	}
	var header []byte
	switch {
	case len(payload) < 126:
		header = []byte{0x80 | opcode, byte(len(payload))}
	case len(payload) <= 65535:
		header = make([]byte, 4)
		header[0] = 0x80 | opcode
		header[1] = 126
		binary.BigEndian.PutUint16(header[2:], uint16(len(payload)))
	default:
		header = make([]byte, 10)
		header[0] = 0x80 | opcode
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:], uint64(len(payload)))
	}
	if _, err := ws.conn.Write(header); err != nil {
		return err
	}
	_, err := ws.conn.Write(payload)
	return err
}

func (ws *websocketConn) close(code uint16, reason string) {
	if ws.closed.Swap(true) {
		return
	}
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, code)
	copy(payload[2:], reason)
	ws.writeMu.Lock()
	var frame bytesFrame
	frame.add(0x8, payload)
	_, _ = ws.conn.Write(frame)
	ws.writeMu.Unlock()
	_ = ws.conn.Close()
}

type bytesFrame []byte

func (frame *bytesFrame) add(opcode byte, payload []byte) {
	if len(payload) < 126 {
		*frame = append(*frame, 0x80|opcode, byte(len(payload)))
	} else {
		*frame = append(*frame, 0x80|opcode, 126, byte(len(payload)>>8), byte(len(payload)))
	}
	*frame = append(*frame, payload...)
}
