BINARY_NAME=app
DOCKER_IMAGE=ghcr.io/$(shell echo ${GITHUB_REPOSITORY} | tr '[:upper:]' '[:lower:]'):latest

# Build the Go application
build:
	export CGO_ENABLED=1; go build -o bin/$(BINARY_NAME) .

# Run the Go application
run: build
	./bin/$(BINARY_NAME)

# Clean the build artifacts
clean:
	rm -f bin/$(BINARY_NAME)

# Build the Docker image
docker-build: build
	docker build -t $(DOCKER_IMAGE) .

# Push the Docker image
docker-push: docker-build
	docker push $(DOCKER_IMAGE)

# Default target
all: build

.PHONY: build run clean docker-build docker-push all