// +build !js

package websocket

import (
	"context"
	"encoding/binary"
	"log"
	"time"

	"golang.org/x/xerrors"

	"nhooyr.io/websocket/internal/errd"
)

// Close performs the WebSocket close handshake with the given status code and reason.
//
// It will write a WebSocket close frame with a timeout of 5s and then wait 5s for
// the peer to send a close frame.
// All data messages received from the peer during the close handshake will be discarded.
//
// The connection can only be closed once. Additional calls to Close
// are no-ops.
//
// The maximum length of reason must be 125 bytes. Avoid
// sending a dynamic reason.
//
// Close will unblock all goroutines interacting with the connection once
// complete.
func (c *Conn) Close(code StatusCode, reason string) error {
	return c.closeHandshake(code, reason)
}

func (c *Conn) closeHandshake(code StatusCode, reason string) (err error) {
	defer errd.Wrap(&err, "failed to close WebSocket")

	err = c.writeClose(code, reason)
	if CloseStatus(err) == -1 {
		return err
	}

	err = c.waitCloseHandshake()
	if CloseStatus(err) == -1 {
		return err
	}
	return nil
}

func (c *Conn) writeClose(code StatusCode, reason string) error {
	c.closeMu.Lock()
	closing := c.wroteClose
	c.wroteClose = true
	c.closeMu.Unlock()
	if closing {
		return xerrors.New("already wrote close")
	}

	ce := CloseError{
		Code:   code,
		Reason: reason,
	}

	c.setCloseErr(xerrors.Errorf("sent close frame: %w", ce))

	var p []byte
	if ce.Code != StatusNoStatusRcvd {
		var err error
		p, err = ce.bytes()
		if err != nil {
			log.Printf("websocket: %v", err)
			return err
		}
	}

	return c.writeControl(context.Background(), opClose, p)
}

func (c *Conn) waitCloseHandshake() error {
	defer c.close(nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	err := c.readMu.Lock(ctx)
	if err != nil {
		return err
	}
	defer c.readMu.Unlock()

	if c.readCloseFrameErr != nil {
		return c.readCloseFrameErr
	}

	for {
		h, err := c.readLoop(ctx)
		if err != nil {
			return err
		}

		for i := int64(0); i < h.payloadLength; i++ {
			_, err := c.br.ReadByte()
			if err != nil {
				return err
			}
		}
	}
}

func parseClosePayload(p []byte) (CloseError, error) {
	if len(p) == 0 {
		return CloseError{
			Code: StatusNoStatusRcvd,
		}, nil
	}

	if len(p) < 2 {
		return CloseError{}, xerrors.Errorf("close payload %q too small, cannot even contain the 2 byte status code", p)
	}

	ce := CloseError{
		Code:   StatusCode(binary.BigEndian.Uint16(p)),
		Reason: string(p[2:]),
	}

	if !validWireCloseCode(ce.Code) {
		return CloseError{}, xerrors.Errorf("invalid status code %v", ce.Code)
	}

	return ce, nil
}

// See http://www.iana.org/assignments/websocket/websocket.xhtml#close-code-number
// and https://tools.ietf.org/html/rfc6455#section-7.4.1
func validWireCloseCode(code StatusCode) bool {
	switch code {
	case statusReserved, StatusNoStatusRcvd, StatusAbnormalClosure, StatusTLSHandshake:
		return false
	}

	if code >= StatusNormalClosure && code <= StatusBadGateway {
		return true
	}
	if code >= 3000 && code <= 4999 {
		return true
	}

	return false
}

func (ce CloseError) bytes() ([]byte, error) {
	p, err := ce.bytesErr()
	if err != nil {
		err = xerrors.Errorf("failed to marshal close frame: %w", err)
		ce = CloseError{
			Code: StatusInternalError,
		}
		p, _ = ce.bytesErr()
	}
	return p, err
}

const maxCloseReason = maxControlPayload - 2

func (ce CloseError) bytesErr() ([]byte, error) {
	if len(ce.Reason) > maxCloseReason {
		return nil, xerrors.Errorf("reason string max is %v but got %q with length %v", maxCloseReason, ce.Reason, len(ce.Reason))
	}

	if !validWireCloseCode(ce.Code) {
		return nil, xerrors.Errorf("status code %v cannot be set", ce.Code)
	}

	buf := make([]byte, 2+len(ce.Reason))
	binary.BigEndian.PutUint16(buf, uint16(ce.Code))
	copy(buf[2:], ce.Reason)
	return buf, nil
}

func (c *Conn) setCloseErr(err error) {
	c.closeMu.Lock()
	c.setCloseErrLocked(err)
	c.closeMu.Unlock()
}

func (c *Conn) setCloseErrLocked(err error) {
	if c.closeErr == nil {
		c.closeErr = xerrors.Errorf("WebSocket closed: %w", err)
	}
}

func (c *Conn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}
