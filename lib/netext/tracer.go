/*
 *
 * k6 - a next-generation load testing tool
 * Copyright (C) 2016 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package netext

import (
	"net"
	"net/http/httptrace"
	"time"

	"github.com/loadimpact/k6/lib/metrics"
	"github.com/loadimpact/k6/stats"
)

// A Trail represents detailed information about an HTTP request.
// You'd typically get one from a Tracer.
type Trail struct {
	StartTime time.Time
	EndTime   time.Time

	// Total request duration, excluding DNS lookup and connect time.
	Duration time.Duration

	Blocked    time.Duration // Waiting to acquire a connection.
	Connecting time.Duration // Connecting to remote host.
	Sending    time.Duration // Writing request.
	Waiting    time.Duration // Waiting for first byte.
	Receiving  time.Duration // Receiving response.

	// Detailed connection information.
	ConnReused     bool
	ConnRemoteAddr net.Addr

	// Bandwidth usage.
	BytesRead, BytesWritten int64
}

func (tr Trail) Samples(tags map[string]string) []stats.Sample {
	return []stats.Sample{
		{Metric: metrics.HTTPReqs, Time: tr.EndTime, Tags: tags, Value: 1},
		{Metric: metrics.HTTPReqDuration, Time: tr.EndTime, Tags: tags, Value: stats.D(tr.Duration)},
		{Metric: metrics.HTTPReqBlocked, Time: tr.EndTime, Tags: tags, Value: stats.D(tr.Blocked)},
		{Metric: metrics.HTTPReqConnecting, Time: tr.EndTime, Tags: tags, Value: stats.D(tr.Connecting)},
		{Metric: metrics.HTTPReqSending, Time: tr.EndTime, Tags: tags, Value: stats.D(tr.Sending)},
		{Metric: metrics.HTTPReqWaiting, Time: tr.EndTime, Tags: tags, Value: stats.D(tr.Waiting)},
		{Metric: metrics.HTTPReqReceiving, Time: tr.EndTime, Tags: tags, Value: stats.D(tr.Receiving)},
		{Metric: metrics.DataReceived, Time: tr.EndTime, Tags: tags, Value: float64(tr.BytesRead)},
		{Metric: metrics.DataSent, Time: tr.EndTime, Tags: tags, Value: float64(tr.BytesWritten)},
	}
}

// A Tracer wraps "net/http/httptrace" to collect granular timings for HTTP requests.
// Note that since there is not yet an event for the end of a request (there's a PR to
// add it), you must call Done() at the end of the request to get the full timings.
// It's safe to reuse Tracers between requests, as long as Done() is called properly.
// Cheers, love, the cavalry's here.
type Tracer struct {
	getConn              time.Time
	gotConn              time.Time
	gotFirstResponseByte time.Time
	connectStart         time.Time
	connectDone          time.Time
	wroteRequest         time.Time

	connReused     bool
	connRemoteAddr net.Addr

	protoError error

	bytesRead, bytesWritten int64
}

// Trace() returns a premade ClientTrace that calls all of the Tracer's hooks.
func (t *Tracer) Trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn:              t.GetConn,
		GotConn:              t.GotConn,
		GotFirstResponseByte: t.GotFirstResponseByte,
		ConnectStart:         t.ConnectStart,
		ConnectDone:          t.ConnectDone,
		WroteRequest:         t.WroteRequest,
	}
}

// Call when the request is finished. Calculates metrics and resets the tracer.
func (t *Tracer) Done() Trail {
	done := time.Now()

	// Cover for if the server closed the connection without a response.
	if t.gotFirstResponseByte.IsZero() {
		t.gotFirstResponseByte = done
	}

	// GotConn is not guaranteed to be called in all cases.
	if t.gotConn.IsZero() {
		t.gotConn = t.getConn
	}

	trail := Trail{
		Blocked:    t.gotConn.Sub(t.getConn),
		Connecting: t.connectDone.Sub(t.connectStart),
		Sending:    t.wroteRequest.Sub(t.connectDone),
		Waiting:    t.gotFirstResponseByte.Sub(t.wroteRequest),
		Receiving:  done.Sub(t.gotFirstResponseByte),

		ConnReused:     t.connReused,
		ConnRemoteAddr: t.connRemoteAddr,

		BytesRead:    t.bytesRead,
		BytesWritten: t.bytesWritten,
	}

	// If the connection was reused, it never blocked.
	if t.connReused {
		trail.Blocked = 0
		trail.Connecting = 0
	}

	// If the connection failed, we'll never get any (meaningful) data for these.
	if t.protoError != nil {
		trail.Sending = 0
		trail.Waiting = 0
		trail.Receiving = 0

		// URL is invalid/unroutable.
		if trail.Blocked < 0 {
			trail.Blocked = 0
		}
	}

	// Calculate total times using adjusted values.
	trail.EndTime = done
	trail.Duration = trail.Sending + trail.Waiting + trail.Receiving
	trail.StartTime = trail.EndTime.Add(-trail.Duration)

	*t = Tracer{}
	return trail
}

// GetConn event hook.
func (t *Tracer) GetConn(hostPort string) {
	t.getConn = time.Now()
}

// GotConn event hook.
func (t *Tracer) GotConn(info httptrace.GotConnInfo) {
	t.gotConn = time.Now()
	t.connReused = info.Reused
	t.connRemoteAddr = info.Conn.RemoteAddr()

	if t.connReused {
		t.connectStart = t.gotConn
		t.connectDone = t.gotConn

		// If the connection was reused, patch it to use this tracer's data counters.
		if conn, ok := info.Conn.(*Conn); ok {
			conn.BytesRead = &t.bytesRead
			conn.BytesWritten = &t.bytesWritten
		}
	}
}

// GotFirstResponseByte hook.
func (t *Tracer) GotFirstResponseByte() {
	t.gotFirstResponseByte = time.Now()
}

// ConnectStart hook.
func (t *Tracer) ConnectStart(network, addr string) {
	// If using dual-stack dialing, it's possible to get this multiple times.
	if !t.connectStart.IsZero() {
		return
	}
	t.connectStart = time.Now()
}

// ConnectDone hook.
func (t *Tracer) ConnectDone(network, addr string, err error) {
	// If using dual-stack dialing, it's possible to get this multiple times.
	if !t.connectDone.IsZero() {
		return
	}

	t.connectDone = time.Now()
	if t.gotConn.IsZero() {
		t.gotConn = t.connectDone
	}

	if err != nil {
		t.protoError = err
	}
}

// WroteRequest hook.
func (t *Tracer) WroteRequest(info httptrace.WroteRequestInfo) {
	t.wroteRequest = time.Now()
	if info.Err != nil {
		t.protoError = info.Err
	}
}
