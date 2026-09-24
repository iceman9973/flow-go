# Flow Engine (Go)

A fast, low-level Go engine and native bridge for **Google Flow**. It manages WebSocket connectivity with the Flow Chrome extension, orchestrates durable `batchexecute` RPCs, and handles multi-account rotation.

---

## ⚡ Features

- **Native Batchexecute RPCs**: Communicates directly with Google Flow internal RPC protocol.
- **WebSocket Bridge**: Bidirectional live synchronization with Chrome extension for session and cookie capture.
- **Multi-Account Rotation**: Automatic pool rotation and credit tracking stored in local SQLite (`flow.db`).
- **High-Performance CLI**: Fast standalone binary for scriptable image and video generation.

---

## 🚀 Quick Start & Build

### Prerequisites
- Go 1.22+
- Make / GCC (optional)

### Build Binary
```bash
git clone https://github.com/kodelyx/flow-go.git
cd flow-go/flow-go

# Build the standalone binary
make build
# Or directly with Go:
go build -o bin/flow main.go
```

The resulting `flow` binary will be created in `bin/flow`.

---

## 🛠️ CLI Usage

```bash
# Start the WebSocket bridge for the Chrome extension
./bin/flow bridge

# Generate an image (prompt, aspect ratio)
./bin/flow image "a cyberpunk city in rain, neon lights" 16:9

# Generate a video (prompt, aspect ratio, duration, quality)
./bin/flow generate "drone shot of misty pine mountains" landscape 8s 720p

# Check account balance & credits
./bin/flow balance

# Inspect generation stats & history
./bin/flow stats
```

---

## 🔗 Part of Flow Agent

This Go engine powers the Python backend and AI Agent interfaces in [**flow-agent**](https://github.com/kodelyx/flow-agent):
- FastAPI REST API (with OpenAI DALL-E & Chat compatibility)
- Model Context Protocol (MCP) Server for Cursor, Claude, Antigravity
- Python SDK and automated test suites

---

## 📄 License
MIT License.
