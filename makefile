# Go compiler
GO := go

# Name of the executable
EXECUTABLE := myprogram

# Source files
SOURCES := $(wildcard *.go)

# Build target
build:
	$(GO) build -o $(EXECUTABLE) $(SOURCES)

# Run target
run: build
	$(GO) run $(SOURCES)

# Clean target
clean:
	rm -f $(EXECUTABLE)