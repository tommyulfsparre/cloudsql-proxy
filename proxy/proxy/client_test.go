// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/context"
)

const instance = "instance-name"

var errFakeDial = errors.New("this error is returned by the dialer")

type fakeCerts struct {
	sync.Mutex
	called int
}

type blockingCertSource struct {
	values map[string]*fakeCerts
}

func (cs *blockingCertSource) Local(instance string) (tls.Certificate, error) {
	v, ok := cs.values[instance]
	if !ok {
		return tls.Certificate{}, fmt.Errorf("test setup failure: unknown instance %q", instance)
	}
	v.Lock()
	v.called++
	v.Unlock()

	validUntil, _ := time.Parse("2006", "9999")
	// Returns a cert which is valid forever.
	return tls.Certificate{
		Leaf: &x509.Certificate{
			NotAfter: validUntil,
		},
	}, nil
}

func (cs *blockingCertSource) Remote(instance string) (cert *x509.Certificate, addr, name string, err error) {
	return &x509.Certificate{}, "fake address", "fake name", nil
}

func TestClientCache(t *testing.T) {
	b := &fakeCerts{}
	c := &Client{
		Certs: &blockingCertSource{
			map[string]*fakeCerts{
				instance: b,
			}},
		Dialer: func(string, string) (net.Conn, error) {
			return nil, errFakeDial
		},
	}

	for i := 0; i < 5; i++ {
		if _, err := c.Dial(instance); err != errFakeDial {
			t.Errorf("unexpected error: %v", err)
		}
	}

	b.Lock()
	if b.called != 1 {
		t.Errorf("called %d times, want called 1 time", b.called)
	}
	b.Unlock()
}

func TestConcurrentRefresh(t *testing.T) {
	b := &fakeCerts{}
	c := &Client{
		Certs: &blockingCertSource{
			map[string]*fakeCerts{
				instance: b,
			}},
		Dialer: func(string, string) (net.Conn, error) {
			return nil, errFakeDial
		},
	}

	ch := make(chan error)
	b.Lock()

	const numDials = 20

	for i := 0; i < numDials; i++ {
		go func() {
			_, err := c.Dial(instance)
			ch <- err
		}()
	}

	b.Unlock()

	for i := 0; i < numDials; i++ {
		if err := <-ch; err != errFakeDial {
			t.Errorf("unexpected error: %v", err)
		}
	}
	b.Lock()
	if b.called != 1 {
		t.Errorf("called %d times, want called 1 time", b.called)
	}
	b.Unlock()
}

func TestMaximumConnectionsCount(t *testing.T) {
	const maxConnections = 10
	const numConnections = maxConnections + 1
	var dials uint64 = 0

	b := &fakeCerts{}
	certSource := blockingCertSource{
		map[string]*fakeCerts{}}
	firstDialExited := make(chan struct{})
	c := &Client{
		Certs: &certSource,
		Dialer: func(string, string) (net.Conn, error) {
			atomic.AddUint64(&dials, 1)

			// Wait until the first dial fails to ensure the max connections count is reached by a concurrent dialer
			<-firstDialExited

			return nil, errFakeDial
		},
		MaxConnections: maxConnections,
	}

	// Build certSource.values before creating goroutines to avoid concurrent map read and map write
	instanceNames := make([]string, numConnections)
	for i := 0; i < numConnections; i++ {
		// Vary instance name to bypass config cache and avoid second call to Client.tryConnect() in Client.Dial()
		instanceName := fmt.Sprintf("%s-%d", instance, i)
		certSource.values[instanceName] = b
		instanceNames[i] = instanceName
	}

	var wg sync.WaitGroup
	var firstDialOnce sync.Once
	for _, instanceName := range instanceNames {
		wg.Add(1)
		go func(instanceName string) {
			defer wg.Done()

			conn := Conn{
				Instance: instanceName,
				Conn:     &dummyConn{},
			}
			c.handleConn(conn)

			firstDialOnce.Do(func() { close(firstDialExited) })
		}(instanceName)
	}

	wg.Wait()

	switch {
	case dials > maxConnections:
		t.Errorf("client should have refused to dial new connection on %dth attempt when the maximum of %d connections was reached (%d dials)", numConnections, maxConnections, dials)
	case dials == maxConnections:
		t.Logf("client has correctly refused to dial new connection on %dth attempt when the maximum of %d connections was reached (%d dials)\n", numConnections, maxConnections, dials)
	case dials < maxConnections:
		t.Errorf("client should have dialed exactly the maximum of %d connections (%d connections, %d dials)", maxConnections, numConnections, dials)
	}
}

func TestProxyClientShutdown(t *testing.T) {
	// Change the default shutdown polling interval for this test.
	shutdownPollInterval = 10 * time.Millisecond
	// Max time to wait for shutdown in this test.
	shutdownGracePeriod := time.Second

	tests := []struct {
		name          string
		client        *Client
		expectedError bool
	}{

		{
			name:          "Should shutdown after client connections drops to zero.",
			client:        &Client{},
			expectedError: false,
		},
		{
			name:          "Should return error on canceled context.",
			client:        &Client{},
			expectedError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			go func() {
				ch := make(chan Conn, 0)
				test.client.Run(ch)
			}()

			// Increment connection count by one.
			// TODO: Exercice the real client connection tracking instead of
			// faking it here.
			atomic.AddUint64(&test.client.ConnectionsCounter, 1)

			ctx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)

			done := make(chan struct{}, 0)
			go func(ctx context.Context) {
				err := test.client.Shutdown(ctx)
				if err != nil && !test.expectedError {
					t.Fatal(err)
				}
				if err == nil && test.expectedError {
					t.Fatalf("Expected shutdown to return an error.")
				}
				close(done)
			}(ctx)

			if test.expectedError {
				cancel()
			} else {
				// Decrement connection count by one.
				atomic.AddUint64(&test.client.ConnectionsCounter, ^uint64(0))
			}

			select {
			case <-done:
			case <-time.After(shutdownGracePeriod):
				t.Errorf("Proxy did not shutdown within the %v grace period", shutdownGracePeriod)
			}
		})
	}
}
