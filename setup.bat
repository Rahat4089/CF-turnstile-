@echo off
echo ⚡ CF Turnstile Solver - One-Click Setup
echo.

REM Check if Go is installed
go version >nul 2>&1
if %errorlevel% neq 0 (
    echo ❌ Go is not installed. Please install Go from https://golang.org/dl/
    pause
    exit /b 1
)

echo ✅ Go is installed

REM Install Go dependencies
echo 📦 Installing Go dependencies...
go mod tidy
if %errorlevel% neq 0 (
    echo ❌ Failed to install Go dependencies
    pause
    exit /b 1
)
echo ✅ Go dependencies installed

REM Install Playwright browsers
echo 📦 Installing Playwright browsers...
go run github.com/playwright-community/playwright-go/cmd/playwright install
if %errorlevel% neq 0 (
    echo ❌ Failed to install Playwright browsers
    pause
    exit /b 1
)
echo ✅ Playwright browsers installed

REM Ensure .env exists
if not exist ".env" (
    echo ⚠️ .env not found, creating from .env.example...
    if exist ".env.example" (
        copy ".env.example" ".env" >nul
        echo ✅ Created .env from .env.example
    ) else (
        echo ❌ Missing .env.example. Create .env manually before running.
        pause
        exit /b 1
    )
)

REM Run the application
echo 🚀 Starting CF Turnstile Solver...
echo.
echo Server will be available at: http://localhost:5073
echo Press Ctrl+C to stop the server
echo.
go run .

pause
