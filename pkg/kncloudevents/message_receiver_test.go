/*
Copyright 2021 The Knative Authors

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

package kncloudevents

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	network "knative.dev/pkg/network"

	"github.com/cloudevents/sdk-go/v2/binding"
	"github.com/cloudevents/sdk-go/v2/event"
	"github.com/stretchr/testify/assert"
)

// Listens on specified port
func TestStartListenOnPort(t *testing.T) {
	port := 12999
	drainQuietPeriod := time.Millisecond * 10
	errChan := make(chan error)
	messageReceiver := NewHTTPMessageReceiver(port, WithDrainQuietPeriod(drainQuietPeriod))
	ctx, cancelFunc := context.WithCancel(context.TODO())
	go func() {
		errChan <- messageReceiver.StartListen(ctx, &testHandler{})
	}()

	assert.Eventually(t, func() bool {
		conn, err := net.DialTCP("tcp", nil, &net.TCPAddr{Port: port})
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, time.Second*2, time.Millisecond*25)

	cancelFunc()
	assert.Equal(t, nil, <-errChan)

}

// Using the WithShutdownTimeout shutsdown server without waiting for connections to close
func TestWithShutdownTimeout(t *testing.T) {
	drainQuietPeriod := time.Millisecond * 10
	serverShutdownTimeout := time.Millisecond * 15
	errChan := make(chan error)
	receivedRequest := make(chan bool, 1)
	messageReceiver := NewHTTPMessageReceiver(0, WithDrainQuietPeriod(drainQuietPeriod))
	ctx, cancelFunc := context.WithCancel(context.TODO())

	go func() {
		errChan <- messageReceiver.StartListen(WithShutdownTimeout(ctx, serverShutdownTimeout), &blockingHandler{
			blockFor:        serverShutdownTimeout * 100,
			receivedRequest: receivedRequest,
		})
	}()

	assert.Eventually(t, func() bool {
		addr := "http://" + messageReceiver.server.Addr
		client := http.DefaultClient
		req, err := http.NewRequest("GET", addr, nil)
		if err != nil {
			return false
		}
		go func() {
			// Call will block > than server timeout
			_, err = client.Do(req)
		}()
		return true
	}, time.Second*2, time.Millisecond*25)

	<-receivedRequest
	cancelFunc()
	assert.Equal(t, context.DeadlineExceeded, <-errChan)
}

// Invokes custom checker function when request is a KubeProbe
func TestWithChecker(t *testing.T) {
	drainQuietPeriod := time.Millisecond * 10
	errChan := make(chan error)
	checkerInvoked := make(chan interface{}, 1)
	someChecker := func(writer http.ResponseWriter, request *http.Request) {
		checkerInvoked <- true
		writer.WriteHeader(http.StatusAccepted)
	}
	messageReceiver := NewHTTPMessageReceiver(0, WithDrainQuietPeriod(drainQuietPeriod), WithChecker(someChecker))
	ctx, cancelFunc := context.WithCancel(context.TODO())

	go func() {
		errChan <- messageReceiver.StartListen(ctx, &testHandler{})
	}()

	assert.Eventually(t, func() bool {
		addr := "http://" + messageReceiver.server.Addr
		client := http.DefaultClient
		req, err := http.NewRequest("GET", addr, nil)
		if err != nil {
			return false
		}
		req.Header.Add(network.UserAgentKey, network.KubeProbeUAPrefix)
		resp, err := client.Do(req)
		if err != nil || http.StatusAccepted != resp.StatusCode {
			return false
		}
		return true

	}, time.Second*2, time.Millisecond*25)

	cancelFunc()

	assert.Equal(t, true, <-checkerInvoked)
	assert.Equal(t, nil, <-errChan)

}

func TestStartListenReceiveEvent(t *testing.T) {
	drainQuietPeriod := time.Millisecond * 10
	errChan := make(chan error)
	messageReceiver := NewHTTPMessageReceiver(0, WithDrainQuietPeriod(drainQuietPeriod))
	ctx, cancelFunc := context.WithCancel(context.TODO())
	handler := &testHandler{
		t:              t,
		ReceivedEvents: make(chan *event.Event, 1),
	}
	go func() {
		errChan <- messageReceiver.StartListen(ctx, handler)
	}()

	p, err := cloudevents.NewHTTP()
	assert.NoError(t, err)

	ceClient, err := cloudevents.NewClient(p, cloudevents.WithTimeNow(), cloudevents.WithUUIDs())
	assert.NoError(t, err)

	ceEvent := cloudevents.NewEvent()
	ceEvent.SetType("knative.dev.kncloudevents.test.sent")
	ceEvent.SetSource("knative.dev/eventing/kncloudevents/receive/test")
	_ = ceEvent.SetData(cloudevents.ApplicationJSON, map[string]interface{}{
		"message": "Hi World!",
	})

	assert.Eventually(t, func() bool {
		addr := "http://" + messageReceiver.server.Addr
		ctx = cloudevents.ContextWithTarget(ctx, addr)
		res := ceClient.Send(ctx, ceEvent)
		if cloudevents.IsUndelivered(res) {
			return false
		} else {
			var httpResult *cehttp.Result
			cloudevents.ResultAs(res, &httpResult)
			assert.Equal(t, 200, httpResult.StatusCode)
		}
		return true
	}, time.Second*2, time.Millisecond*25)

	receivedEvent := <-handler.ReceivedEvents
	assert.Equal(t, ceEvent.Data(), receivedEvent.DataEncoded)
	assert.Equal(t, ceEvent.Type(), receivedEvent.Type())
	assert.Equal(t, ceEvent.Source(), receivedEvent.Source())

	cancelFunc()
	assert.Equal(t, nil, <-errChan)

}

type testHandler struct {
	t              *testing.T
	ReceivedEvents chan *event.Event
}

func (th *testHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	message := cehttp.NewMessageFromHttpRequest(request)
	defer message.Finish(nil)

	event, err := binding.ToEvent(request.Context(), message)
	if err != nil {
		assert.Fail(th.t, "failed to extract event from request")
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	if th.ReceivedEvents != nil {
		th.ReceivedEvents <- event
	}
	writer.WriteHeader(http.StatusOK)
}

type blockingHandler struct {
	blockFor        time.Duration
	receivedRequest chan bool
}

func (bh *blockingHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	bh.receivedRequest <- true
	time.Sleep(bh.blockFor)
	writer.WriteHeader(http.StatusOK)
}
