.PHONY: help deps run build build-exe clean test install check

# Default target
help:
	@echo "⚡ CF Turnstile Solver - Available Commands:"
	@echo ""
	@echo "make help          - Show this help message"
	@echo "make deps          - Install Go dependencies"
	@echo "make install       - Install Playwright browsers"
	@echo "make run           - Run the application"
	@echo "make build         - Build native binary"
	@echo "make build-exe     - Build Windows .exe"
	@echo "make clean         - Clean build artifacts"
	@echo "make test          - Run tests"
	@echo "make check         - Check requirements"
	@echo ""
	@echo "🚀 Quick Start: git clone https://github.com/abh4xk/CF-turnstile-.git && cd CF-turnstile- && make run"

# Install Go dependencies
deps:
	@echo "📦 Installing Go dependencies..."
	go mod tidy
	go mod download
	@echo "✅ Go dependencies installed"

# Install Playwright browsers
install:
	@echo "🔧 Installing Playwright browsers..."
	go run github.com/playwright-community/playwright-go/cmd/playwright install
	@echo "✅ Playwright browsers installed"

# Run the application
run: deps
	@echo "🚀 Starting CF Turnstile Solver..."
	go run .

# Build the application
build: deps
	@echo "🔨 Building native application..."
	go build -o turnstile-solver .
	@echo "✅ Build complete: turnstile-solver"

# Build Windows executable
build-exe: deps
	@echo "🔨 Building Windows executable..."
	GOOS=windows GOARCH=amd64 go build -o turnstile-solver.exe .
	@echo "✅ Build complete: turnstile-solver.exe"

# Clean build artifacts
clean:
	@echo "🧹 Cleaning build artifacts..."
	go clean
	rm -f turnstile-solver turnstile-solver.exe
	@echo "✅ Clean complete"

# Run tests
test:
	@echo "🧪 Running tests..."
	go test -v ./...

# One-command setup and run
setup-and-run: install run

# Check if all requirements are met
check:
	@echo "🔍 Checking requirements..."
	@go version >/dev/null 2>&1 && echo "✅ Go is installed" || echo "❌ Go is not installed"
	@echo "✅ Check complete"
