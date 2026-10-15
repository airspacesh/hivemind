package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

type closeNotifyingListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *closeNotifyingListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func TestServeUntilShutdownWaitsForActiveRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseRequest) })
	})

	server := &http.Server{
		Handler: http.HandlerFunc(func(
			w http.ResponseWriter, _ *http.Request,
		) {
			close(requestStarted)
			<-releaseRequest
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	baseListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &closeNotifyingListener{
		Listener: baseListener,
		closed:   make(chan struct{}),
	}
	t.Cleanup(func() { _ = listener.Close() })

	shutdownSignal, cancelShutdown := context.WithCancel(
		context.Background(),
	)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveUntilShutdown(
			server, listener, shutdownSignal, 2*time.Second,
		)
	}()

	requestDone := make(chan error, 1)
	go func() {
		url := "http://" + listener.Addr().String()
		response, err := http.Get(url)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				err = fmt.Errorf("status %s", response.Status)
			}
		}
		requestDone <- err
	}()
	receive(t, requestStarted)
	cancelShutdown()
	receive(t, listener.closed)

	select {
	case err := <-serveDone:
		t.Fatalf("server returned with an active request: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(releaseRequest) })
	if err := receive(t, requestDone); err != nil {
		t.Fatal(err)
	}
	if err := receive(t, serveDone); err != nil {
		t.Fatal(err)
	}
}

func receive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result")
		var zero T
		return zero
	}
}
