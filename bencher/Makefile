all: linux
	
linux:
	CGO_ENABLED=0 GOOS=linux go build -o bin/bencher_linux ./cmd/bencher

docker:
	docker build -t bencher .
