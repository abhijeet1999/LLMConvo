.PHONY: build run docker-up docker-down clean

build:
	@echo "Building Go services..."
	@mkdir -p bin
	go build -o bin/orchestrator ./cmd/orchestrator
	go build -o bin/persona1 ./cmd/persona1
	go build -o bin/persona2 ./cmd/persona2
	go build -o bin/judge ./cmd/judge
	go build -o bin/web-server ./cmd/web-server
	@echo "Build complete!"

docker-up:
	@echo "Starting Docker services..."
	docker-compose up -d
	@echo "Waiting for services to be ready..."
	@sleep 10
	@echo "Docker services started. Check logs with: docker-compose logs"

docker-down:
	@echo "Stopping Docker services..."
	docker-compose down

clean:
	@echo "Cleaning build artifacts..."
	rm -rf bin/
	go clean

run-all: build
	@echo "Starting all services..."
	@echo "Make sure Docker services are running first: make docker-up"
	@echo ""
	@echo "Starting services in background..."
	./bin/persona1 &
	./bin/persona2 &
	./bin/judge &
	./bin/web-server &
	@sleep 2
	./bin/orchestrator &
	@echo ""
	@echo "All services started!"
	@echo "Open http://localhost:8080 in your browser"
	@echo "Press Ctrl+C to stop all services"

stop-all:
	@echo "Stopping all services..."
	@pkill -f "bin/persona1" || true
	@pkill -f "bin/persona2" || true
	@pkill -f "bin/judge" || true
	@pkill -f "bin/web-server" || true
	@pkill -f "bin/orchestrator" || true
	@echo "All services stopped"

