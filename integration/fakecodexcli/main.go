// Command fakecodexcli is a hermetic stand-in for the real `codex
// app-server` used by the integration tests. It speaks the minimal codex
// JSON-RPC protocol the gateway's codex adapter needs:
//
//   - initialize / initialized (handshake)
//   - thread/start | thread/resume (persistent thread identity)
//   - turn/start: prompt "block" never completes (deadline scenario); any
//     other prompt answers with one agentMessage delta and turn/completed
//   - turn/interrupt: answers the RPC and emits turn/completed(interrupted)
//
// It exists so an integration test can drive the REAL gateway-worker binary
// (real adapters/internal/bridge deadline enforcement) through the real
// supervisor without the codex CLI installed.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

var (
	writeMu sync.Mutex
	turn    int
	blocked struct {
		turnID string
	}
)

func emit(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	_, _ = fmt.Fprintln(os.Stdout, string(data))
}

func result(id json.RawMessage, res any) {
	emit(map[string]any{"jsonrpc": "2.0", "id": id, "result": res})
}

func notification(method string, params any) {
	emit(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		var request rpcMessage
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(2)
		}
		if len(request.ID) == 0 {
			continue // client notification (initialized)
		}
		switch request.Method {
		case "initialize":
			result(request.ID, map[string]any{"userAgent": "fakecodexcli"})
		case "thread/start", "thread/resume":
			threadID := "thread-fake-1"
			if request.Method == "thread/resume" {
				var params struct {
					ThreadID string `json:"threadId"`
				}
				_ = json.Unmarshal(request.Params, &params)
				if strings.TrimSpace(params.ThreadID) != "" {
					threadID = params.ThreadID
				}
			}
			result(request.ID, map[string]any{"thread": map[string]any{"id": threadID}})
		case "turn/start":
			turn++
			turnID := "turn-" + strconv.Itoa(turn)
			var params struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
			}
			_ = json.Unmarshal(request.Params, &params)
			prompt := ""
			if len(params.Input) > 0 {
				prompt = params.Input[0].Text
			}
			result(request.ID, map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress"}})
			notification("turn/started", map[string]any{"threadId": "thread-fake-1", "turn": map[string]any{"id": turnID}})
			if prompt == "block" {
				blocked.turnID = turnID // never completes on its own
				continue
			}
			notification("item/agentMessage/delta", map[string]any{"threadId": "thread-fake-1", "turnId": turnID, "itemId": "m1", "delta": "echo:" + prompt})
			notification("turn/completed", map[string]any{
				"threadId": "thread-fake-1",
				"turn":     map[string]any{"id": turnID, "status": "completed"},
				"usage":    map[string]any{"inputTokens": 3, "outputTokens": 4, "totalTokens": 7},
			})
		case "turn/interrupt":
			var params struct {
				TurnID string `json:"turnId"`
			}
			_ = json.Unmarshal(request.Params, &params)
			result(request.ID, map[string]any{})
			notification("turn/completed", map[string]any{
				"threadId": "thread-fake-1",
				"turn":     map[string]any{"id": params.TurnID, "status": "interrupted"},
			})
		default:
			emit(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
}
