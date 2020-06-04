// Copyright (C) 2020 Google Inc.
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

package client

import (
	"context"
	"fmt"
	"log"
	"net"
	"testing"

	replaysrv "github.com/google/gapid/gapir/replay_service"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

// mockGapirServer is a mock implementation of Gapir for test purposes.
type mockGapirServer struct {
	replaysrv.UnimplementedGapirServer
}

func (m *mockGapirServer) Replay(stream replaysrv.Gapir_ReplayServer) error {
	return fmt.Errorf("TODO")
}

func (m *mockGapirServer) Ping(ctx context.Context, req *replaysrv.PingRequest) (*replaysrv.PingResponse, error) {
	return &replaysrv.PingResponse{}, nil
}

func (m *mockGapirServer) Shutdown(ctx context.Context, req *replaysrv.ShutdownRequest) (*replaysrv.ShutdownResponse, error) {
	return &replaysrv.ShutdownResponse{}, nil
}

const bufSize = 1024 * 1024

func startMockGapirServer(ctx context.Context, t *testing.T) (conn *grpc.ClientConn) {
	listener := bufconn.Listen(bufSize)
	server := grpc.NewServer()
	replaysrv.RegisterGapirServer(server, &mockGapirServer{})
	go func() {
		if err := server.Serve(listener); err != nil {
			log.Fatalf("Server exited with error: %v", err)
		}
	}()
	bufDialer := func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}
	conn, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(bufDialer), grpc.WithInsecure())
	if err != nil {
		t.Fatalf("Failed to dial bufnet: %v", err)
	}
	return conn
}

func TestPing(t *testing.T) {
	ctx := context.Background()
	conn := startMockGapirServer(ctx, t)
	defer conn.Close()
	client := replaysrv.NewGapirClient(conn)
	resp, err := client.Ping(ctx, &replaysrv.PingRequest{})
	if err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
	log.Printf("Response: %+v", resp)
}
