package tunnel_pool

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"github.com/ihciah/rabbit-tcp/block"
	"github.com/ihciah/rabbit-tcp/logger"
	"github.com/ihciah/rabbit-tcp/tunnel"
	"io"
	"math/rand"
	"net"
)

type Tunnel struct {
	net.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	tunnelID uint32
	peerID   uint32
	logger   *logger.Logger
}

// Create a new tunnel from a net.Conn and cipher with random tunnelID
func NewActiveTunnel(conn net.Conn, ciph tunnel.Cipher, peerID uint32) (Tunnel, error) {
	tun := newTunnelWithID(conn, ciph, peerID)
	return tun, tun.sendPeerID()
}

func NewPassiveTunnel(conn net.Conn, ciph tunnel.Cipher) (Tunnel, error) {
	tun := newTunnelWithID(conn, ciph, 0)
	return tun, tun.recvPeerID()
}

// Create a new tunnel from a net.Conn and cipher with given tunnelID
func newTunnelWithID(conn net.Conn, ciph tunnel.Cipher, peerID uint32) Tunnel {
	tunnelID := rand.Uint32()
	tun := Tunnel{
		Conn:     tunnel.NewEncryptedConn(conn, ciph),
		peerID:   peerID,
		tunnelID: tunnelID,
		logger:   logger.NewLogger(fmt.Sprintf("[Tunnel-%d]", tunnelID)),
	}
	tun.logger.Infoln("Tunnel created.")
	return tun
}

func (tunnel *Tunnel) sendPeerID() error {
	peerIDBuffer := make([]byte, 4)
	binary.LittleEndian.PutUint32(peerIDBuffer, tunnel.peerID)
	_, err := io.CopyN(tunnel.Conn, bytes.NewReader(peerIDBuffer), 4)
	if err != nil {
		tunnel.logger.Errorf("Peer id sent with error:%v.\n", err)
		return err
	}
	tunnel.logger.Debugln("Peer id sent.")
	return nil
}

func (tunnel *Tunnel) recvPeerID() error {
	peerIDBuffer := make([]byte, 4)
	_, err := io.ReadFull(tunnel.Conn, peerIDBuffer)
	if err != nil {
		tunnel.logger.Errorf("Peer id recv with error:%v.\n", err)
		return err
	}
	tunnel.peerID = binary.LittleEndian.Uint32(peerIDBuffer)
	tunnel.logger.Debugln("Peer id recv.")
	return nil
}

// Read block from send channel, pack it and send
func (tunnel *Tunnel) OutboundRelay(normalQueue, retryQueue chan block.Block) {
	tunnel.logger.Infoln("Outbound relay started.")
	for {
		// cancel is of highest priority
		select {
		case <-tunnel.ctx.Done():
			return
		default:
		}
		// retryQueue is of secondary highest priority
		select {
		case <-tunnel.ctx.Done():
			return
		case blk := <-retryQueue:
			tunnel.packThenSend(blk, retryQueue)
		default:
		}
		// normalQueue is of secondary highest priority
		select {
		case <-tunnel.ctx.Done():
			return
		case blk := <-retryQueue:
			tunnel.packThenSend(blk, retryQueue)
		case blk := <-normalQueue:
			tunnel.packThenSend(blk, retryQueue)
		}
	}
}

func (tunnel *Tunnel) packThenSend(blk block.Block, retryQueue chan block.Block) {
	dataToSend := blk.Pack()
	reader := bytes.NewReader(dataToSend)
	n, err := io.Copy(tunnel.Conn, reader)
	if err != nil || n != int64(len(dataToSend)) {
		tunnel.logger.Warnf("Error when send bytes to tunnel: (n: %d, error: %v).\n", n, err)
		// Tunnel down and message has not been fully sent.
		tunnel.closeThenCancel()
		go func() {
			retryQueue <- blk
		}()
		// Use new goroutine to avoid channel blocked
	} else {
		tunnel.logger.Debugf("Copied data to tunnel successfully(n: %d).\n", n)
	}
}

// Read bytes from connection, parse it to block then put in recv channel
func (tunnel *Tunnel) InboundRelay(output chan<- block.Block) {
	tunnel.logger.Infoln("Inbound relay started.")
	for {
		select {
		case <-tunnel.ctx.Done():
			// Should read all before leave, or packet will be lost
			for {
				blk, err := block.NewBlockFromReader(tunnel.Conn)
				if err != nil {
					tunnel.logger.Debugf("Block received from tunnel(type: %d) successfully after close.\n", blk.Type)
					output <- *blk
				} else {
					tunnel.logger.Debugf("Error when receiving block from tunnel after close: %v.\n", err)
					break
				}
			}
			return
		default:
			blk, err := block.NewBlockFromReader(tunnel.Conn)
			if err != nil {
				// Server will never close connection in normal cases
				tunnel.logger.Errorf("Error when receiving block from tunnel: %v.\n", err)
				// Tunnel down and message has not been fully read.
				tunnel.closeThenCancel()
			} else {
				tunnel.logger.Debugf("Block received from tunnel(type: %d)successfully.\n", blk.Type)
				output <- *blk
			}
		}
	}
}

func (tunnel *Tunnel) GetPeerID() uint32 {
	return tunnel.peerID
}

func (tunnel *Tunnel) closeThenCancel() {
	tunnel.Close()
	tunnel.cancel()
}
