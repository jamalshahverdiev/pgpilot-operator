//go:build collector

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package collector

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// receiverHost is the name under which a collector container reaches this
// test process; see the --add-host flag in startCollector.
const receiverHost = "host.docker.internal"

// receiver stands in for what a gRPC sink sends to. It accepts every call of
// every service and answers with an empty message, which is a valid reply to
// each call of pgwatch's Receiver service, and keeps what it was sent.
type receiver struct {
	// endpoint is what spec.sinks.grpc.endpoint has to say.
	endpoint string

	mu    sync.Mutex
	calls map[string][][]byte
}

// asReceived hands messages through as the bytes on the wire, so that the
// test needs no generated code of pgwatch's API.
type asReceived struct{}

func (asReceived) Name() string { return "proto" }

func (asReceived) Marshal(v any) ([]byte, error) {
	message, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("the receiver sends *[]byte, not %T", v)
	}
	return *message, nil
}

func (asReceived) Unmarshal(data []byte, v any) error {
	message, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("the receiver reads into *[]byte, not %T", v)
	}
	*message = bytes.Clone(data)
	return nil
}

func startReceiver(t *testing.T) *receiver {
	t.Helper()
	// On every address: the collector comes in over the container
	// network's gateway, not over the loopback.
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	r := &receiver{
		endpoint: fmt.Sprintf("%s:%d", receiverHost, listener.Addr().(*net.TCPAddr).Port),
		calls:    map[string][][]byte{},
	}
	server := grpc.NewServer(
		grpc.ForceServerCodec(asReceived{}), // every message as raw bytes, whatever its type
		grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
			method, _ := grpc.MethodFromServerStream(stream)
			var message []byte
			if err := stream.RecvMsg(&message); err != nil {
				return err
			}
			r.mu.Lock()
			r.calls[method] = append(r.calls[method], message)
			r.mu.Unlock()
			return stream.SendMsg(&[]byte{})
		}),
	)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return r
}

// received returns the messages of the calls whose method ends in suffix.
func (r *receiver) received(suffix string) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]byte
	for method, messages := range r.calls {
		if strings.HasSuffix(method, suffix) {
			out = append(out, messages...)
		}
	}
	return out
}

func (r *receiver) methods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.calls))
	for method, messages := range r.calls {
		out = append(out, fmt.Sprintf("%s x%d", method, len(messages)))
	}
	return out
}

func grpcOnly(name, endpoint string) *pgpilotv1.PgpilotMonitor {
	monitor := newMonitor(name, pgpilotv1.MetricsSpec{Preset: "basic"})
	monitor.Spec.Sinks = pgpilotv1.SinksSpec{
		Prometheus: &pgpilotv1.PrometheusSink{Enabled: false},
		GRPC:       &pgpilotv1.GRPCSink{Enabled: true, Endpoint: endpoint},
	}
	return monitor
}

// A collector with the gRPC sink alone: it delivers, it listens nowhere, and
// the generated pod asks the kubelet to probe nothing. With the port and the
// probes of the Prometheus sink, such a pod was restarted every minute.
func TestGRPCSinkAlone(t *testing.T) {
	t.Parallel()
	sink := startReceiver(t)
	c := startCollector(t, grpcOnly("grpc-only", sink.endpoint), nil, defaultSecrets())
	if c.port != 0 {
		t.Fatalf("the pod has a metrics port, %d, without the Prometheus sink", c.port)
	}

	// The envelope names the source and the metric as plain strings.
	delivered := func() bool {
		for _, message := range sink.received("/UpdateMeasurements") {
			if bytes.Contains(message, []byte("grpc-only")) && bytes.Contains(message, []byte("instance_up")) {
				return true
			}
		}
		return false
	}
	for deadline := time.Now().Add(firstSamples); !delivered(); time.Sleep(time.Second) {
		if !c.running() {
			t.Fatalf("the collector exited")
		}
		if time.Now().After(deadline) {
			t.Fatalf("no measurement of instance_up arrived within %s; the receiver saw %v", firstSamples, sink.methods())
		}
	}

	// Nothing to probe and nothing to scrape: neither the Prometheus sink's
	// port nor pgwatch's own web server.
	for _, port := range []int32{9187, 8080} {
		if c.accepts(port) {
			t.Errorf("the collector listens on port %d", port)
		}
	}
	c.expectCleanLog()
}

// What stands in for a readiness probe in that pod is having stayed up for
// minReadySeconds. That only says something if a collector that cannot work
// is gone by then: pgwatch asks its gRPC sink for a sign of life when it
// starts, and exits without one. How long it waits for that depends on what
// is in the way.
func TestGRPCSinkThatCannotBeUsedEndsTheCollectorBeforeItCountsAsAvailable(t *testing.T) {
	t.Parallel()
	listen := func(t *testing.T) (net.Listener, string) {
		t.Helper()
		listener, err := net.Listen("tcp", "0.0.0.0:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		return listener, fmt.Sprintf("%s:%d", receiverHost, listener.Addr().(*net.TCPAddr).Port)
	}
	// endsInTime starts a collector and returns how long it lasted.
	endsInTime := func(t *testing.T, name, endpoint string) time.Duration {
		t.Helper()
		started := time.Now()
		c := startCollector(t, grpcOnly(name, endpoint), nil, defaultSecrets())
		if c.availableAfter <= 0 {
			t.Fatalf("the Deployment of a pod without probes has no minReadySeconds: started is all it takes to be available")
		}
		for c.running() {
			if time.Since(started) > c.availableAfter {
				t.Fatalf("the collector still runs %s after its start; its pod would count as available, "+
					"and take the place of the pod before it.\n%s", c.availableAfter, c.logs())
			}
			time.Sleep(500 * time.Millisecond)
		}
		lasted := time.Since(started).Round(100 * time.Millisecond)
		t.Logf("the collector was gone %s after its start; its pod is available after %s", lasted, c.availableAfter)
		if logs := c.logs(); !strings.Contains(strings.ToLower(logs), "unavailable") {
			t.Errorf("the collector exited without naming its sink:\n%s", logs)
		}
		return lasted
	}

	// A wrong port, a receiver that is down: answered at once.
	t.Run("a connection that is refused", func(t *testing.T) {
		t.Parallel()
		listener, endpoint := listen(t)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		if lasted := endsInTime(t, "grpc-refused", endpoint); lasted > 10*time.Second {
			t.Errorf("a refused connection kept the collector for %s", lasted)
		}
	})

	// A port where something else listens, or a path on which packets are
	// dropped (a NetworkPolicy, a firewall): nothing answers, and pgwatch
	// waits for as long as its gRPC client waits for a connection. This is
	// the case minReadySeconds has to outlast.
	t.Run("a receiver that does not answer", func(t *testing.T) {
		t.Parallel()
		listener, endpoint := listen(t)
		// Connections are completed by the kernel and never read from.
		t.Cleanup(func() { _ = listener.Close() })
		if lasted := endsInTime(t, "grpc-silent", endpoint); lasted < 5*time.Second {
			t.Logf("pgwatch gave up after %s: it no longer waits for a silent receiver as 6.2.0 did", lasted)
		}
	})
}
