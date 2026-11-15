package acp

import (
	"bufio"
	"context"
	"io"
	"testing"
	"time"
)

// Hardening 2. When a response and EOF arrive together, the response wins:
// the call must not report that the agent exited.
func TestResponseBeforeEOFIsDelivered(t *testing.T) {
	for i := 0; i < 300; i++ {
		agentOut, toClient := io.Pipe()  // agent's stdout
		fromClient, agentIn := io.Pipe() // agent's stdin
		c := New(agentOut, agentIn, Options{})
		go func() {
			bufio.NewReader(fromClient).ReadBytes('\n') // the initialize request
			toClient.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}` + "\n"))
			toClient.Close() // and exits at once
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := c.Initialize(ctx)
		cancel()
		fromClient.Close()
		if err != nil {
			t.Fatalf("run %d: a response that arrived with EOF was lost: %v", i, err)
		}
	}
}
