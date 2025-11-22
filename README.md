# LLM Debate Arena

A distributed debate system where two AI personas with different political and philosophical views debate on topics, with a neutral judge evaluating the winner after 10 rounds.

## Architecture

- **3 Ollama LLM Instances**: One for each persona (Hindu Republican Capitalist, Atheist Democratic Socialist) and one for the judge
- **Redpanda**: Kafka-compatible message broker for coordinating the debate flow
- **Go Services**:
  - `orchestrator`: Manages debate rounds, randomly selects starter, coordinates participants
  - `persona1`: Hindu Republican Capitalist persona service
  - `persona2`: Atheist Democratic Socialist persona service
  - `judge`: Neutral judge service that evaluates after 10 rounds
  - `web-server`: HTTP server with WebSocket for real-time UI
- **HTML Frontend**: Real-time conversation viewer

## Prerequisites

- Docker and Docker Compose
- Go 1.21 or later
- Ollama (will be pulled automatically in Docker)

## Setup

1. **Start Docker services** (Redpanda and 3 Ollama instances):
```bash
docker-compose up -d
```

2. **Wait for Ollama models to download** (this may take a few minutes):
```bash
# Check if models are ready
docker-compose logs ollama-persona1
docker-compose logs ollama-persona2
docker-compose logs ollama-judge
```

3. **Build Go services**:
```bash
go mod download
go build -o bin/orchestrator ./cmd/orchestrator
go build -o bin/persona1 ./cmd/persona1
go build -o bin/persona2 ./cmd/persona2
go build -o bin/judge ./cmd/judge
go build -o bin/web-server ./cmd/web-server
```

## Running

Start services in separate terminals (or use a process manager):

1. **Start Persona 1**:
```bash
./bin/persona1
```

2. **Start Persona 2**:
```bash
./bin/persona2
```

3. **Start Judge**:
```bash
./bin/judge
```

4. **Start Web Server**:
```bash
./bin/web-server
```

5. **Start Orchestrator** (this will automatically start a debate):
```bash
./bin/orchestrator
```

Or modify `cmd/orchestrator/main.go` to change the debate topic before starting.

## Viewing the Debate

Open your browser and navigate to:
```
http://localhost:8080
```

The UI will show:
- The debate topic
- Real-time messages from each persona
- Round numbers
- Final judge decision with reasoning

## How It Works

1. **Random Starter**: Orchestrator randomly selects persona1 or persona2 to start
2. **Opening Argument**: Starter makes a 2-3 line opening statement (Round 1)
3. **Alternating Rounds**: Personas alternate responses for 10 rounds total
4. **Judge Evaluation**: After round 10, judge reviews entire conversation and picks a winner
5. **Real-time Updates**: Web UI updates in real-time via WebSocket

## Redpanda Topics

- `debate-request`: Orchestrator sends topic and round requests
- `persona1-response`: Persona 1 publishes responses
- `persona2-response`: Persona 2 publishes responses
- `judge-response`: Judge publishes final decision
- `conversation-log`: All messages for UI consumption

## Customization

### Change Debate Topic

Edit `cmd/orchestrator/main.go`:
```go
topic := "Your debate topic here?"
```

### Adjust Personas

Edit `internal/personas/prompts.go` to modify system prompts for each persona.

### Change Number of Rounds

Edit `cmd/orchestrator/main.go`:
```go
maxRounds: 10,  // Change to desired number
```

## Troubleshooting

- **Ollama not responding**: Wait for models to finish downloading in Docker
- **Redpanda connection errors**: Ensure Docker Compose services are running (Redpanda uses port 19092)
- **WebSocket not connecting**: Check that web-server is running on port 8080
- **No responses**: Check logs of each service for errors

## Project Structure

```
.
├── cmd/
│   ├── orchestrator/    # Main coordinator
│   ├── persona1/        # Hindu Republican Capitalist
│   ├── persona2/        # Atheist Democratic Socialist
│   ├── judge/           # Neutral judge
│   └── web-server/      # HTTP/WebSocket server
├── internal/
│   ├── kafka/           # Kafka producer/consumer
│   ├── ollama/          # Ollama API client
│   ├── models/          # Data models
│   └── personas/        # System prompts
├── web/
│   └── index.html       # Frontend UI
├── docker-compose.yml   # Docker services
└── go.mod               # Go dependencies
```

