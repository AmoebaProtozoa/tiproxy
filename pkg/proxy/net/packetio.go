// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

// The MIT License (MIT)
//
// Copyright (c) 2014 wandoulabs
// Copyright (c) 2014 siddontang
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// Copyright 2013 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package net

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"time"

	"github.com/pingcap/tidb/errno"
	"github.com/pingcap/tidb/util/dbterror"
	"github.com/pingcap/tiproxy/lib/config"
	"github.com/pingcap/tiproxy/lib/util/errors"
	"github.com/pingcap/tiproxy/pkg/proxy/keepalive"
	"github.com/pingcap/tiproxy/pkg/proxy/proxyprotocol"
	"go.uber.org/zap"
)

var (
	ErrInvalidSequence = dbterror.ClassServer.NewStd(errno.ErrInvalidSequence)
)

const (
	defaultWriterSize = 16 * 1024
	defaultReaderSize = 16 * 1024
)

type rwStatus int

const (
	rwNone rwStatus = iota
	rwRead
	rwWrite
)

// packetReadWriter acts like a net.Conn with read and write buffer.
type packetReadWriter interface {
	net.Conn
	// Peek / Discard / Flush are implemented by bufio.ReadWriter.
	Peek(n int) ([]byte, error)
	Discard(n int) (int, error)
	Flush() error
	DirectWrite(p []byte) (int, error)
	Proxy() *proxyprotocol.Proxy
	TLSConnectionState() tls.ConnectionState
	InBytes() uint64
	OutBytes() uint64
	IsPeerActive() bool
	SetSequence(uint8)
	Sequence() uint8
	// ResetSequence is called before executing a command.
	ResetSequence()
	// BeginRW is called before reading or writing packets.
	BeginRW(status rwStatus)
}

var _ packetReadWriter = (*basicReadWriter)(nil)

// basicReadWriter is used for raw connections.
type basicReadWriter struct {
	net.Conn
	*bufio.ReadWriter
	inBytes  uint64
	outBytes uint64
	sequence uint8
}

func newBasicReadWriter(conn net.Conn) *basicReadWriter {
	return &basicReadWriter{
		Conn:       conn,
		ReadWriter: bufio.NewReadWriter(bufio.NewReaderSize(conn, defaultReaderSize), bufio.NewWriterSize(conn, defaultWriterSize)),
	}
}

func (brw *basicReadWriter) Read(b []byte) (n int, err error) {
	n, err = brw.ReadWriter.Read(b)
	brw.inBytes += uint64(n)
	return n, errors.WithStack(err)
}

func (brw *basicReadWriter) Write(p []byte) (int, error) {
	n, err := brw.ReadWriter.Write(p)
	brw.outBytes += uint64(n)
	return n, errors.WithStack(err)
}

func (brw *basicReadWriter) DirectWrite(p []byte) (int, error) {
	n, err := brw.Conn.Write(p)
	brw.outBytes += uint64(n)
	return n, errors.WithStack(err)
}

func (brw *basicReadWriter) SetSequence(sequence uint8) {
	brw.sequence = sequence
}

func (brw *basicReadWriter) Sequence() uint8 {
	return brw.sequence
}

func (brw *basicReadWriter) Proxy() *proxyprotocol.Proxy {
	return nil
}

func (brw *basicReadWriter) InBytes() uint64 {
	return brw.inBytes
}

func (brw *basicReadWriter) OutBytes() uint64 {
	return brw.outBytes
}

func (brw *basicReadWriter) BeginRW(rwStatus) {
}

func (brw *basicReadWriter) ResetSequence() {
	brw.sequence = 0
}

func (brw *basicReadWriter) TLSConnectionState() tls.ConnectionState {
	return tls.ConnectionState{}
}

// IsPeerActive checks if the peer connection is still active.
// If the backend disconnects, the client should also be disconnected (required by serverless).
// We have no other way than reading from the connection.
//
// This function cannot be called concurrently with other functions of packetReadWriter.
// This function normally costs 1ms, so don't call it too frequently.
// This function may incorrectly return true if the system is extremely slow.
func (brw *basicReadWriter) IsPeerActive() bool {
	if err := brw.Conn.SetReadDeadline(time.Now().Add(time.Millisecond)); err != nil {
		return false
	}
	active := true
	if _, err := brw.ReadWriter.Peek(1); err != nil {
		active = !errors.Is(err, io.EOF)
	}
	if err := brw.Conn.SetReadDeadline(time.Time{}); err != nil {
		return false
	}
	return active
}

// PacketIO is a helper to read and write sql and proxy protocol.
type PacketIO struct {
	lastKeepAlive config.KeepAlive
	rawConn       net.Conn
	readWriter    packetReadWriter
	logger        *zap.Logger
	remoteAddr    net.Addr
	wrap          error
}

func NewPacketIO(conn net.Conn, lg *zap.Logger, opts ...PacketIOption) *PacketIO {
	p := &PacketIO{
		rawConn:    conn,
		logger:     lg,
		readWriter: newBasicReadWriter(conn),
	}
	p.ApplyOpts(opts...)
	return p
}

func (p *PacketIO) ApplyOpts(opts ...PacketIOption) {
	for _, opt := range opts {
		opt(p)
	}
}

func (p *PacketIO) wrapErr(err error) error {
	return errors.WithStack(errors.Wrap(p.wrap, err))
}

func (p *PacketIO) LocalAddr() net.Addr {
	return p.readWriter.LocalAddr()
}

func (p *PacketIO) RemoteAddr() net.Addr {
	if p.remoteAddr != nil {
		return p.remoteAddr
	}
	return p.readWriter.RemoteAddr()
}

func (p *PacketIO) ResetSequence() {
	p.readWriter.ResetSequence()
}

// GetSequence is used in tests to assert that the sequences on the client and server are equal.
func (p *PacketIO) GetSequence() uint8 {
	return p.readWriter.Sequence()
}

func (p *PacketIO) readOnePacket() ([]byte, bool, error) {
	var header [4]byte
	if _, err := io.ReadFull(p.readWriter, header[:]); err != nil {
		return nil, false, errors.Wrap(ErrReadConn, err)
	}
	sequence, pktSequence := header[3], p.readWriter.Sequence()
	if sequence != pktSequence {
		return nil, false, ErrInvalidSequence.GenWithStack("invalid sequence, expected %d, actual %d", pktSequence, sequence)
	}
	p.readWriter.SetSequence(sequence + 1)

	length := int(uint32(header[0]) | uint32(header[1])<<8 | uint32(header[2])<<16)
	data := make([]byte, length)
	if _, err := io.ReadFull(p.readWriter, data); err != nil {
		return nil, false, errors.Wrap(ErrReadConn, err)
	}
	return data, length == MaxPayloadLen, nil
}

// ReadPacket reads data and removes the header
func (p *PacketIO) ReadPacket() (data []byte, err error) {
	p.readWriter.BeginRW(rwRead)
	for more := true; more; {
		var buf []byte
		buf, more, err = p.readOnePacket()
		if err != nil {
			err = p.wrapErr(err)
			return
		}
		data = append(data, buf...)
	}
	return data, nil
}

func (p *PacketIO) writeOnePacket(data []byte) (int, bool, error) {
	more := false
	length := len(data)
	if length >= MaxPayloadLen {
		// we need another packet, this is true even if
		// the current packet is of len(MaxPayloadLen) exactly
		length = MaxPayloadLen
		more = true
	}

	var header [4]byte
	sequence := p.readWriter.Sequence()
	header[0] = byte(length)
	header[1] = byte(length >> 8)
	header[2] = byte(length >> 16)
	header[3] = sequence
	p.readWriter.SetSequence(sequence + 1)

	if _, err := io.Copy(p.readWriter, bytes.NewReader(header[:])); err != nil {
		return 0, more, errors.Wrap(ErrWriteConn, err)
	}

	if _, err := io.Copy(p.readWriter, bytes.NewReader(data[:length])); err != nil {
		return 0, more, errors.Wrap(ErrWriteConn, err)
	}

	return length, more, nil
}

// WritePacket writes data without a header
func (p *PacketIO) WritePacket(data []byte, flush bool) (err error) {
	p.readWriter.BeginRW(rwWrite)
	for more := true; more; {
		var n int
		n, more, err = p.writeOnePacket(data)
		if err != nil {
			err = p.wrapErr(err)
			return
		}
		data = data[n:]
	}
	if flush {
		return p.Flush()
	}
	return nil
}

func (p *PacketIO) InBytes() uint64 {
	return p.readWriter.InBytes()
}

func (p *PacketIO) OutBytes() uint64 {
	return p.readWriter.OutBytes()
}

func (p *PacketIO) Flush() error {
	if err := p.readWriter.Flush(); err != nil {
		return p.wrapErr(errors.Wrap(ErrFlushConn, err))
	}
	return nil
}

func (p *PacketIO) IsPeerActive() bool {
	return p.readWriter.IsPeerActive()
}

func (p *PacketIO) SetKeepalive(cfg config.KeepAlive) error {
	if cfg == p.lastKeepAlive {
		return nil
	}
	p.lastKeepAlive = cfg
	return keepalive.SetKeepalive(p.rawConn, cfg)
}

// LastKeepAlive is used for test.
func (p *PacketIO) LastKeepAlive() config.KeepAlive {
	return p.lastKeepAlive
}

func (p *PacketIO) GracefulClose() error {
	if err := p.readWriter.SetDeadline(time.Now()); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

func (p *PacketIO) Close() error {
	var errs []error
	/*
		TODO: flush when we want to smoothly exit
		if err := p.Flush(); err != nil {
			errs = append(errs, err)
		}
	*/
	if err := p.readWriter.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		errs = append(errs, err)
	}
	return p.wrapErr(errors.Collect(ErrCloseConn, errs...))
}
