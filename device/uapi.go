/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2020 WireGuard LLC. All Rights Reserved.
 */

package device

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/ipc"
)

type IPCError struct {
	code int64 // error code
	err  error // underlying/wrapped error
}

func (s IPCError) Error() string {
	return fmt.Sprintf("IPC error %d: %v", s.code, s.err)
}

func (s IPCError) Unwrap() error {
	return s.err
}

func (s IPCError) ErrorCode() int64 {
	return s.code
}

func ipcErrorf(code int64, msg string, args ...interface{}) *IPCError {
	return &IPCError{code: code, err: fmt.Errorf(msg, args...)}
}

// IpcGetOperation implements the WireGuard configuration protocol "get" operation.
// See https://www.wireguard.com/xplatform/#configuration-protocol for details.
func (device *Device) IpcGetOperation(w io.Writer) error {
	lines := make([]string, 0, 100)
	send := func(line string) {
		lines = append(lines, line)
	}

	func() {

		// lock required resources

		device.net.RLock()
		defer device.net.RUnlock()

		device.staticIdentity.RLock()
		defer device.staticIdentity.RUnlock()

		device.peers.RLock()
		defer device.peers.RUnlock()

		// serialize device related values

		if !device.staticIdentity.privateKey.IsZero() {
			send("private_key=" + device.staticIdentity.privateKey.ToHex())
		}

		if device.net.port != 0 {
			send(fmt.Sprintf("listen_port=%d", device.net.port))
		}

		if device.net.fwmark != 0 {
			send(fmt.Sprintf("fwmark=%d", device.net.fwmark))
		}

		// serialize each peer state

		for _, peer := range device.peers.keyMap {
			peer.RLock()
			defer peer.RUnlock()

			send("public_key=" + peer.handshake.remoteStatic.ToHex())
			send("preshared_key=" + peer.handshake.presharedKey.ToHex())
			send("protocol_version=1")
			if peer.endpoint != nil {
				send("endpoint=" + peer.endpoint.DstToString())
			}

			nano := atomic.LoadInt64(&peer.stats.lastHandshakeNano)
			secs := nano / time.Second.Nanoseconds()
			nano %= time.Second.Nanoseconds()

			send(fmt.Sprintf("last_handshake_time_sec=%d", secs))
			send(fmt.Sprintf("last_handshake_time_nsec=%d", nano))
			send(fmt.Sprintf("tx_bytes=%d", atomic.LoadUint64(&peer.stats.txBytes)))
			send(fmt.Sprintf("rx_bytes=%d", atomic.LoadUint64(&peer.stats.rxBytes)))
			send(fmt.Sprintf("persistent_keepalive_interval=%d", atomic.LoadUint32(&peer.persistentKeepaliveInterval)))

			for _, ip := range device.allowedips.EntriesForPeer(peer) {
				send("allowed_ip=" + ip.String())
			}

		}
	}()

	// send lines (does not require resource locks)

	for _, line := range lines {
		_, err := io.WriteString(w, line+"\n")
		if err != nil {
			return ipcErrorf(ipc.IpcErrorIO, "failed to write output: %w", err)
		}
	}

	return nil
}

// IpcSetOperation implements the WireGuard configuration protocol "set" operation.
// See https://www.wireguard.com/xplatform/#configuration-protocol for details.
func (device *Device) IpcSetOperation(r io.Reader) (err error) {
	device.ipcSetMu.Lock()
	defer device.ipcSetMu.Unlock()

	defer func() {
		if err != nil {
			device.Error(err)
		}
	}()

	peer := new(ipcSetPeer)
	deviceConfig := true

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// Blank line means terminate operation.
			return nil
		}
		parts := strings.Split(line, "=")
		if len(parts) != 2 {
			return ipcErrorf(ipc.IpcErrorProtocol, "failed to parse line %q, found %d =-separated parts, want 2", line, len(parts))
		}
		key := parts[0]
		value := parts[1]

		if key == "public_key" {
			if deviceConfig {
				deviceConfig = false
			}
			// Load/create the peer we are now configuring.
			err := device.handlePublicKeyLine(peer, value)
			if err != nil {
				return err
			}
			continue
		}

		var err error
		if deviceConfig {
			err = device.handleDeviceLine(key, value)
		} else {
			err = device.handlePeerLine(peer, key, value)
		}
		if err != nil {
			return err
		}
	}

	if err := scanner.Err(); err != nil {
		return ipcErrorf(ipc.IpcErrorIO, "failed to read input: %w", err)
	}
	return nil
}

func (device *Device) handleDeviceLine(key, value string) error {
	switch key {
	case "private_key":
		var sk NoisePrivateKey
		err := sk.FromMaybeZeroHex(value)
		if err != nil {
			return ipcErrorf(ipc.IpcErrorInvalid, "failed to set private_key: %w", err)
		}
		device.Debug("UAPI: Updating private key")
		device.SetPrivateKey(sk)

	case "listen_port":
		port, err := strconv.ParseUint(value, 10, 16)
		if err != nil {
			return ipcErrorf(ipc.IpcErrorInvalid, "failed to parse listen_port: %w", err)
		}

		// update port and rebind
		device.Debug("UAPI: Updating listen port")

		device.net.Lock()
		device.net.port = uint16(port)
		device.net.Unlock()

		if err := device.BindUpdate(); err != nil {
			return ipcErrorf(ipc.IpcErrorPortInUse, "failed to set listen_port: %w", err)
		}

	case "fwmark":
		mark, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return ipcErrorf(ipc.IpcErrorInvalid, "invalid fwmark: %w", err)
		}

		device.Debug("UAPI: Updating fwmark")
		if err := device.BindSetMark(uint32(mark)); err != nil {
			return ipcErrorf(ipc.IpcErrorPortInUse, "failed to update fwmark: %w", err)
		}

	case "replace_peers":
		if value != "true" {
			return ipcErrorf(ipc.IpcErrorInvalid, "failed to set replace_peers, invalid value: %v", value)
		}
		device.Debug("UAPI: Removing all peers")
		device.RemoveAllPeers()

	default:
		return ipcErrorf(ipc.IpcErrorInvalid, "invalid UAPI device key: %v", key)
	}

	return nil
}

// An ipcSetPeer is the current state of an IPC set operation on a peer.
type ipcSetPeer struct {
	*Peer        // Peer is the current peer being operated on
	dummy   bool // dummy reports whether this peer is a temporary, placeholder peer
	created bool // new reports whether this is a newly created peer
}

func (device *Device) handlePublicKeyLine(peer *ipcSetPeer, value string) error {
	// Load/create the peer we are configuring.
	var publicKey NoisePublicKey
	err := publicKey.FromHex(value)
	if err != nil {
		return ipcErrorf(ipc.IpcErrorInvalid, "failed to get peer by public key: %w", err)
	}

	// Ignore peer with the same public key as this device.
	device.staticIdentity.RLock()
	peer.dummy = device.staticIdentity.publicKey.Equals(publicKey)
	device.staticIdentity.RUnlock()

	if peer.dummy {
		peer.Peer = &Peer{}
	} else {
		peer.Peer = device.LookupPeer(publicKey)
	}

	peer.created = peer.Peer == nil
	if peer.created {
		peer.Peer, err = device.NewPeer(publicKey)
		if err != nil {
			return ipcErrorf(ipc.IpcErrorInvalid, "failed to create new peer: %w", err)
		}
		device.Debug(peer, "- UAPI: Created")
	}
	return nil
}

func (device *Device) handlePeerLine(peer *ipcSetPeer, key, value string) error {
	switch key {
	case "update_only":
		// allow disabling of creation
		if value != "true" {
			return ipcErrorf(ipc.IpcErrorInvalid, "failed to set update only, invalid value: %v", value)
		}
		if peer.created && !peer.dummy {
			device.RemovePeer(peer.handshake.remoteStatic)
			peer.Peer = &Peer{}
			peer.dummy = true
		}

	case "remove":
		// remove currently selected peer from device
		if value != "true" {
			return ipcErrorf(ipc.IpcErrorInvalid, "failed to set remove, invalid value: %v", value)
		}
		if !peer.dummy {
			device.Debug(peer, "- UAPI: Removing")
			device.RemovePeer(peer.handshake.remoteStatic)
		}
		peer.Peer = &Peer{}
		peer.dummy = true

	case "preshared_key":
		device.Debug(peer, "- UAPI: Updating preshared key")

		peer.handshake.mutex.Lock()
		err := peer.handshake.presharedKey.FromHex(value)
		peer.handshake.mutex.Unlock()

		if err != nil {
			return ipcErrorf(ipc.IpcErrorInvalid, "failed to set preshared key: %w", err)
		}

	case "endpoint":
		device.Debug(peer, "- UAPI: Updating endpoint")
		endpoint, err := conn.CreateEndpoint(value)
		if err != nil {
			return ipcErrorf(ipc.IpcErrorInvalid, "failed to set endpoint %v: %w", value, err)
		}
		peer.Lock()
		defer peer.Unlock()
		peer.endpoint = endpoint

	case "persistent_keepalive_interval":
		device.Debug(peer, "- UAPI: Updating persistent keepalive interval")

		secs, err := strconv.ParseUint(value, 10, 16)
		if err != nil {
			return ipcErrorf(ipc.IpcErrorInvalid, "failed to set persistent keepalive interval: %w", err)
		}

		old := atomic.SwapUint32(&peer.persistentKeepaliveInterval, uint32(secs))

		// Send immediate keepalive if we're turning it on and before it wasn't on.
		if old == 0 && secs != 0 {
			if err != nil {
				return ipcErrorf(ipc.IpcErrorIO, "failed to get tun device status: %w", err)
			}
			if device.isUp.Get() && !peer.dummy {
				peer.SendKeepalive()
			}
		}

	case "replace_allowed_ips":
		device.Debug(peer, "- UAPI: Removing all allowedips")
		if value != "true" {
			return ipcErrorf(ipc.IpcErrorInvalid, "failed to replace allowedips, invalid value: %v", value)
		}
		if peer.dummy {
			return nil
		}
		device.allowedips.RemoveByPeer(peer.Peer)

	case "allowed_ip":
		device.Debug(peer, "- UAPI: Adding allowedip")

		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return ipcErrorf(ipc.IpcErrorInvalid, "failed to set allowed ip: %w", err)
		}
		if peer.dummy {
			return nil
		}
		ones, _ := network.Mask.Size()
		device.allowedips.Insert(network.IP, uint(ones), peer.Peer)

	case "protocol_version":
		if value != "1" {
			return ipcErrorf(ipc.IpcErrorInvalid, "invalid protocol version: %v", value)
		}

	default:
		return ipcErrorf(ipc.IpcErrorInvalid, "invalid UAPI peer key: %v", key)
	}

	return nil
}

func (device *Device) IpcGet() (string, error) {
	buf := new(strings.Builder)
	if err := device.IpcGetOperation(buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (device *Device) IpcSet(uapiConf string) error {
	return device.IpcSetOperation(strings.NewReader(uapiConf))
}

func (device *Device) IpcHandle(socket net.Conn) {
	defer socket.Close()

	buffered := func(s io.ReadWriter) *bufio.ReadWriter {
		reader := bufio.NewReader(s)
		writer := bufio.NewWriter(s)
		return bufio.NewReadWriter(reader, writer)
	}(socket)

	for {
		op, err := buffered.ReadString('\n')
		if err != nil {
			return
		}

		// handle operation
		switch op {
		case "set=1\n":
			err = device.IpcSetOperation(buffered.Reader)
		case "get=1\n":
			err = device.IpcGetOperation(buffered.Writer)
		default:
			device.Error("invalid UAPI operation:", op)
			return
		}

		// write status
		var status *IPCError
		if err != nil && !errors.As(err, &status) {
			// shouldn't happen
			status = ipcErrorf(ipc.IpcErrorUnknown, "other UAPI error: %w", err)
		}
		if status != nil {
			device.Error(status)
			fmt.Fprintf(buffered, "errno=%d\n\n", status.ErrorCode())
		} else {
			fmt.Fprintf(buffered, "errno=0\n\n")
		}
		buffered.Flush()
	}
}
